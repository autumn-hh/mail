package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"maildrop/internal/config"
	"maildrop/internal/mailparse"
	"maildrop/internal/store"
)

func TestLegacyREALReadsAndDelivery(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		name := "written after migration"
		if reopen {
			name = "normalized on reopen"
		}
		t.Run(name, func(t *testing.T) {
			f := setup(t, func(cfg *config.Config) { cfg.MaxEmailsPerAddress = 1 })
			ctx := context.Background()
			m := f.mailbox(t, "legacy@example.com", CreateMailboxOptions{})
			if _, err := f.db.DB().Exec(`UPDATE mailboxes SET created_at = 1800000000.75,
 expires_at = 1800129600.75, retention_days = 1.5, last_accessed = 1800000001.75,
 storage_used = 100.75, storage_limit = 100000.75, is_active = 1.75,
 whitelist_enabled = 0.75 WHERE id = ?`, m.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.DB().Exec(`INSERT INTO emails (id, mailbox_id, from_address, to_address,
 timestamp, size_bytes, is_read) VALUES ('legacy-real', ?, 'sender@example.com', ?,
 1799999999.75, 17.75, 1.75)`, m.ID, m.Address); err != nil {
				t.Fatal(err)
			}
			if reopen {
				var path string
				if err := f.db.DB().QueryRow("SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&path); err != nil {
					t.Fatal(err)
				}
				if err := f.db.Close(); err != nil {
					t.Fatal(err)
				}
				db, err := store.OpenWithConfig(path, f.cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				f.db = db
				f.service = New(f.cfg, db, func() time.Time { return time.Unix(f.clock.Load(), 0) })
			}
			got, err := f.service.CreateOrGetMailbox(ctx, m.Address, CreateMailboxOptions{})
			if err != nil || got.CreatedAt != 1800000000 || got.ExpiresAt != 1800129600 || got.RetentionDays != 1 ||
				got.LastAccessed == nil || *got.LastAccessed != 1800000001 || got.StorageUsed != 100 || got.StorageLimit != 100000 || !got.IsActive || got.WhitelistEnabled {
				t.Fatalf("legacy mailbox = %+v, err = %v", got, err)
			}
			info, err := f.service.GetMailboxInfo(ctx, m.Address)
			if err != nil || info.LastEmailTime == nil || *info.LastEmailTime != 1799999999 {
				t.Fatalf("legacy info = %+v, err = %v", info, err)
			}
			emails, err := f.service.GetInboxEmails(ctx, m.Address)
			if err != nil || len(emails) != 1 || emails[0].Timestamp != 1799999999 || !emails[0].IsRead {
				t.Fatalf("legacy emails = %+v, err = %v", emails, err)
			}
			if email, err := f.service.GetEmailByID(ctx, m.Address, "legacy-real"); err != nil || email == nil || email.Timestamp != 1799999999 {
				t.Fatalf("legacy email = %+v, err = %v", email, err)
			}
			result, err := f.service.Deliver(ctx, "", []string{m.Address}, f.message(t, "From: sender@example.com\r\n\r\nbody"))
			if err != nil || result.Accepted != 1 {
				t.Fatalf("legacy delivery = %+v, err = %v", result, err)
			}
			emails, err = f.service.GetEmailsByMailbox(ctx, m.ID)
			if err != nil || len(emails) != 1 || emails[0].ID == "legacy-real" {
				t.Fatalf("pruned emails = %+v, err = %v", emails, err)
			}
		})
	}
}

func TestLegacyREALDeletionAndCleanup(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		name := "delete"
		if cleanup {
			name = "cleanup"
		}
		t.Run(name, func(t *testing.T) {
			f := setup(t, nil)
			m := f.mailbox(t, "legacy@example.com", CreateMailboxOptions{})
			if _, err := f.db.DB().Exec(`INSERT INTO emails (id, mailbox_id, from_address, to_address,
 timestamp, size_bytes) VALUES ('legacy-real', ?, 'sender@example.com', ?, ?, 17.75)`,
				m.ID, m.Address, float64(f.clock.Load()-f.cfg.EmailRetentionTime-1)+0.75); err != nil {
				t.Fatal(err)
			}
			if cleanup {
				result, err := f.service.CleanExpiredData(context.Background())
				if err != nil || result.Emails != 1 || result.Bytes != 17 {
					t.Fatalf("cleanup = %+v, err = %v", result, err)
				}
			} else if count, err := f.service.DeleteEmail(context.Background(), "legacy-real"); err != nil || count != 1 {
				t.Fatalf("delete = %d, err = %v", count, err)
			}
		})
	}
}

