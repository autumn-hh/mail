package ipguard

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestBlockerWindowAndThreshold(t *testing.T) {
	for _, tc := range []struct {
		name    string
		advance time.Duration
		blocked bool
	}{
		{"within window", 30*time.Second - time.Nanosecond, true},
		{"exact window boundary", 30 * time.Second, false},
		{"outside window", 31 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			blocker := NewBlockerWithClock(60, 2, 30, clock.Now)
			if blocker.RecordFailedAttempt("192.0.2.1") {
				t.Fatal("first attempt blocked before threshold")
			}
			clock.Advance(tc.advance)
			if got := blocker.RecordFailedAttempt("192.0.2.1"); got != tc.blocked {
				t.Fatalf("RecordFailedAttempt = %v, want %v", got, tc.blocked)
			}
			if got := blocker.IsBlocked("192.0.2.1"); got != tc.blocked {
				t.Fatalf("IsBlocked = %v, want %v", got, tc.blocked)
			}
			if blocker.IsBlocked("192.0.2.2") {
				t.Fatal("unrelated IP was blocked")
			}
		})
	}
}

func TestBlockerExpiryAndUnblock(t *testing.T) {
	for _, action := range []string{"is blocked", "cleanup", "record", "manual"} {
		t.Run(action, func(t *testing.T) {
			clock := newFakeClock()
			b := NewBlockerWithClock(60, 2, 120, clock.Now)
			ip := "2001:db8::1"
			if remaining, ok := b.GetRemainingBlockTime(ip); remaining != 0 || ok {
				t.Fatalf("unknown IP remaining = (%d, %v)", remaining, ok)
			}
			b.RecordFailedAttempt(ip)
			if !b.RecordFailedAttempt(ip) {
				t.Fatal("threshold did not block")
			}
			clock.Advance(500 * time.Millisecond)
			if remaining, ok := b.GetRemainingBlockTime(ip); remaining != 59 || !ok {
				t.Fatalf("remaining = (%d, %v), want (59, true)", remaining, ok)
			}
			snapshot := b.GetBlockedIPs()
			if snapshot[ip] != 59 {
				t.Fatalf("snapshot = %v", snapshot)
			}
			snapshot[ip] = 999
			if b.GetBlockedIPs()[ip] != 59 {
				t.Fatal("snapshot changed blocker state")
			}
			if action == "manual" {
				b.UnblockIP(ip)
			} else {
				clock.Advance(59500 * time.Millisecond)
				if remaining, ok := b.GetRemainingBlockTime(ip); remaining != 0 || !ok {
					t.Fatalf("expired record = (%d, %v), want (0, true)", remaining, ok)
				}
				if len(b.GetBlockedIPs()) != 0 {
					t.Fatal("expired block appeared in snapshot")
				}
				switch action {
				case "is blocked":
					if b.IsBlocked(ip) {
						t.Fatal("block is active at expiry")
					}
				case "cleanup":
					b.CleanupExpired()
				case "record":
					if b.RecordFailedAttempt(ip) {
						t.Fatal("expired block reused stale attempts")
					}
					return
				}
			}
			if remaining, ok := b.GetRemainingBlockTime(ip); remaining != 0 || ok {
				t.Fatalf("cleared IP remaining = (%d, %v)", remaining, ok)
			}
			if b.RecordFailedAttempt(ip) {
				t.Fatal("attempt history was not reset")
			}
		})
	}
}

func TestBlockerRepeatedFailuresExtendBlock(t *testing.T) {
	clock := newFakeClock()
	b := NewBlockerWithClock(60, 2, 120, clock.Now)
	b.RecordFailedAttempt("ip")
	b.RecordFailedAttempt("ip")
	clock.Advance(10 * time.Second)
	if !b.RecordFailedAttempt("ip") {
		t.Fatal("Python returns true for a repeated threshold failure")
	}
	if remaining, _ := b.GetRemainingBlockTime("ip"); remaining != 60 {
		t.Fatalf("extended remaining = %d, want 60", remaining)
	}
}

func TestBlockerCleanupRemovesUnblockedHistory(t *testing.T) {
	clock := newFakeClock()
	b := NewBlockerWithClock(60, 3, 30, clock.Now)
	b.RecordFailedAttempt("old")
	clock.Advance(15 * time.Second)
	b.RecordFailedAttempt("fresh")
	clock.Advance(15 * time.Second)
	b.CleanupExpired()
	if _, ok := b.attempts["old"]; ok {
		t.Fatal("expired unblocked IP was retained")
	}
	if len(b.attempts["fresh"]) != 1 {
		t.Fatal("fresh history was removed")
	}
}

