package cleanup

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"maildrop/internal/config"
	"maildrop/internal/inbox"
	"maildrop/internal/ipguard"
	"maildrop/internal/mailparse"
	"maildrop/internal/store"
)

type recordingCleaner struct {
	calls   atomic.Int64
	blocker *ipguard.Blocker
}

func (c *recordingCleaner) CleanupExpired() {
	c.calls.Add(1)
	if c.blocker != nil {
		c.blocker.CleanupExpired()
	}
}

func TestRunOnceRetentionAccountingAndBlocker(t *testing.T) {
	for _, tc := range []struct {
		name          string
		seconds, days int
		retention     int64
	}{
		{"seconds overrides days", 10, 7, 10},
		{"days", 0, 2, 2 * 86400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.FromEnv(func(string) string { return "" })
			cfg.EmailRetentionSeconds = tc.seconds
			cfg.EmailRetentionDays = tc.days
			cfg.EmailRetentionTime = tc.retention
			db, err := store.OpenWithConfig(filepath.Join(t.TempDir(), "cleanup.db"), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			now := time.Unix(1800000000, 0)
			svc := inbox.New(cfg, db, func() time.Time { return now })
			ctx := context.Background()
			active, err := svc.CreateOrGetMailbox(ctx, "active@test.example", inbox.CreateMailboxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			expired, err := svc.CreateOrGetMailbox(ctx, "expired@test.example", inbox.CreateMailboxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for i, stamp := range []int64{now.Unix() - tc.retention - 1, now.Unix() - tc.retention, now.Unix()} {
				body := []string{"old", "boundary", "fresh"}[i]
				result, err := svc.Deliver(ctx, "", []string{active.Address}, &mailparse.Message{Body: body, Timestamp: stamp})
				if err != nil || result.Accepted != 1 {
					t.Fatalf("delivery=%+v err=%v", result, err)
				}
			}
			if _, err := svc.Deliver(ctx, "", []string{expired.Address}, &mailparse.Message{Body: "expired-box", Timestamp: now.Unix()}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.DB().Exec("UPDATE mailboxes SET expires_at = ? WHERE id = ?", now.Unix()-1, expired.ID); err != nil {
				t.Fatal(err)
			}
			blockClock := now.Add(-20 * time.Second)
			blocker := ipguard.NewBlockerWithClock(10, 1, 5, func() time.Time { return blockClock })
			blocker.RecordFailedAttempt("192.0.2.5")
			blockClock = now
			if _, exists := blocker.GetRemainingBlockTime("192.0.2.5"); !exists {
				t.Fatal("expired record missing before cleanup")
			}
			cleaner := &recordingCleaner{blocker: blocker}
			result, err := RunOnce(ctx, svc, cleaner)
			if err != nil || result.Mailboxes != 1 || result.Emails != 2 || result.Bytes != int64(len("oldexpired-box")) {
				t.Fatalf("cleanup=%+v err=%v", result, err)
			}
			if cleaner.calls.Load() != 1 {
				t.Fatal("Blocker.CleanupExpired not called")
			}
			if _, exists := blocker.GetRemainingBlockTime("192.0.2.5"); exists {
				t.Fatal("expired block record remains")
			}
			var used, total, count int64
			if err := db.DB().QueryRow(`SELECT storage_used, (SELECT COALESCE(SUM(size_bytes),0) FROM emails WHERE mailbox_id = ?),
 (SELECT COUNT(*) FROM emails WHERE mailbox_id = ?) FROM mailboxes WHERE id = ?`, active.ID, active.ID, active.ID).Scan(&used, &total, &count); err != nil {
				t.Fatal(err)
			}
			if count != 2 || used != total || used != int64(len("boundaryfresh")) {
				t.Fatalf("used=%d total=%d count=%d", used, total, count)
			}
			if info, err := svc.GetMailboxInfo(ctx, expired.Address); err != nil || info != nil {
				t.Fatalf("expired info=%+v err=%v", info, err)
			}
			result, err = RunOnce(ctx, svc, cleaner)
			if err != nil || result != (inbox.CleanupResult{}) {
				t.Fatalf("repeat=%+v err=%v", result, err)
			}
		})
	}
}

func TestCleanupInterval(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		want    time.Duration
	}{{0, time.Hour}, {1, 5 * time.Second}, {9, 5 * time.Second}, {11, 5 * time.Second}, {20, 10 * time.Second}, {120, 60 * time.Second}} {
		if got := Interval(&config.Config{EmailRetentionSeconds: tc.seconds}); got != tc.want {
			t.Errorf("seconds=%d interval=%s want=%s", tc.seconds, got, tc.want)
		}
	}
}

func TestRunCancellation(t *testing.T) {
	cfg := config.FromEnv(func(string) string { return "" })
	db, err := store.OpenWithConfig(filepath.Join(t.TempDir(), "cancel.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cleaner := &recordingCleaner{}
	done := make(chan struct{})
	go func() { Run(ctx, cfg, inbox.New(cfg, db, time.Now), cleaner); close(done) }()
	deadline := time.After(2 * time.Second)
	for cleaner.calls.Load() == 0 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("initial cleanup did not run")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
	if cleaner.calls.Load() != 1 {
		t.Fatalf("calls=%d", cleaner.calls.Load())
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	Run(ctx, cfg, nil, cleaner)
}

func TestRunOnceCleansBlockerOnDatabaseError(t *testing.T) {
	cfg := config.FromEnv(func(string) string { return "" })
	db, err := store.OpenWithConfig(filepath.Join(t.TempDir(), "error.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	svc := inbox.New(cfg, db, time.Now)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cleaner := &recordingCleaner{}
	if _, err := RunOnce(context.Background(), svc, cleaner); err == nil {
		t.Fatal("closed DB error swallowed")
	}
	if cleaner.calls.Load() != 1 {
		t.Fatal("blocker cleanup skipped after DB error")
	}
}
