package admin_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"maildrop/internal/admin"
)

func TestMailboxAdminDisabledTransitions(t *testing.T) {
	svc, db, _ := newMailboxTestService(t)
	ctx := context.Background()
	created, err := svc.CreateMailbox(ctx, admin.CreateOptions{Address: "disabled@example.com"}, admin.Actor{})
	if err != nil {
		t.Fatal(err)
	}
	id := created["id"].(string)
	assertState := func(active, disabled bool) {
		t.Helper()
		var storedActive, storedDisabled bool
		if err := db.DB().QueryRow("SELECT is_active,admin_disabled FROM mailboxes WHERE id=?", id).Scan(&storedActive, &storedDisabled); err != nil {
			t.Fatal(err)
		}
		if storedActive != active || storedDisabled != disabled {
			t.Fatalf("stored active=%v disabled=%v, want %v/%v", storedActive, storedDisabled, active, disabled)
		}
		detail, err := svc.GetMailboxDetail(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		list, err := svc.ListMailboxes(ctx, admin.ListOptions{Search: id})
		if err != nil || len(list.Mailboxes) != 1 {
			t.Fatalf("list=%v err=%v", list, err)
		}
		for _, item := range []map[string]any{detail, list.Mailboxes[0]} {
			if item["is_active"] != active || item["admin_disabled"] != disabled {
				t.Errorf("response active=%v disabled=%v, want active=%v disabled=%v", item["is_active"], item["admin_disabled"], active, disabled)
			}
		}
	}
	assertAudit := func(action string, want any) {
		t.Helper()
		var gotAction, raw string
		if err := db.DB().QueryRow("SELECT action,changes FROM audit_logs WHERE mailbox_id=? ORDER BY rowid DESC LIMIT 1", id).Scan(&gotAction, &raw); err != nil {
			t.Fatal(err)
		}
		var changes map[string]any
		if err := json.Unmarshal([]byte(raw), &changes); err != nil {
			t.Fatal(err)
		}
		if gotAction != action || !reflect.DeepEqual(changes["admin_disabled"], want) {
			t.Errorf("audit=%s %s, want %s admin_disabled=%v", gotAction, raw, action, want)
		}
	}
	assertState(true, false)
	if err := svc.DeleteMailbox(ctx, id, true, admin.Actor{}); err != nil {
		t.Fatal(err)
	}
	assertState(false, true)
	assertAudit("SOFT_DELETE", true)
	if err := svc.EnableMailbox(ctx, id, admin.Actor{}); err != nil {
		t.Fatal(err)
	}
	assertState(true, false)
	assertAudit("RESTORE", false)
	for _, active := range []bool{false, true} {
		if err := svc.UpdateMailbox(ctx, id, map[string]any{"is_active": active}, admin.Actor{}); err != nil {
			t.Fatal(err)
		}
		assertState(active, !active)
		assertAudit("UPDATE", map[string]any{"old": active, "new": !active})
	}
	result := svc.BatchDelete(ctx, []string{id}, true, admin.Actor{})
	if result.SuccessCount != 1 || result.FailedCount != 0 {
		t.Fatalf("batch=%+v", result)
	}
	assertState(false, true)
	assertAudit("SOFT_DELETE", true)
}

func TestMailboxStrictListsService(t *testing.T) {
	for _, tt := range []struct {
		name, field, message string
		value                any
	}{
		{"whitelist false item", "sender_whitelist", "白名单条目必须是字符串", []any{false}},
		{"whitelist zero item", "sender_whitelist", "白名单条目必须是字符串", []any{0}},
		{"whitelist null item", "sender_whitelist", "白名单条目必须是字符串", []any{nil}},
		{"whitelist false", "sender_whitelist", "白名单必须是列表格式", false},
		{"whitelist empty string", "sender_whitelist", "白名单必须是列表格式", ""},
		{"domains string", "allowed_domains", "允许的域名必须是字符串列表", "example.com"},
		{"domains false", "allowed_domains", "允许的域名必须是字符串列表", false},
		{"domains nonstring", "allowed_domains", "允许的域名必须是字符串列表", []any{nil}},
		{"domains invalid", "allowed_domains", "允许的域名格式不正确: bad/domain", []string{"bad/domain"}},
		{"domains double dot", "allowed_domains", "允许的域名格式不正确: example.com..", []string{"example.com.."}},
		{"domains blank", "allowed_domains", "允许的域名格式不正确: ", []string{"  "}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, db, _ := newMailboxTestService(t)
			ctx := context.Background()
			options := admin.CreateOptions{Address: "strict@example.com"}
			if tt.field == "sender_whitelist" {
				options.SenderWhitelist = tt.value
			} else {
				options.AllowedDomains = tt.value
			}
			if _, err := svc.CreateMailbox(ctx, options, admin.Actor{}); err == nil || err.Error() != tt.message {
				t.Fatalf("create error=%v, want %q", err, tt.message)
			}
			created, err := svc.CreateMailbox(ctx, admin.CreateOptions{Address: "strict@example.com"}, admin.Actor{})
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.UpdateMailbox(ctx, created["id"].(string), map[string]any{tt.field: tt.value, "is_active": false}, admin.Actor{}); err == nil || err.Error() != tt.message {
				t.Fatalf("update error=%v, want %q", err, tt.message)
			}
			var count int
			if err := db.DB().QueryRow("SELECT COUNT(*) FROM audit_logs").Scan(&count); err != nil || count != 1 {
				t.Fatalf("audit count=%d err=%v", count, err)
			}
			detail, err := svc.GetMailboxDetail(ctx, created["id"].(string))
			if err != nil || detail["is_active"] != true {
				t.Fatalf("invalid update changed mailbox: detail=%v err=%v", detail, err)
			}
		})
	}
}

