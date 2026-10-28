package inbox

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"maildrop/internal/config"
	"maildrop/internal/mailparse"
	"maildrop/internal/store"
)

type fixture struct {
	service *Service
	db      *store.Store
	clock   *atomic.Int64
	cfg     *config.Config
}

func setup(t *testing.T, adjust func(*config.Config)) fixture {
	t.Helper()
	cfg := config.FromEnv(func(string) string { return "" })
	if adjust != nil {
		adjust(cfg)
	}
	db, err := store.OpenWithConfig(filepath.Join(t.TempDir(), "inbox.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	clock := &atomic.Int64{}
	clock.Store(1800000000)
	return fixture{New(cfg, db, func() time.Time { return time.Unix(clock.Load(), 0) }), db, clock, cfg}
}

func (f fixture) mailbox(t *testing.T, address string, options CreateMailboxOptions) *Mailbox {
	t.Helper()
	m, err := f.service.CreateOrGetMailbox(context.Background(), address, options)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (f fixture) message(t *testing.T, raw string) *mailparse.Message {
	t.Helper()
	m, err := mailparse.Parse([]byte(raw), time.Unix(f.clock.Load(), 0))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func assertStorage(t *testing.T, f fixture, mailboxID string, count int64) int64 {
	t.Helper()
	var used, actual, found int64
	err := f.db.DB().QueryRow(`SELECT storage_used, (SELECT COALESCE(SUM(size_bytes), 0) FROM emails WHERE mailbox_id = ?),
 (SELECT COUNT(*) FROM emails WHERE mailbox_id = ?) FROM mailboxes WHERE id = ?`, mailboxID, mailboxID, mailboxID).
		Scan(&used, &actual, &found)
	if err != nil {
		t.Fatal(err)
	}
	if used != actual || found != count {
		t.Fatalf("storage=%d actual=%d count=%d, want count=%d", used, actual, found, count)
	}
	return used
}

func TestDeliverEnvelopeRouting(t *testing.T) {
	for _, tc := range []struct {
		name, headerTo string
		recipients     []string
		accepted       int
	}{
		{"bcc without To", "", []string{"one@test.example"}, 1},
		{"forwarded header", "external@other.example", []string{"one@test.example"}, 1},
		{"two mailboxes", "header@test.example", []string{"one@test.example", "two@test.example"}, 2},
		{"mixed outcomes", "header@test.example", []string{"missing@test.example", "one@test.example", "disabled@test.example", "expired@test.example", "two@test.example"}, 2},
		{"duplicate envelope", "header@test.example", []string{"one@test.example", "one@test.example"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, nil)
			one := f.mailbox(t, "one@test.example", CreateMailboxOptions{})
			two := f.mailbox(t, "two@test.example", CreateMailboxOptions{})
			header := f.mailbox(t, "header@test.example", CreateMailboxOptions{})
			disabled := f.mailbox(t, "disabled@test.example", CreateMailboxOptions{})
			expired := f.mailbox(t, "expired@test.example", CreateMailboxOptions{})
			if _, err := f.service.UpdateMailboxStatus(context.Background(), disabled.ID, false); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.DB().Exec("UPDATE mailboxes SET expires_at = ? WHERE id = ?", f.clock.Load()-1, expired.ID); err != nil {
				t.Fatal(err)
			}
			raw := "From: sender@origin.example\r\nSubject: envelope\r\n"
			if tc.headerTo != "" {
				raw += "To: " + tc.headerTo + "\r\n"
			}
			msg := f.message(t, raw+"\r\nhello")
			result, err := f.service.Deliver(context.Background(), "sender@origin.example", tc.recipients, msg)
			if err != nil {
				t.Fatal(err)
			}
			if result.Accepted != tc.accepted || result.Rejected != len(tc.recipients)-tc.accepted || len(result.Recipients) != len(tc.recipients) {
				t.Fatalf("result=%+v", result)
			}
			ids := map[string]string{}
			for i, outcome := range result.Recipients {
				if outcome.Recipient != tc.recipients[i] {
					t.Fatalf("outcome order=%+v", outcome)
				}
				if outcome.Accepted {
					email, err := f.service.GetEmailByID(context.Background(), outcome.Recipient, outcome.EmailID)
					if err != nil || email == nil || email.To != tc.headerTo {
						t.Fatalf("stored=%+v err=%v", email, err)
					}
					if other, exists := ids[outcome.EmailID]; exists && other != outcome.Recipient {
						t.Fatal("recipient copies share database primary key")
					}
					ids[outcome.EmailID] = outcome.Recipient
				}
			}
			assertStorage(t, f, header.ID, 0)
			assertStorage(t, f, one.ID, 1)
			wantTwo := int64(0)
			for _, recipient := range tc.recipients {
				if recipient == two.Address {
					wantTwo = 1
				}
			}
			assertStorage(t, f, two.ID, wantTwo)
			if tc.name == "mixed outcomes" {
				for i, reason := range map[int]string{0: "Mailbox missing@test.example does not exist", 2: "Mailbox disabled@test.example is disabled", 3: "Mailbox expired@test.example has expired"} {
					if result.Recipients[i].Reason != reason {
						t.Fatalf("reason=%q want=%q", result.Recipients[i].Reason, reason)
					}
				}
			}
		})
	}
}

func TestSenderWhitelist(t *testing.T) {
	for _, tc := range []struct {
		name             string
		list             []string
		enabled          bool
		header, envelope string
		accepted         bool
	}{
		{"disabled", []string{"other@allowed.example"}, false, "a@bad.example", "b@bad.example", true},
		{"empty enabled", nil, true, "a@bad.example", "b@bad.example", true},
		{"exact", []string{"sender@allowed.example"}, true, "sender@allowed.example", "sender@allowed.example", true},
		{"domain", []string{"@allowed.example"}, true, "sender@allowed.example", "bounce@allowed.example", true},
		{"header rejected", []string{"@allowed.example"}, true, "sender@bad.example", "bounce@allowed.example", false},
		// 信封发件人不参与白名单判断，第三方代发（退信地址域名不同）仍可投递。
		{"third-party envelope", []string{"@allowed.example"}, true, "sender@allowed.example", "bounce@amazonses.example", true},
		{"null reverse path", []string{"@allowed.example"}, true, "sender@allowed.example", "", true},
		{"case insensitive", []string{"@Allowed.Example"}, true, "Sender@allowed.example", "sender@allowed.example", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, nil)
			m := f.mailbox(t, "in@test.example", CreateMailboxOptions{SenderWhitelist: tc.list})
			if _, err := f.service.UpdateWhitelistStatus(context.Background(), m.ID, tc.enabled); err != nil {
				t.Fatal(err)
			}
			msg := f.message(t, "From: "+tc.header+"\r\n\r\nhello")
			result, err := f.service.Deliver(context.Background(), tc.envelope, []string{m.Address}, msg)
			if err != nil {
				t.Fatal(err)
			}
			if result.Recipients[0].Accepted != tc.accepted {
				t.Fatalf("result=%+v", result)
			}
			if !tc.accepted && !strings.HasPrefix(result.Recipients[0].Reason, "Sender ") {
				t.Fatalf("reason=%q", result.Recipients[0].Reason)
			}
		})
	}
}

func TestSizeAndCapacity(t *testing.T) {
	for _, tc := range []struct {
		name                string
		body                string
		sizeLimit, capacity int64
		accept              bool
	}{
		{"at size limit", "世界", 6, 100, true},
		{"utf8 exceeds size", "世界!", 6, 100, false},
		{"at capacity", "hello", 100, 5, true},
		{"capacity exceeds", "hello!", 100, 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, func(c *config.Config) { c.MaxEmailSizeBytes = tc.sizeLimit })
			m := f.mailbox(t, "in@test.example", CreateMailboxOptions{})
			if _, err := f.db.DB().Exec("UPDATE mailboxes SET storage_limit = ? WHERE id = ?", tc.capacity, m.ID); err != nil {
				t.Fatal(err)
			}
			result, err := f.service.Deliver(context.Background(), "", []string{m.Address}, &mailparse.Message{Body: tc.body, Timestamp: f.clock.Load()})
			if err != nil {
				t.Fatal(err)
			}
			if result.Recipients[0].Accepted != tc.accept {
				t.Fatalf("result=%+v", result)
			}
			count := int64(0)
			if tc.accept {
				count = 1
			}
			assertStorage(t, f, m.ID, count)
		})
	}
}