func TestBlockerConfig(t *testing.T) {
	ptr := func(v int) *int { return &v }
	for _, tc := range []struct {
		name   string
		update ConfigUpdate
		valid  bool
	}{
		{"empty", ConfigUpdate{}, true},
		{"minimum", ConfigUpdate{ptr(60), ptr(1), ptr(30)}, true},
		{"maximum", ConfigUpdate{ptr(86400), ptr(10), ptr(600)}, true},
		{"duration too low", ConfigUpdate{BlockDuration: ptr(59)}, false},
		{"duration too high", ConfigUpdate{BlockDuration: ptr(86401)}, false},
		{"attempts too low", ConfigUpdate{MaxAttempts: ptr(0)}, false},
		{"attempts too high", ConfigUpdate{MaxAttempts: ptr(11)}, false},
		{"window too low", ConfigUpdate{AttemptWindow: ptr(29)}, false},
		{"window too high", ConfigUpdate{AttemptWindow: ptr(601)}, false},
		{"invalid update is atomic", ConfigUpdate{ptr(120), ptr(11), ptr(90)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBlocker(300, 3, 60)
			before := b.GetConfig()
			err := b.UpdateConfig(tc.update)
			if (err == nil) != tc.valid {
				t.Fatalf("UpdateConfig error = %v, valid = %v", err, tc.valid)
			}
			if !tc.valid && b.GetConfig() != before {
				t.Fatal("invalid update partially changed configuration")
			}
			if tc.valid {
				want := before
				if tc.update.BlockDuration != nil {
					want.BlockDuration = *tc.update.BlockDuration
				}
				if tc.update.MaxAttempts != nil {
					want.MaxAttempts = *tc.update.MaxAttempts
				}
				if tc.update.AttemptWindow != nil {
					want.AttemptWindow = *tc.update.AttemptWindow
				}
				if b.GetConfig() != want {
					t.Fatalf("GetConfig = %+v, want %+v", b.GetConfig(), want)
				}
			}
		})
	}
	b := NewBlocker(300, 3, 60)
	encoded, err := json.Marshal(b.GetConfig())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]int
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]int{"block_duration": 300, "max_attempts": 3, "attempt_window": 60}) {
		t.Fatalf("JSON config = %s", encoded)
	}
	var update ConfigUpdate
	if err := json.Unmarshal([]byte(`{"max_attempts":2,"attempt_window":null}`), &update); err != nil {
		t.Fatal(err)
	}
	if err := b.UpdateConfig(update); err != nil {
		t.Fatal(err)
	}
	if got := b.GetConfig(); got != (Config{300, 2, 60}) {
		t.Fatalf("partial config = %+v", got)
	}
	if err := json.Unmarshal([]byte(`{"max_attempts":true}`), &update); err == nil {
		t.Fatal("boolean configuration accepted as integer")
	}
}

func TestBlockerConfigDoesNotChangeExistingExpiry(t *testing.T) {
	clock := newFakeClock()
	b := NewBlockerWithClock(60, 1, 30, clock.Now)
	b.RecordFailedAttempt("ip")
	value := 120
	if err := b.UpdateConfig(ConfigUpdate{BlockDuration: &value}); err != nil {
		t.Fatal(err)
	}
	if remaining, _ := b.GetRemainingBlockTime("ip"); remaining != 60 {
		t.Fatalf("existing expiry changed: %d", remaining)
	}
}

func TestBlockerDefaults(t *testing.T) {
	b := NewBlockerWithClock(0, -1, 0, nil)
	if got := b.GetConfig(); got != (Config{300, 3, 60}) {
		t.Fatalf("defaults = %+v", got)
	}
	if b.RecordFailedAttempt("ip") {
		t.Fatal("default threshold ignored")
	}
}

func TestBlockerConcurrentUse(t *testing.T) {
	clock := newFakeClock()
	b := NewBlockerWithClock(60, 3, 30, clock.Now)
	var workers sync.WaitGroup
	for worker := range 16 {
		workers.Go(func() {
			for iteration := range 100 {
				ip := fmt.Sprintf("192.0.2.%d", (worker+iteration)%8)
				b.RecordFailedAttempt(ip)
				b.IsBlocked(ip)
				b.GetRemainingBlockTime(ip)
				b.GetBlockedIPs()
				b.GetConfig()
				attempts := iteration%10 + 1
				if err := b.UpdateConfig(ConfigUpdate{MaxAttempts: &attempts}); err != nil {
					t.Error(err)
				}
				if iteration%5 == 0 {
					b.UnblockIP(ip)
				}
				b.CleanupExpired()
				clock.Advance(time.Millisecond)
			}
		})
	}
	workers.Wait()
}
