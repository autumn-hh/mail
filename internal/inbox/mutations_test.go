package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"maildrop/internal/store"
)

func TestMailboxMutationsIgnoreRecreatedAddress(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(context.Context, *Service, *Mailbox) (bool, error)
	}{
		{"add sender", func(ctx context.Context, s *Service, m *Mailbox) (bool, error) {
			return s.AddSenderToWhitelist(ctx, m.ID, "@added.example")
		}},
		{"remove sender", func(ctx context.Context, s *Service, m *Mailbox) (bool, error) {
			return s.RemoveSenderFromWhitelist(ctx, m.ID, "@kept.example")
		}},
		{"retention", func(ctx context.Context, s *Service, m *Mailbox) (bool, error) {
			return s.UpdateMailboxRetention(ctx, m.ID, 31)
		}},
		{"key CAS", func(ctx context.Context, s *Service, m *Mailbox) (bool, error) {
			key, err := s.RegenerateMailboxKey(ctx, m.ID, m.MailboxKey)
			return key != "", err
		}},
		{"mailbox status", func(ctx context.Context, s *Service, m *Mailbox) (bool, error) {
			return s.UpdateMailboxStatus(ctx, m.ID, false)
		}},
		{"whitelist status", func(ctx context.Context, s *Service, m *Mailbox) (bool, error) {
			return s.UpdateWhitelistStatus(ctx, m.ID, false)
		}},
		{"replace whitelist", func(ctx context.Context, s *Service, m *Mailbox) (bool, error) {
			return s.UpdateSenderWhitelist(ctx, m.ID, []string{"@replacement.example"})
		}},
		{"delete mailbox", func(ctx context.Context, s *Service, m *Mailbox) (bool, error) {
			return s.DeleteMailbox(ctx, m.ID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, nil)
			ctx := context.Background()
			owner := f.mailbox(t, "reused@test.example", CreateMailboxOptions{})
			authenticated, err := f.service.GetMailboxByToken(ctx, owner.AccessToken)
			if err != nil || authenticated == nil || authenticated.ID != owner.ID {
				t.Fatalf("authentication snapshot=%+v err=%v", authenticated, err)
			}
			if _, err := f.db.DB().Exec("DELETE FROM mailboxes WHERE id = ?", owner.ID); err != nil {
				t.Fatal(err)
			}
			fresh := f.mailbox(t, owner.Address, CreateMailboxOptions{SenderWhitelist: []string{"@kept.example"}})
			if fresh.ID == authenticated.ID || fresh.AccessToken == owner.AccessToken {
				t.Fatal("recreated mailbox reused authentication identity")
			}
			// 密钥相同也不能让旧 ID 操作新行。
			if _, err := f.db.DB().Exec("UPDATE mailboxes SET mailbox_key = ? WHERE id = ?", authenticated.MailboxKey, fresh.ID); err != nil {
				t.Fatal(err)
			}
			emailID, err := f.db.AddEmail(fresh.ID, store.Email{From: "sender@example.com", Body: "replacement", Timestamp: f.clock.Load()})
			if err != nil {
				t.Fatal(err)
			}
			before, err := f.service.GetMailboxByToken(ctx, fresh.AccessToken)
			if err != nil || before == nil {
				t.Fatalf("replacement snapshot=%+v err=%v", before, err)
			}
			if changed, err := tc.mutate(ctx, f.service, authenticated); err != nil || changed {
				t.Fatalf("stale mutation changed=%v err=%v", changed, err)
			}
			after, err := f.service.GetMailboxByToken(ctx, fresh.AccessToken)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("replacement changed: before=%+v after=%+v err=%v", before, after, err)
			}
			if email, err := f.db.GetEmailByID(emailID); err != nil || email.MailboxID != fresh.ID {
				t.Fatalf("replacement email changed: email=%+v err=%v", email, err)
			}
			if old, err := f.service.GetMailboxByToken(ctx, owner.AccessToken); err != nil || old != nil {
				t.Fatalf("old token still authenticates: mailbox=%+v err=%v", old, err)
			}
		})
	}
}

func TestMailboxStatusAdminDisabledGuard(t *testing.T) {
	for _, tc := range []struct {
		name          string
		initialActive bool
		adminDisabled bool
		active        bool
		blocked       bool
	}{
		{"holder pause", true, false, false, false},
		{"holder resume", false, false, true, false},
		{"administrator resume blocked", false, true, true, true},
		{"administrator pause allowed", true, true, false, false},
		{"administrator repeated pause allowed", false, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, nil)
			ctx := context.Background()
			owner := f.mailbox(t, "status@test.example", CreateMailboxOptions{})
			if _, err := f.db.DB().Exec("UPDATE mailboxes SET is_active = ?, admin_disabled = ? WHERE id = ?", tc.initialActive, tc.adminDisabled, owner.ID); err != nil {
				t.Fatal(err)
			}
			changed, err := f.service.UpdateMailboxStatus(ctx, owner.ID, tc.active)
			if tc.blocked {
				if changed || !errors.Is(err, ErrAdminDisabled) {
					t.Fatalf("blocked status changed=%v err=%v", changed, err)
				}
			} else if err != nil || !changed {
				t.Fatalf("allowed status changed=%v err=%v", changed, err)
			}
			mailbox, err := f.service.GetMailboxByToken(ctx, owner.AccessToken)
			wantActive := tc.active
			if tc.blocked {
				wantActive = tc.initialActive
			}
			if err != nil || mailbox == nil || mailbox.IsActive != wantActive || mailbox.AdminDisabled != tc.adminDisabled {
				t.Fatalf("status=%+v err=%v", mailbox, err)
			}
			info, err := f.service.GetMailboxInfo(ctx, owner.Address)
			if err != nil || info == nil || info.AdminDisabled != tc.adminDisabled {
				t.Fatalf("mailbox info=%+v err=%v", info, err)
			}
			for _, value := range []any{mailbox, info} {
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]any
				if err := json.Unmarshal(data, &fields); err != nil || fields["admin_disabled"] != tc.adminDisabled {
					t.Fatalf("admin_disabled JSON=%s err=%v", data, err)
				}
			}
		})
	}
	f := setup(t, nil)
	for _, active := range []bool{false, true} {
		if changed, err := f.service.UpdateMailboxStatus(context.Background(), "missing-id", active); err != nil || changed {
			t.Fatalf("missing mailbox confused with administrator block: changed=%v err=%v", changed, err)
		}
	}
}
