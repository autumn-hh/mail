package outbound

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	"maildrop/internal/config"
	"maildrop/internal/inbox"
	"maildrop/internal/mailparse"
	"maildrop/internal/store"
)

type fixture struct {
	s       *Service
	db      *store.Store
	mailbox *store.Mailbox
	clock   *atomic.Int64
	cfg     *config.Config
}

func setup(t *testing.T) fixture {
	t.Helper()
	cfg := config.FromEnv(func(string) string { return "" })
	cfg.OutboundEnabled = true
	cfg.SMTPHostname = "mail.example.com"
	db, err := store.OpenWithConfig(filepath.Join(t.TempDir(), "mail.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mailbox, err := db.CreateMailbox("owner@example.com", store.CreateMailboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	clock := &atomic.Int64{}
	clock.Store(time.Date(2026, 11, 6, 15, 59, 0, 0, time.UTC).Unix())
	if _, err = db.DB().Exec(`UPDATE mailboxes SET expires_at=?`, clock.Load()+100*86400); err != nil {
		t.Fatal(err)
	}
	service := New(cfg, db, func() time.Time { return time.Unix(clock.Load(), 0) })
	service.sign = func(_ string, raw []byte) ([]byte, error) { return raw, nil }
	return fixture{service, db, mailbox, clock, cfg}
}
func (f fixture) enable(t *testing.T, daily, minute int) {
	t.Helper()
	if err := f.s.SetPolicy(context.Background(), f.mailbox.ID, Policy{Enabled: true, DailyLimit: daily, MinuteLimit: minute}, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
}
func (f fixture) submit(t *testing.T, key string, req Request) *Sent {
	t.Helper()
	sent, err := f.s.Submit(context.Background(), f.mailbox.ID, key, req)
	if err != nil {
		t.Fatal(err)
	}
	return sent
}
func simpleRequest() Request {
	return Request{To: []string{"a@recipient.test"}, Subject: "hello", Text: "body"}
}
func statusError(t *testing.T, err error, want int) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Status != want {
		t.Fatalf("error=%v want HTTP %d", err, want)
	}
}

type transportFunc func(context.Context, string, string, []byte) error

func (f transportFunc) Deliver(ctx context.Context, from, to string, raw []byte) error {
	return f(ctx, from, to, raw)
}
func (f fixture) process(t *testing.T) bool {
	t.Helper()
	done, err := f.s.process(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return done
}

func TestAuthorizationQuotaIdempotencyAndArchive(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	req := simpleRequest()
	_, err := f.s.Submit(ctx, f.mailbox.ID, "request", req)
	statusError(t, err, 403)
	f.enable(t, 3, 2)
	req.Cc = []string{"b@recipient.test"}
	req.Bcc = []string{"a@recipient.test"}
	sent := f.submit(t, "request", req)
	duplicate := f.submit(t, "request", req)
	if duplicate.ID != sent.ID {
		t.Fatal("idempotent request created a new message")
	}
	req.Subject = "changed"
	_, err = f.s.Submit(ctx, f.mailbox.ID, "request", req)
	statusError(t, err, 409)
	_, err = f.s.Submit(ctx, f.mailbox.ID, "other", simpleRequest())
	statusError(t, err, 429)
	policy, err := f.s.Policy(ctx, f.mailbox.ID)
	if err != nil || policy.DailyUsed != 2 || policy.MinuteUsed != 2 {
		t.Fatalf("policy=%+v %v", policy, err)
	}
	f.clock.Add(60)
	f.submit(t, "next-day", simpleRequest())
	policy, _ = f.s.Policy(ctx, f.mailbox.ID)
	if policy.DailyUsed != 1 || policy.MinuteUsed != 1 {
		t.Fatalf("Beijing midnight/rolling minute: %+v", policy)
	}
	if err = f.s.Delete(ctx, f.mailbox.ID, sent.ID); err == nil {
		t.Fatal("pending archive deletion accepted")
	}
	f.s.transport = transportFunc(func(context.Context, string, string, []byte) error { return nil })
	for f.process(t) {
	}
	if err = f.s.Delete(ctx, f.mailbox.ID, sent.ID); err != nil {
		t.Fatal(err)
	}
	original := simpleRequest()
	original.Cc = []string{"b@recipient.test"}
	original.Bcc = []string{"a@recipient.test"}
	_, err = f.s.Submit(ctx, f.mailbox.ID, "request", original)
	statusError(t, err, 410)
	f.clock.Add(int64(Retention/time.Second) + 1)
	if err = f.s.Clean(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Get(ctx, f.mailbox.ID, sent.ID)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	_, err = f.s.Submit(ctx, f.mailbox.ID, "request", original)
	statusError(t, err, 410)
	policy, _ = f.s.Policy(ctx, f.mailbox.ID)
	if policy.StorageUsed != 0 {
		t.Fatal("archive storage not released", policy)
	}
}

func TestConcurrentQuotaAndRequestReservations(t *testing.T) {
	f := setup(t)
	f.enable(t, 100, 10)
	var wg sync.WaitGroup
	var accepted atomic.Int64
	for i := range 30 {
		wg.Go(func() {
			_, err := f.s.Submit(context.Background(), f.mailbox.ID, fmt.Sprint(i), simpleRequest())
			if err == nil {
				accepted.Add(1)
			} else {
				var problem *Error
				if !errors.As(err, &problem) || problem.Status != 429 {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 10 {
		t.Fatal("quota oversubscription", accepted.Load())
	}
	f.clock.Add(61)
	ids := make(chan string, 12)
	for range 12 {
		wg.Go(func() {
			sent, err := f.s.Submit(context.Background(), f.mailbox.ID, "same", simpleRequest())
			if err != nil {
				t.Error(err)
				return
			}
			ids <- sent.ID
		})
	}
	wg.Wait()
	close(ids)
	id := ""
	for next := range ids {
		if id != "" && next != id {
			t.Fatal("duplicate queue entry")
		}
		id = next
	}
	p, _ := f.s.Policy(context.Background(), f.mailbox.ID)
	if p.MinuteUsed != 1 || p.DailyUsed != 1 {
		t.Fatalf("quota charged duplicate request %+v", p)
	}
}

func TestRecipientResultsRetriesAndRevocation(t *testing.T) {
	f := setup(t)
	f.enable(t, 100, 10)
	req := simpleRequest()
	req.Cc = []string{"b@recipient.test"}
	sent := f.submit(t, "two", req)
	calls := map[string]int{}
	f.s.transport = transportFunc(func(_ context.Context, _, to string, _ []byte) error {
		calls[to]++
		if strings.HasPrefix(to, "b@") && calls[to] == 1 {
			return &DeliveryError{Reason: "421 busy"}
		}
		return nil
	})
	f.process(t)
	f.process(t)
	f.clock.Add(61)
	f.process(t)
	if calls["a@recipient.test"] != 1 || calls["b@recipient.test"] != 2 {
		t.Fatal(calls)
	}
	got, _ := f.s.Get(context.Background(), f.mailbox.ID, sent.ID)
	if got.Status != "accepted" {
		t.Fatal(got)
	}
	next := f.submit(t, "uncertain", simpleRequest())
	f.s.transport = transportFunc(func(context.Context, string, string, []byte) error {
		return &DeliveryError{Uncertain: true, Reason: "lost DATA reply"}
	})
	f.process(t)
	got, _ = f.s.Get(context.Background(), f.mailbox.ID, next.ID)
	if !got.Recipients[0].Uncertain || got.Recipients[0].Status != "retry" {
		t.Fatal(got)
	}
	if err := f.s.SetPolicy(context.Background(), f.mailbox.ID, Policy{DailyLimit: 100, MinuteLimit: 10}, ""); err != nil {
		t.Fatal(err)
	}
	f.enable(t, 100, 10)
	f.clock.Add(3600)
	if f.process(t) {
		t.Fatal("revoked retry revived")
	}
	got, _ = f.s.Get(context.Background(), f.mailbox.ID, next.ID)
	if got.Status != "failed" {
		t.Fatal(got)
	}
}

func TestRevocationDuringFlightCannotReenableRetry(t *testing.T) {
	f := setup(t)
	f.enable(t, 100, 10)
	sent := f.submit(t, "revoke", simpleRequest())
	f.s.transport = transportFunc(func(context.Context, string, string, []byte) error {
		if err := f.s.SetPolicy(context.Background(), f.mailbox.ID, Policy{DailyLimit: 100, MinuteLimit: 10}, ""); err != nil {
			return err
		}
		f.enable(t, 100, 10)
		return &DeliveryError{Reason: "temporary failure"}
	})
	f.process(t)
	got, _ := f.s.Get(context.Background(), f.mailbox.ID, sent.ID)
	if got.Status != "failed" {
		t.Fatalf("in-flight retry survived revocation: %+v", got)
	}
}

func TestRestartRecoveryAndRetryDeadline(t *testing.T) {
	f := setup(t)
	f.enable(t, 100, 10)
	first := f.submit(t, "crashed", simpleRequest())
	second := f.submit(t, "accepted", simpleRequest())
	_, err := f.db.DB().Exec(`UPDATE outbound_recipients SET status='sending',attempts=1,lease_until=? WHERE message_id=?`, f.clock.Load()-1, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.db.DB().Exec(`UPDATE outbound_recipients SET status='accepted' WHERE message_id=?`, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	restarted := New(f.cfg, f.db, f.s.now)
	var calls int
	restarted.transport = transportFunc(func(context.Context, string, string, []byte) error { calls++; return nil })
	f.s = restarted
	f.process(t)
	if calls != 1 {
		t.Fatal(calls)
	}
	got, _ := f.s.Get(context.Background(), f.mailbox.ID, first.ID)
	if !got.Recipients[0].Uncertain || got.Status != "accepted" {
		t.Fatal(got)
	}
	f.s.sign = func(_ string, raw []byte) ([]byte, error) { return raw, nil }
	expired := f.submit(t, "expired", simpleRequest())
	f.clock.Add(86400)
	if f.process(t) {
		t.Fatal("delivered beyond 24-hour deadline")
	}
	got, _ = f.s.Get(context.Background(), f.mailbox.ID, expired.ID)
	if got.Status != "failed" {
		t.Fatal(got)
	}
}

func TestComposeBCCAttachmentsRepliesAndDKIM(t *testing.T) {
	f := setup(t)
	f.enable(t, 100, 10)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f.s.cfg.DKIMKeyDir = t.TempDir()
	if err = os.WriteFile(filepath.Join(f.s.cfg.DKIMKeyDir, "example.com.pem"), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	f.s.sign = f.s.signMessage
	raw := []byte("From: client@recipient.test\r\nMessage-ID: <original@recipient.test>\r\nReferences: <earlier@recipient.test>\r\nReply-To: replies@recipient.test\r\n\r\noriginal")
	original, err := mailparse.Parse(raw, f.s.now())
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := inbox.New(f.cfg, f.db, f.s.now).Deliver(context.Background(), "", []string{f.mailbox.Address}, original)
	if err != nil || delivery.Accepted != 1 {
		t.Fatal(delivery, err)
	}
	request := Request{To: []string{"a@recipient.test"}, Cc: []string{"b@recipient.test"}, Bcc: []string{"secret@recipient.test"}, Subject: strings.Repeat("中文回复", 20), Text: "你好\n世界", HTML: "<p>你好</p>", ReplyEmailID: original.ID, Attachments: []Upload{{Filename: "中文附件.bin", ContentType: "application/octet-stream", Content: []byte{0, 1, 255, 10}}}}
	sent := f.submit(t, "signed", request)
	raw, err = f.s.Raw(context.Background(), f.mailbox.ID, sent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("secret@recipient.test")) || bytes.Contains(raw, []byte("Bcc:")) {
		t.Fatal("BCC leaked into SMTP message")
	}
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if message.Header.Get("From") != "<owner@example.com>" || message.Header.Get("In-Reply-To") != "<original@recipient.test>" {
		t.Fatal(message.Header)
	}
	for _, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 998 {
			t.Fatal("oversized SMTP line", len(line))
		}
	}
	parsed, err := mailparse.Parse(raw, f.s.now())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Subject != request.Subject || len(parsed.Details.Attachments) != 1 || !bytes.Equal(parsed.Details.Attachments[0].Data, request.Attachments[0].Content) {
		t.Fatalf("MIME round trip %+v %v", parsed, err)
	}
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	signatures, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{LookupTXT: func(string) ([]string, error) {
		return []string{"v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(public)}, nil
	}})
	if err != nil || len(signatures) != 1 || signatures[0].Err != nil {
		t.Fatalf("DKIM verify %+v %v", signatures, err)
	}
	if sent.Details.Attachments[0].Size != 4 || sent.Details.Attachments[0].ID != "1" || len(sent.Details.Attachments[0].Content) != 0 {
		t.Fatal("archive metadata", sent)
	}
}

func TestArchiveLimitAndAttachmentValidation(t *testing.T) {
	f := setup(t)
	f.enable(t, 100, 10)
	sent := f.submit(t, "full", simpleRequest())
	if _, err := f.db.DB().Exec(`UPDATE outbound_messages SET size_bytes=? WHERE id=?`, ArchiveLimit, sent.ID); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.Submit(context.Background(), f.mailbox.ID, "full2", simpleRequest())
	statusError(t, err, 409)
	req := simpleRequest()
	req.Attachments = make([]Upload, 11)
	_, err = normalize(&req)
	statusError(t, err, 413)
	req = simpleRequest()
	req.Attachments = []Upload{{Filename: "large.bin", Content: make([]byte, AttachmentLimit+1)}}
	_, err = normalize(&req)
	statusError(t, err, 413)
	req = simpleRequest()
	req.Subject = "x\r\nBcc: evil@recipient.test"
	_, err = normalize(&req)
	statusError(t, err, 400)
	req = simpleRequest()
	req.To = []string{"x@recipient.test\r\nCc: evil@recipient.test"}
	_, err = normalize(&req)
	statusError(t, err, 400)
}

func TestBounceAssociationAndIsolation(t *testing.T) {
	f := setup(t)
	f.enable(t, 100, 10)
	sent := f.submit(t, "bounce", simpleRequest())
	f.s.transport = transportFunc(func(context.Context, string, string, []byte) error { return nil })
	f.process(t)
	var address string
	if err := f.db.DB().QueryRow(`SELECT bounce_address FROM outbound_messages WHERE id=?`, sent.ID).Scan(&address); err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: MAILER-DAEMON@recipient.test\r\nSubject: Delivery failed\r\nContent-Type: multipart/report; report-type=delivery-status; boundary=r\r\n\r\n--r\r\nContent-Type: text/plain\r\n\r\nFailure\r\n--r\r\nContent-Type: message/delivery-status\r\n\r\nReporting-MTA: dns; mx.recipient.test\r\n\r\nFinal-Recipient: rfc822; a@recipient.test\r\nAction: failed\r\nStatus: 5.1.1\r\nDiagnostic-Code: smtp; 550 unknown\r\n\r\n--r--\r\n")
	handled, err := f.s.ReceiveBounce(context.Background(), []string{address}, raw)
	if err != nil || !handled {
		t.Fatal(handled, err)
	}
	got, err := f.s.Get(context.Background(), f.mailbox.ID, sent.ID)
	if err != nil || got.Status != "bounced" || got.BounceCount != 1 {
		t.Fatalf("bounce %+v %v", got, err)
	}
	if _, err = f.s.ReportRaw(context.Background(), "another", got.Reports[0].ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("cross-mailbox report", err)
	}
	policy, _ := f.s.Policy(context.Background(), f.mailbox.ID)
	var size int64
	f.db.DB().QueryRow(`SELECT size_bytes FROM outbound_messages WHERE id=?`, sent.ID).Scan(&size)
	if policy.StorageUsed != size+int64(len(raw)) {
		t.Fatal("bounce archive accounting", policy)
	}
}

func TestQueueTransactionRollback(t *testing.T) {
	f := setup(t)
	f.enable(t, 100, 10)
	if _, err := f.db.DB().Exec(`CREATE TRIGGER reject_recipient BEFORE INSERT ON outbound_recipients BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Submit(context.Background(), f.mailbox.ID, "rollback", simpleRequest()); err == nil {
		t.Fatal("expected storage failure")
	}
	for _, table := range []string{"outbound_messages", "outbound_requests", "outbound_recipients"} {
		var count int
		if err := f.db.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal(table, count, err)
		}
	}
	if err := f.db.WithTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM mailboxes WHERE id=?`, f.mailbox.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestResultSaveFailureDoesNotResend(t *testing.T) {
	for _, outcome := range []struct {
		status string
		result error
	}{
		{"accepted", nil},
		{"failed", &DeliveryError{Permanent: true, Reason: "550 no such user"}},
	} {
		t.Run(outcome.status, func(t *testing.T) {
			f := setup(t)
			f.enable(t, 100, 10)
			sent := f.submit(t, "save-retry", simpleRequest())
			if _, err := f.db.DB().Exec(`CREATE TRIGGER reject_result BEFORE UPDATE ON outbound_recipients WHEN NEW.status IN ('accepted','failed') BEGIN SELECT RAISE(ABORT,'temporary write failure'); END`); err != nil {
				t.Fatal(err)
			}
			var count atomic.Int64
			called := make(chan struct{}, 1)
			f.s.transport = transportFunc(func(context.Context, string, string, []byte) error {
				count.Add(1)
				called <- struct{}{}
				return outcome.result
			})
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := f.s.process(ctx); done <- err }()
			<-called
			select {
			case err := <-done:
				t.Fatalf("delivery result must wait for storage recovery: %v", err)
			case <-time.After(150 * time.Millisecond):
			}
			// 使 lease 已过期；同一进程也只能等待记录成功，不重新投递。
			f.clock.Add(3600)
			if _, err := f.db.DB().Exec(`DROP TRIGGER reject_result`); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			record, err := f.s.Get(context.Background(), f.mailbox.ID, sent.ID)
			if err != nil || record.Status != outcome.status || count.Load() != 1 {
				t.Fatal(record, count.Load(), err)
			}
			if f.process(t) {
				t.Fatal("terminal recipient retried")
			}
		})
	}
}

func TestReopenRestoresQueueWithoutResendingAccepted(t *testing.T) {
	f := setup(t)
	f.enable(t, 100, 10)
	pending := f.submit(t, "restart-pending", simpleRequest())
	accepted := f.submit(t, "restart-accepted", simpleRequest())
	if _, err := f.db.DB().Exec(`UPDATE outbound_recipients SET status='sending',lease_until=? WHERE message_id=?`, f.clock.Load()+1800, pending.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.DB().Exec(`UPDATE outbound_recipients SET status='accepted' WHERE message_id=?`, accepted.ID); err != nil {
		t.Fatal(err)
	}
	var name, path string
	var sequence int
	if err := f.db.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.OpenWithConfig(path, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	service := New(f.cfg, reopened, f.s.now)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var calls int
	service.transport = transportFunc(func(context.Context, string, string, []byte) error { calls++; cancel(); return nil })
	service.Run(ctx)
	got, err := service.Get(context.Background(), f.mailbox.ID, pending.ID)
	if err != nil || calls != 1 || got.Status != "accepted" || !got.Recipients[0].Uncertain {
		t.Fatal(got, calls, err)
	}
	if done, err := service.process(context.Background()); err != nil || done {
		t.Fatal("accepted queue replayed", done, err)
	}
}

func TestRevokedLeaseDoesNotResumeAfterReauthorization(t *testing.T) {
	f := setup(t)
	f.enable(t, 100, 10)
	sent := f.submit(t, "crashed-after-revoke", simpleRequest())
	if _, err := f.s.claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetPolicy(context.Background(), f.mailbox.ID, Policy{DailyLimit: 100, MinuteLimit: 10}, ""); err != nil {
		t.Fatal(err)
	}
	f.enable(t, 100, 10)
	if _, err := f.db.DB().Exec(`UPDATE outbound_recipients SET lease_until=0`); err != nil {
		t.Fatal(err)
	}
	f.s = New(f.cfg, f.db, f.s.now)
	f.s.transport = transportFunc(func(context.Context, string, string, []byte) error {
		t.Error("revoked recipient sent after restart")
		return nil
	})
	if f.process(t) {
		t.Fatal("cancelled lease was claimed again")
	}
	got, err := f.s.Get(context.Background(), f.mailbox.ID, sent.ID)
	if err != nil || got.Status != "failed" || got.Recipients[0].Attempts != 1 {
		t.Fatal(got, err)
	}
}

func TestDisabledOrExpiredMailboxCannotSend(t *testing.T) {
	for _, change := range []string{"is_active=0", "expires_at=1"} {
		t.Run(change, func(t *testing.T) {
			f := setup(t)
			f.enable(t, 100, 10)
			sent := f.submit(t, "before-change", simpleRequest())
			if _, err := f.db.DB().Exec(`UPDATE mailboxes SET `+change+` WHERE id=?`, f.mailbox.ID); err != nil {
				t.Fatal(err)
			}
			_, err := f.s.Submit(context.Background(), f.mailbox.ID, "after-change", simpleRequest())
			statusError(t, err, 423)
			f.s.transport = transportFunc(func(context.Context, string, string, []byte) error { t.Error("inactive mailbox sent"); return nil })
			if f.process(t) {
				t.Fatal("inactive mailbox task was claimed")
			}
			got, err := f.s.Get(context.Background(), f.mailbox.ID, sent.ID)
			if err != nil || got.Status != "failed" {
				t.Fatal(got, err)
			}
		})
	}
}

func TestReplyCannotReferenceAnotherMailbox(t *testing.T) {
	f := setup(t)
	f.enable(t, 100, 10)
	other, err := f.db.CreateMailbox("other@example.com", store.CreateMailboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.DB().Exec(`UPDATE mailboxes SET expires_at=? WHERE id=?`, f.clock.Load()+86400, other.ID); err != nil {
		t.Fatal(err)
	}
	original, err := mailparse.Parse([]byte("From: customer@recipient.test\r\nMessage-ID: <private@recipient.test>\r\n\r\nprivate"), f.s.now())
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := inbox.New(f.cfg, f.db, f.s.now).Deliver(context.Background(), "", []string{other.Address}, original)
	if err != nil || delivery.Accepted != 1 {
		t.Fatal(delivery, err)
	}
	req := simpleRequest()
	req.ReplyEmailID = original.ID
	_, err = f.s.Submit(context.Background(), f.mailbox.ID, "cross-reply", req)
	statusError(t, err, 404)
}

func TestBounceBeforeResultSaveRemainsTerminal(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprint(uncertain), func(t *testing.T) {
			f := setup(t)
			f.enable(t, 100, 10)
			sent := f.submit(t, "early-report", simpleRequest())
			f.s.transport = transportFunc(func(ctx context.Context, from, to string, _ []byte) error {
				raw := []byte("From: MAILER-DAEMON@recipient.test\r\nContent-Type: message/delivery-status\r\n\r\nReporting-MTA: dns; mx.recipient.test\r\n\r\nFinal-Recipient: rfc822; a@RECIPIENT.TEST\r\nAction: failed\r\nStatus: 5.1.1\r\n")
				handled, err := f.s.ReceiveBounce(ctx, []string{from}, raw)
				if err != nil || !handled {
					t.Errorf("report %v %v", handled, err)
				}
				if uncertain {
					return &DeliveryError{Uncertain: true, Reason: "DATA confirmation lost"}
				}
				return nil
			})
			f.process(t)
			got, err := f.s.Get(context.Background(), f.mailbox.ID, sent.ID)
			if err != nil || got.Status != "bounced" || got.BounceCount != 1 {
				t.Fatal(got, err)
			}
			f.clock.Add(3600)
			if f.process(t) {
				t.Fatal("bounced recipient was retried")
			}
		})
	}
}

func TestMalformedReplyHeadersDoNotCreateOversizedLines(t *testing.T) {
	reply := &mailparse.Details{MessageID: strings.Repeat("<malformed@recipient.test> ", 100), References: strings.Repeat("<earlier@recipient.test> ", 100)}
	raw, err := compose("owner@example.com", "<new@example.com>", simpleRequest(), reply, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 998 {
			t.Fatal("malformed original generated oversized reply header", len(line))
		}
	}
	if bytes.Contains(raw, []byte("In-Reply-To:")) {
		t.Fatal("multi-value Message-ID copied into reply")
	}
}