func TestMailboxLegacyLists(t *testing.T) {
	f := setup(t, nil)
	m := f.mailbox(t, "lists@example.com", CreateMailboxOptions{})
	for _, tc := range []struct {
		name, encoded string
		want          []string
	}{
		{"invalid", "[", []string{}},
		{"object", `{"domain":"example.com"}`, []string{}},
		{"string", `"example.com"`, []string{}},
		{"null", "null", []string{}},
		{"empty text", "", []string{}},
		{"empty array", "[]", []string{}},
		{"mixed", `["example.com",1,true,null,{},[],"sender@example.com",1e400]`, []string{"example.com", "sender@example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.db.DB().Exec("UPDATE mailboxes SET sender_whitelist = ?, allowed_domains = ?, whitelist_enabled = 1 WHERE id = ?", tc.encoded, tc.encoded, m.ID); err != nil {
				t.Fatal(err)
			}
			got, err := f.service.CreateOrGetMailbox(context.Background(), m.Address, CreateMailboxOptions{})
			if err != nil || !reflect.DeepEqual(got.SenderWhitelist, tc.want) || !reflect.DeepEqual(got.AllowedDomains, tc.want) {
				t.Fatalf("mailbox = %+v, err = %v", got, err)
			}
			info, err := f.service.GetMailboxInfo(context.Background(), m.Address)
			if err != nil || !reflect.DeepEqual(info.SenderWhitelist, tc.want) {
				t.Fatalf("info = %+v, err = %v", info, err)
			}
			result, err := f.service.Deliver(context.Background(), "", []string{m.Address}, &mailparse.Message{From: "sender@example.com", Timestamp: f.clock.Load()})
			if err != nil || result.Accepted != 1 {
				t.Fatalf("delivery = %+v, err = %v", result, err)
			}
			var whitelist, domains string
			if err := f.db.DB().QueryRow("SELECT sender_whitelist, allowed_domains FROM mailboxes WHERE id = ?", m.ID).Scan(&whitelist, &domains); err != nil || whitelist != tc.encoded || domains != tc.encoded {
				t.Fatalf("read changed stored lists: %q, %q, err = %v", whitelist, domains, err)
			}
		})
	}
}