func TestMailboxListNormalizationService(t *testing.T) {
	svc, _, _ := newMailboxTestService(t)
	ctx := context.Background()
	whitelist := []string{" Sender@Example.NET ", "sender@example.net", "", "  ", " @Example.ORG ", "@example.org"}
	domains := []string{" Example.COM. ", "EXAMPLE.com", " BÜCHER.DE. ", "xn--bcher-kva.de"}
	created, err := svc.CreateMailbox(ctx, admin.CreateOptions{Address: "normalized@example.com", SenderWhitelist: whitelist, AllowedDomains: domains}, admin.Actor{})
	if err != nil {
		t.Fatal(err)
	}
	id := created["id"].(string)
	for _, operation := range []string{"create", "update"} {
		if operation == "update" {
			if err := svc.UpdateMailbox(ctx, id, map[string]any{"sender_whitelist": whitelist, "allowed_domains": domains}, admin.Actor{}); err != nil {
				t.Fatal(err)
			}
		}
		detail, err := svc.GetMailboxDetail(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for field, want := range map[string][]string{"sender_whitelist": {"Sender@Example.NET", "@Example.ORG"}, "allowed_domains": {"example.com", "xn--bcher-kva.de"}} {
			got, err := json.Marshal(detail[field])
			wantJSON, _ := json.Marshal(want)
			if err != nil || string(got) != string(wantJSON) {
				t.Errorf("%s %s=%s err=%v, want %s", operation, field, got, err, wantJSON)
			}
		}
	}
}

func TestAuditLogServiceLimits(t *testing.T) {
	svc, db, _ := newMailboxTestService(t)
	ctx := context.Background()
	created, err := svc.CreateMailbox(ctx, admin.CreateOptions{Address: "auditlimits@example.com"}, admin.Actor{})
	if err != nil {
		t.Fatal(err)
	}
	id := created["id"].(string)
	if err := db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`WITH RECURSIVE entries(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM entries WHERE n<1005)
 INSERT INTO audit_logs (id,timestamp,action,mailbox_id,admin_user,changes)
 SELECT 'limit-' || n,1800000000+n,'UPDATE',?,'admin','{}' FROM entries`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, mailboxID := range []string{"", id} {
		fallback := 100
		if mailboxID != "" {
			fallback = 50
		}
		for _, tt := range []struct{ limit, want int }{{-1, fallback}, {0, fallback}, {1, 1}, {1000, 1000}, {5000, 1000}} {
			t.Run(fmt.Sprintf("mailbox=%t/limit=%d", mailboxID != "", tt.limit), func(t *testing.T) {
				logs, err := svc.GetAuditLogs(ctx, mailboxID, tt.limit)
				if err != nil || len(logs) != tt.want {
					t.Fatalf("logs=%d err=%v, want %d", len(logs), err, tt.want)
				}
				if logs[0].ID != "limit-1005" {
					t.Errorf("newest log=%s", logs[0].ID)
				}
			})
		}
	}
}

func TestSubAdminStrictWhitelistService(t *testing.T) {
	for _, value := range []any{false, 0, nil, "example.com", []any{false}, []any{0}, []any{nil}} {
		t.Run(fmt.Sprintf("%T-%v", value, value), func(t *testing.T) {
			svc, _, _ := newMailboxTestService(t)
			ctx := context.Background()
			data := map[string]any{"token": "strict-token", "domains": []string{"example.com"}, "sender_whitelist": value}
			if _, err := svc.CreateSubAdmin(ctx, data); err == nil || !strings.Contains(err.Error(), "白名单") {
				t.Fatalf("create error=%v", err)
			}
			delete(data, "sender_whitelist")
			created, err := svc.CreateSubAdmin(ctx, data)
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.UpdateSubAdmin(ctx, created["id"].(string), map[string]any{"sender_whitelist": value}); err == nil || !strings.Contains(err.Error(), "白名单") {
				t.Fatalf("update error=%v", err)
			}
		})
	}
}