func TestCountPruningAndDeletionAccounting(t *testing.T) {
	f := setup(t, func(c *config.Config) { c.MaxEmailsPerAddress = 2 })
	m := f.mailbox(t, "in@test.example", CreateMailboxOptions{})
	for i, body := range []string{"old", "newer", "newest"} {
		result, err := f.service.Deliver(context.Background(), "", []string{m.Address}, &mailparse.Message{ID: fmt.Sprintf("email-%d", i), Body: body, Timestamp: f.clock.Load()})
		if err != nil || result.Accepted != 1 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
	if used := assertStorage(t, f, m.ID, 2); used != int64(len("newernewest")) {
		t.Fatalf("used=%d", used)
	}
	emails, err := f.service.GetInboxEmails(context.Background(), m.Address)
	if err != nil || len(emails) != 2 || emails[0].Body != "newest" || emails[1].Body != "newer" {
		t.Fatalf("emails=%+v err=%v", emails, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := f.db.AddEmail(m.ID, store.Email{Body: "extra", Timestamp: f.clock.Load() + int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	count, err := f.service.LimitEmailsPerMailbox(context.Background(), m.ID)
	if err != nil || count != 2 {
		t.Fatalf("pruned=%d err=%v", count, err)
	}
	assertStorage(t, f, m.ID, 2)
	emails, err = f.service.GetInboxEmails(context.Background(), m.Address)
	if err != nil {
		t.Fatal(err)
	}
	count, err = f.service.DeleteEmail(context.Background(), emails[0].ID)
	if err != nil || count != 1 {
		t.Fatalf("delete=%d err=%v", count, err)
	}
	assertStorage(t, f, m.ID, 1)
}

func TestExpiredAddressRecreationAndKeyRotation(t *testing.T) {
	f := setup(t, nil)
	old := f.mailbox(t, "in@test.example", CreateMailboxOptions{})
	if _, err := f.db.AddEmail(old.ID, store.Email{Body: "old", Timestamp: f.clock.Load()}); err != nil {
		t.Fatal(err)
	}
	f.clock.Store(old.ExpiresAt + 1)
	fresh := f.mailbox(t, old.Address, CreateMailboxOptions{})
	if fresh.ID == old.ID || fresh.MailboxKey == old.MailboxKey || fresh.AccessToken == old.AccessToken {
		t.Fatal("expired credentials reused")
	}
	assertStorage(t, f, fresh.ID, 0)
	var oldCount int
	if err := f.db.DB().QueryRow("SELECT COUNT(*) FROM emails WHERE mailbox_id = ?", old.ID).Scan(&oldCount); err != nil || oldCount != 0 {
		t.Fatalf("old count=%d err=%v", oldCount, err)
	}
	keys := make(chan string, 12)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			key, err := f.service.RegenerateMailboxKey(context.Background(), fresh.ID, fresh.MailboxKey)
			if err != nil {
				t.Error(err)
			}
			keys <- key
		})
	}
	wg.Wait()
	close(keys)
	success := 0
	for key := range keys {
		if key != "" {
			success++
			if len(key) != 10 {
				t.Fatalf("key length=%d", len(key))
			}
		}
	}
	if success != 1 {
		t.Fatalf("rotations=%d", success)
	}
	if key, err := f.service.RegenerateMailboxKey(context.Background(), fresh.ID, "wrong"); err != nil || key != "" {
		t.Fatalf("wrong-key rotation err=%v", err)
	}
}

func TestMailboxReadMutationAndJSONContracts(t *testing.T) {
	f := setup(t, nil)
	ctx := context.Background()
	m := f.mailbox(t, "in@test.example", CreateMailboxOptions{})
	checkKeys := func(value any, expected string) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var keys map[string]any
		if err = json.Unmarshal(data, &keys); err != nil {
			t.Fatal(err)
		}
		want := strings.Fields(expected)
		if len(keys) != len(want) {
			t.Fatalf("json=%s", data)
		}
		for _, key := range want {
			if _, ok := keys[key]; !ok {
				t.Fatalf("missing %s json=%s", key, data)
			}
		}
	}
	checkKeys(m, "id address created_at expires_at retention_days sender_whitelist whitelist_enabled access_token mailbox_key is_active admin_disabled created_source")
	existing := f.mailbox(t, m.Address, CreateMailboxOptions{})
	checkKeys(existing, "id address created_at expires_at retention_days sender_whitelist whitelist_enabled access_token mailbox_key is_active admin_disabled created_by_ip last_accessed storage_used storage_limit")
	info, err := f.service.GetMailboxInfo(ctx, m.Address)
	if err != nil {
		t.Fatal(err)
	}
	checkKeys(info, "id address created_at expires_at retention_days sender_whitelist whitelist_enabled access_token mailbox_key is_active admin_disabled is_expired email_count unread_count last_email_time")
	if info.LastEmailTime != nil {
		t.Fatal("empty last_email_time must be null")
	}
	result, err := f.service.Deliver(ctx, "", []string{m.Address}, f.message(t, "From: sender@example.com\r\nTo: other@example.com\r\n\r\nbody"))
	if err != nil {
		t.Fatal(err)
	}
	emails, err := f.service.GetInboxEmails(ctx, m.Address)
	if err != nil || len(emails) != 1 {
		t.Fatalf("list err=%v", err)
	}
	checkKeys(emails[0], "id From To Subject Body ContentType Timestamp Sent is_read")
	other := f.mailbox(t, "other@test.example", CreateMailboxOptions{})
	if found, err := f.service.GetEmailByID(ctx, other.Address, result.Recipients[0].EmailID); err != nil || found != nil {
		t.Fatal("cross-mailbox read")
	}
	email, err := f.service.GetEmailByID(ctx, m.Address, result.Recipients[0].EmailID)
	if err != nil || email == nil || email.IsRead {
		t.Fatalf("get=%+v err=%v", email, err)
	}
	checkKeys(email, "id mailbox_id From To Subject Body ContentType Timestamp Sent is_read")
	info, err = f.service.GetMailboxInfo(ctx, m.Address)
	if err != nil || info.UnreadCount != 0 {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	if ok, err := f.service.MarkEmailAsUnread(ctx, email.ID); err != nil || !ok {
		t.Fatal(err)
	}
	if count, err := f.service.MarkAllEmailsRead(ctx, m.ID); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if ok, err := f.service.MarkEmailAsRead(ctx, "missing"); err != nil || !ok {
		t.Fatal("Python missing mark semantics changed")
	}
	for _, operation := range []func() (bool, error){
		func() (bool, error) { return f.service.AddSenderToWhitelist(ctx, m.ID, "@example.com") },
		func() (bool, error) { return f.service.AddSenderToWhitelist(ctx, m.ID, "@example.com") },
		func() (bool, error) { return f.service.RemoveSenderFromWhitelist(ctx, m.ID, "@example.com") },
		func() (bool, error) {
			return f.service.UpdateSenderWhitelist(ctx, m.ID, []string{"one@example.com", "@example.com"})
		},
		func() (bool, error) { return f.service.UpdateWhitelistStatus(ctx, m.ID, true) },
		func() (bool, error) { return f.service.UpdateMailboxRetention(ctx, m.ID, 3) },
	} {
		if ok, err := operation(); err != nil || !ok {
			t.Fatalf("mutation=%v err=%v", ok, err)
		}
	}
	info, err = f.service.GetMailboxInfo(ctx, m.Address)
	if err != nil || len(info.SenderWhitelist) != 2 || !info.WhitelistEnabled || info.ExpiresAt != f.clock.Load()+3*86400 {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	if ok, err := f.service.UpdateMailboxStatus(ctx, m.ID, false); err != nil || !ok {
		t.Fatal(err)
	}
	if list, err := f.service.GetInboxEmails(ctx, m.Address); err != nil || len(list) != 0 || list == nil {
		t.Fatalf("disabled list=%+v err=%v", list, err)
	}
	if ok, err := f.service.DeleteMailbox(ctx, m.ID); err != nil || !ok {
		t.Fatal(err)
	}
	if info, err := f.service.GetMailboxInfo(ctx, m.Address); err != nil || info != nil {
		t.Fatal("deleted mailbox exists")
	}
	if ok, err := f.service.DeleteMailbox(ctx, m.ID); err != nil || ok {
		t.Fatal("repeat deletion")
	}
}

func TestDeliveryRollbackAndNoImplicitCleanup(t *testing.T) {
	f := setup(t, nil)
	ctx := context.Background()
	first := f.mailbox(t, "first@test.example", CreateMailboxOptions{})
	second := f.mailbox(t, "second@test.example", CreateMailboxOptions{})
	expired := f.mailbox(t, "expired@test.example", CreateMailboxOptions{})
	if _, err := f.db.DB().Exec("UPDATE mailboxes SET expires_at = ? WHERE id = ?", f.clock.Load()-1, expired.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.DB().Exec(`CREATE TRIGGER fail_second BEFORE INSERT ON emails WHEN NEW.mailbox_id = '` + second.ID + `' BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	result, err := f.service.Deliver(ctx, "", []string{first.Address, second.Address}, &mailparse.Message{Body: "body", Timestamp: f.clock.Load()})
	if err == nil || result.Accepted != 0 {
		t.Fatalf("rollback result=%+v err=%v", result, err)
	}
	assertStorage(t, f, first.ID, 0)
	if _, err := f.db.DB().Exec("DROP TRIGGER fail_second"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Deliver(ctx, "", []string{first.Address}, &mailparse.Message{Body: "old", Timestamp: f.clock.Load() - f.cfg.EmailRetentionTime - 1}); err != nil {
		t.Fatal(err)
	}
	assertStorage(t, f, first.ID, 1)
	if info, err := f.service.GetMailboxInfo(ctx, expired.Address); err != nil || info == nil || !info.IsExpired {
		t.Fatal("delivery cleaned expired mailbox")
	}
}

func TestCreateConcurrentAndWhitelistUpdates(t *testing.T) {
	f := setup(t, nil)
	ctx := context.Background()
	var wg sync.WaitGroup
	ids := make(chan string, 10)
	for i := range 10 {
		wg.Go(func() {
			m, err := f.service.CreateOrGetMailbox(ctx, "same@test.example", CreateMailboxOptions{})
			if err != nil {
				t.Error(err)
				return
			}
			ids <- m.ID
			if _, err := f.service.AddSenderToWhitelist(ctx, m.ID, fmt.Sprintf("sender%d@example.com", i)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	close(ids)
	id := ""
	for found := range ids {
		if id != "" && id != found {
			t.Fatal("non-atomic mailbox creation")
		}
		id = found
	}
	info, err := f.service.GetMailboxInfo(ctx, "same@test.example")
	if err != nil || len(info.SenderWhitelist) != 10 {
		t.Fatalf("info=%+v err=%v", info, err)
	}
}

func TestNullableJSONAndTrustedListLimits(t *testing.T) {
	f := setup(t, nil)
	ctx := context.Background()
	m := f.mailbox(t, "nullable@test.example", CreateMailboxOptions{})
	if _, err := f.db.DB().Exec(`UPDATE mailboxes SET retention_days = NULL, access_token = NULL, sender_whitelist = 'null' WHERE id = ?`, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.DB().Exec(`INSERT INTO emails (id, mailbox_id, from_address, to_address, timestamp, subject, body, content_type, sent_formatted)
 VALUES ('legacy', ?, 'sender@example.com', 'header@example.com', ?, NULL, NULL, NULL, NULL)`, m.ID, f.clock.Load()); err != nil {
		t.Fatal(err)
	}
	info, err := f.service.GetMailboxInfo(ctx, m.Address)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"retention_days", "access_token"} {
		if string(fields[name]) != "null" {
			t.Fatalf("%s=%s", name, fields[name])
		}
	}
	if string(fields["sender_whitelist"]) != "[]" {
		t.Fatalf("whitelist=%s", fields["sender_whitelist"])
	}
	for _, limit := range []int{0, -1, 1} {
		emails, err := f.service.GetEmailsByMailbox(ctx, m.ID, limit)
		if err != nil || len(emails) != 1 {
			t.Fatalf("limit=%d count=%d err=%v", limit, len(emails), err)
		}
		data, err := json.Marshal(emails[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"Subject", "Body", "ContentType", "Sent"} {
			if string(fields[name]) != "null" {
				t.Fatalf("%s=%s", name, fields[name])
			}
		}
	}
	if count, err := f.service.DeleteEmail(ctx, "legacy"); err != nil || count != 1 {
		t.Fatalf("delete=%d err=%v", count, err)
	}
	if count, err := f.service.DeleteEmail(ctx, "legacy"); err != nil || count != 0 {
		t.Fatalf("repeat delete=%d err=%v", count, err)
	}
}

func TestConcurrentCapacityAndHistoricalAccounting(t *testing.T) {
	f := setup(t, func(cfg *config.Config) { cfg.MaxEmailsPerAddress = 2 })
	ctx := context.Background()
	m := f.mailbox(t, "capacity@test.example", CreateMailboxOptions{})
	if _, err := f.db.DB().Exec("UPDATE mailboxes SET storage_limit = 10 WHERE id = ?", m.ID); err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			result, err := f.service.Deliver(ctx, "", []string{m.Address}, &mailparse.Message{Body: "hello", Timestamp: f.clock.Load()})
			if err != nil {
				t.Error(err)
				return
			}
			accepted.Add(int64(result.Accepted))
		})
	}
	wg.Wait()
	if accepted.Load() != 2 {
		t.Fatalf("concurrent accepts=%d", accepted.Load())
	}
	assertStorage(t, f, m.ID, 2)
	// 历史计数偏移不应被本次裁剪或清理静默重写。
	if _, err := f.db.DB().Exec("UPDATE mailboxes SET storage_used = storage_used + 100, storage_limit = 1000 WHERE id = ?", m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Deliver(ctx, "", []string{m.Address}, &mailparse.Message{Body: "new", Timestamp: f.clock.Load() + 1}); err != nil {
		t.Fatal(err)
	}
	var used, sum int64
	if err := f.db.DB().QueryRow("SELECT storage_used, (SELECT SUM(size_bytes) FROM emails WHERE mailbox_id = ?) FROM mailboxes WHERE id = ?", m.ID, m.ID).Scan(&used, &sum); err != nil {
		t.Fatal(err)
	}
	if used-sum != 100 {
		t.Fatalf("pruning overwrote historical offset: used=%d sum=%d", used, sum)
	}
	f.clock.Add(f.cfg.EmailRetentionTime + 2)
	if _, err := f.service.CleanExpiredData(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.db.DB().QueryRow("SELECT storage_used FROM mailboxes WHERE id = ?", m.ID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != 100 {
		t.Fatalf("cleanup overwrote historical offset: used=%d", used)
	}
}