func TestDeliverAllowedDomains(t *testing.T) {
	for _, tc := range []struct {
		name, sender string
		domains      []string
		whitelist    []string
		enabled      bool
		accepted     bool
	}{
		{"unrestricted", "sender@other.example", nil, nil, false, true},
		{"domain", "sender@example.com", []string{"example.com"}, nil, false, true},
		{"case insensitive", "sender@EXAMPLE.COM", []string{"Example.Com"}, nil, false, true},
		{"subdomain", "sender@a.b.EXAMPLE.COM", []string{"example.com"}, nil, false, true},
		{"trailing dot", "sender@SUB.EXAMPLE.COM.", []string{"example.com"}, nil, false, true},
		{"last at", "sender@local@EXAMPLE.COM", []string{"example.com"}, nil, false, true},
		{"multiple domains", "sender@other.example", []string{"example.com", "other.example"}, nil, false, true},
		{"whitelist disabled", "sender@other.example", []string{"example.com"}, []string{"other.example"}, false, false},
		{"enabled empty whitelist", "sender@other.example", []string{"example.com"}, nil, true, false},
		{"domain suffix attack", "sender@badexample.com", []string{"example.com"}, nil, false, false},
		{"domain extension attack", "sender@example.com.evil", []string{"example.com"}, nil, false, false},
		{"missing domain", "sender", []string{"example.com"}, nil, false, false},
		{"empty domain rule", "sender@example.com", []string{""}, nil, false, false},
		{"both pass", "sender@EXAMPLE.COM", []string{"example.com"}, []string{"Sender@example.com"}, true, true},
		{"whitelist rejects", "sender@example.com", []string{"example.com"}, []string{"other@example.com"}, true, false},
		{"domain rejects", "sender@other.example", []string{"example.com"}, []string{"sender@other.example"}, true, false},
		{"bare whitelist domain", "sender@example.com", nil, []string{"EXAMPLE.COM"}, true, true},
		{"bare whitelist rejects", "sender@badexample.com", nil, []string{"example.com"}, true, false},
		{"bare equals at rule", "sender@sub.example.com", nil, []string{"example.com"}, true, false},
		{"exact whitelist address", "SENDER@example.com", nil, []string{"sender@EXAMPLE.COM"}, true, true},
		{"exact address remains exact", "other@example.com", nil, []string{"sender@example.com"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, nil)
			m := f.mailbox(t, "in@example.com", CreateMailboxOptions{SenderWhitelist: tc.whitelist})
			domains := append([]string{}, tc.domains...)
			data, err := json.Marshal(domains)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.DB().Exec("UPDATE mailboxes SET allowed_domains = ?, whitelist_enabled = ? WHERE id = ?", string(data), tc.enabled, m.ID); err != nil {
				t.Fatal(err)
			}
			result, err := f.service.Deliver(context.Background(), "bounce@unrelated.example", []string{m.Address}, &mailparse.Message{From: tc.sender, Timestamp: f.clock.Load()})
			if err != nil || len(result.Recipients) != 1 || result.Recipients[0].Accepted != tc.accepted {
				t.Fatalf("delivery = %+v, err = %v", result, err)
			}
			if !tc.accepted && result.Recipients[0].Reason != "Sender "+tc.sender+" not allowed for mailbox "+m.Address {
				t.Fatalf("reason = %q", result.Recipients[0].Reason)
			}
			count := int64(0)
			if tc.accepted {
				count = 1
			}
			assertStorage(t, f, m.ID, count)
		})
	}
}

func TestDeliverRecipientDomainCase(t *testing.T) {
	for _, tc := range []struct {
		name, recipient string
		stored          []string
		want            int
	}{
		{"lowercase stored", "foo@EXAMPLE.COM", []string{"foo@example.com"}, 0},
		{"mixed case stored", "foo@EXAMPLE.COM", []string{"foo@ExAmPlE.CoM"}, 0},
		{"lowercase recipient", "foo@example.com", []string{"foo@ExAmPlE.CoM"}, 0},
		{"exact first", "foo@EXAMPLE.COM", []string{"foo@example.com", "foo@EXAMPLE.COM"}, 1},
		{"lowercase second", "foo@EXAMPLE.COM", []string{"foo@ExAmPlE.CoM", "foo@example.com"}, 1},
		{"oldest fallback", "foo@EXAMPLE.COM", []string{"foo@ExAmPlE.CoM", "foo@eXaMpLe.cOm"}, 0},
		{"local part remains exact", "Foo@EXAMPLE.COM", []string{"foo@example.com", "foo@ExAmPlE.CoM"}, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, nil)
			mailboxes := make([]*Mailbox, len(tc.stored))
			for i, address := range tc.stored {
				mailboxes[i] = f.mailbox(t, address, CreateMailboxOptions{})
			}
			result, err := f.service.Deliver(context.Background(), "", []string{tc.recipient}, &mailparse.Message{Body: "body", Timestamp: f.clock.Load()})
			if err != nil || len(result.Recipients) != 1 || result.Recipients[0].Recipient != tc.recipient || result.Recipients[0].Accepted != (tc.want >= 0) {
				t.Fatalf("delivery = %+v, err = %v", result, err)
			}
			for i, m := range mailboxes {
				count := int64(0)
				if i == tc.want {
					count = 1
				}
				assertStorage(t, f, m.ID, count)
			}
			if tc.want < 0 {
				if result.Recipients[0].Reason != "Mailbox "+tc.recipient+" does not exist" {
					t.Fatal(result.Recipients[0].Reason)
				}
			} else if err := f.db.WithTx(context.Background(), func(tx *sql.Tx) error {
				m, err := f.service.mailbox(context.Background(), tx, tc.recipient)
				if tc.stored[tc.want] != tc.recipient && m != nil {
					t.Error("non-delivery mailbox lookups changed")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
