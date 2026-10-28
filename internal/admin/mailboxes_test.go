package admin_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"maildrop/internal/admin"
	"maildrop/internal/config"
	"maildrop/internal/store"
)

func newMailboxTestService(t *testing.T) (*admin.Service, *store.Store, *config.Config) {
	t.Helper()
	cfg := config.FromEnv(func(string) string { return "" })
	db, err := store.OpenWithConfig(filepath.Join(t.TempDir(), "mailbox.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return admin.New(cfg, db, func() time.Time { return time.Unix(1800000000, 0) }), db, cfg
}

func TestMailboxAuditFailureRollsBack(t *testing.T) {
	for _, operation := range []string{"create", "update", "update-disable", "update-enable", "soft-delete", "hard-delete", "reset-token", "enable"} {
		t.Run(operation, func(t *testing.T) {
			svc, db, _ := newMailboxTestService(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			actor := admin.Actor{User: "admin", IP: "192.0.2.1"}
			current, err := svc.CreateMailbox(ctx, admin.CreateOptions{Address: "owner@example.com", AllowedDomains: []string{"example.net"}}, actor)
			if err != nil {
				t.Fatal(err)
			}
			id := current["id"].(string)
			if _, err := db.AddEmail(id, store.Email{From: "sender@example.net", To: "owner@example.com", Timestamp: 1800000000}); err != nil {
				t.Fatal(err)
			}
			if operation == "enable" || operation == "update-enable" {
				if err := svc.DeleteMailbox(ctx, id, true, actor); err != nil {
					t.Fatal(err)
				}
			}
			before, err := svc.GetMailboxDetail(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.DB().Exec(`CREATE TRIGGER fail_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "create":
				_, err = svc.CreateMailbox(ctx, admin.CreateOptions{Address: "second@example.com", AllowedDomains: []string{"allowed.test"}}, actor)
			case "update":
				err = svc.UpdateMailbox(ctx, id, map[string]any{"retention_days": 14, "allowed_domains": []string{"new.test"}}, actor)
			case "update-disable":
				err = svc.UpdateMailbox(ctx, id, map[string]any{"is_active": false}, actor)
			case "update-enable":
				err = svc.UpdateMailbox(ctx, id, map[string]any{"is_active": true}, actor)
			case "soft-delete":
				err = svc.DeleteMailbox(ctx, id, true, actor)
			case "hard-delete":
				err = svc.DeleteMailbox(ctx, id, false, actor)
			case "reset-token":
				_, err = svc.ResetToken(ctx, id, actor)
			case "enable":
				err = svc.EnableMailbox(ctx, id, actor)
			}
			if err == nil || !strings.Contains(err.Error(), "audit unavailable") {
				t.Fatalf("operation error=%v", err)
			}
			after, err := svc.GetMailboxDetail(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			beforeJSON, _ := json.Marshal(before)
			afterJSON, _ := json.Marshal(after)
			if string(beforeJSON) != string(afterJSON) {
				t.Fatalf("failed operation changed mailbox\nbefore=%s\nafter=%s", beforeJSON, afterJSON)
			}
			var count int
			if err := db.DB().QueryRow("SELECT COUNT(*) FROM mailboxes").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("mailboxes=%d", count)
			}
			if err := db.DB().QueryRow("SELECT COUNT(*) FROM emails WHERE mailbox_id=?", id).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("emails after rollback=%d", count)
			}
		})
	}
}

func TestMailboxAllowedDomainsAtomicInsert(t *testing.T) {
	svc, db, _ := newMailboxTestService(t)
	// INSERT 时必须已经包含 allowed_domains；先创建再 UPDATE 将被触发器拒绝。
	if _, err := db.DB().Exec(`CREATE TRIGGER require_domains BEFORE INSERT ON mailboxes
 WHEN NEW.allowed_domains != '["one.test","two.test"]' BEGIN SELECT RAISE(ABORT,'domains missing at insert'); END`); err != nil {
		t.Fatal(err)
	}
	result, err := svc.CreateMailbox(context.Background(), admin.CreateOptions{Address: "atomic@example.com", AllowedDomains: []string{"one.test", "two.test"}}, admin.Actor{})
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := db.DB().QueryRow("SELECT allowed_domains FROM mailboxes WHERE id=?", result["id"]).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != `["one.test","two.test"]` {
		t.Fatalf("allowed_domains=%s", raw)
	}
}

func TestMailboxConcurrentUpdateAuditChain(t *testing.T) {
	svc, db, _ := newMailboxTestService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	created, err := svc.CreateMailbox(ctx, admin.CreateOptions{Address: "parallel@example.com"}, admin.Actor{})
	if err != nil {
		t.Fatal(err)
	}
	id := created["id"].(string)
	var wg sync.WaitGroup
	errorsCh := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(days int) {
			defer wg.Done()
			errorsCh <- svc.UpdateMailbox(ctx, id, map[string]any{"retention_days": days}, admin.Actor{})
		}(i + 1)
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.DB().Query(`SELECT changes FROM audit_logs WHERE action='UPDATE' ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	previous := float64(30)
	count := 0
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var changes map[string]map[string]float64
		if err := json.Unmarshal([]byte(raw), &changes); err != nil {
			t.Fatal(err)
		}
		change := changes["retention_days"]
		if change["old"] != previous {
			t.Errorf("audit old=%v preceding new=%v", change["old"], previous)
		}
		previous = change["new"]
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if count != 12 {
		t.Fatalf("update audits=%d", count)
	}
	var days int
	if err := db.DB().QueryRow("SELECT retention_days FROM mailboxes WHERE id=?", id).Scan(&days); err != nil {
		t.Fatal(err)
	}
	if float64(days) != previous {
		t.Fatalf("final days=%d audit new=%v", days, previous)
	}
}

func TestMailboxHardDeleteForeignKeys(t *testing.T) {
	svc, db, _ := newMailboxTestService(t)
	ctx := context.Background()
	created, err := svc.CreateMailbox(ctx, admin.CreateOptions{Address: "remove@example.com"}, admin.Actor{})
	if err != nil {
		t.Fatal(err)
	}
	id := created["id"].(string)
	var enabled int
	if err := db.DB().QueryRow("PRAGMA foreign_keys").Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("foreign_keys=%d err=%v", enabled, err)
	}
	if err := svc.DeleteMailbox(ctx, id, false, admin.Actor{}); err != nil {
		t.Fatal(err)
	}
	var mailboxID sql.NullString
	var raw string
	if err := db.DB().QueryRow("SELECT mailbox_id,changes FROM audit_logs WHERE action='HARD_DELETE'").Scan(&mailboxID, &raw); err != nil {
		t.Fatal(err)
	}
	if mailboxID.Valid {
		t.Fatalf("audit mailbox id=%q", mailboxID.String)
	}
	var changes map[string]any
	if err := json.Unmarshal([]byte(raw), &changes); err != nil {
		t.Fatal(err)
	}
	if changes["mailbox_id"] != id || changes["address"] != "remove@example.com" {
		t.Fatalf("changes=%v", changes)
	}
	rows, err := db.DB().Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign key violation")
	}
}

func TestMailboxValidationRules(t *testing.T) {
	svc, _, cfg := newMailboxTestService(t)
	tests := []struct{ name, address, want string }{
		{"empty", "", "邮箱地址不能为空"}, {"invalid", "user@localhost", "邮箱地址格式不正确"},
		{"protected", "admin123@example.com", "该邮箱地址受保护，无法创建"},
		{"case-sensitive", "Admin123@example.com", ""}, {"valid", "any.name+tag@example.com", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := svc.ValidateAddress(tt.address)
			message := ""
			if err != nil {
				message = err.Error()
			}
			if message != tt.want {
				t.Fatalf("message=%q want=%q", message, tt.want)
			}
		})
	}
	for _, tt := range []struct {
		value any
		want  string
	}{
		{"sender@example.com", "白名单必须是列表格式"}, {[]string{"@bad"}, "域名格式不正确: @bad"},
		{[]string{"admin@example.com"}, "邮箱地址格式不正确: admin@example.com"},
		{[]string{"", "@example.com", "sender@example.net"}, ""},
	} {
		t.Run(fmt.Sprint(tt.value), func(t *testing.T) {
			err := svc.ValidateSenderWhitelist(tt.value)
			message := ""
			if err != nil {
				message = err.Error()
			}
			if message != tt.want {
				t.Fatalf("message=%q want=%q", message, tt.want)
			}
		})
	}
	cfg.ProtectedAddresses = "owner"
	custom := admin.New(cfg, nil, nil)
	if custom.ValidateAddress("someowner@example.com") != nil {
		t.Fatal("unanchored Python re.match must only match at offset zero")
	}
	if custom.ValidateAddress("owner@example.com") == nil {
		t.Fatal("custom protected regex not applied")
	}
	cfg.ProtectedAddresses = "["
	if admin.New(cfg, nil, nil).ValidateAddress("owner@example.com") == nil {
		t.Fatal("invalid protected regex must fail closed")
	}
}

func TestMailboxPythonNumericEdges(t *testing.T) {
	svc, _, _ := newMailboxTestService(t)
	ctx := context.Background()
	item, err := svc.CreateMailbox(ctx, admin.CreateOptions{Address: "fraction@example.com"}, admin.Actor{})
	if err != nil {
		t.Fatal(err)
	}
	id := item["id"].(string)
	if err := svc.UpdateMailbox(ctx, id, map[string]any{"retention_days": 1.000001}, admin.Actor{}); err != nil {
		t.Fatal(err)
	}
	detail, err := svc.GetMailboxDetail(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if detail["is_expired"] != false {
		t.Fatalf("fractional expiry=%v", detail)
	}
	list, err := svc.ListMailboxes(ctx, admin.ListOptions{Page: 1, PageSize: 20})
	if err != nil || list.Total != 1 {
		t.Fatalf("list=%v err=%v", list, err)
	}
	if _, err := svc.CreateMailbox(ctx, admin.CreateOptions{Address: "fraction@example.com"}, admin.Actor{}); err == nil || err.Error() != "邮箱地址已存在" {
		t.Fatalf("fractional existing=%v", err)
	}
	for _, tt := range []struct {
		value any
		want  int
		valid bool
	}{
		{"  +1_2  ", 12, true}, {"１２", 12, true}, {"١٢", 12, true}, {true, 1, true}, {3.9, 3, true},
		{"_12", 0, false}, {"12_", 0, false}, {"1__2", 0, false}, {"1.2", 0, false}, {nil, 0, false}, {1e100, 0, false},
	} {
		t.Run(fmt.Sprint(tt.value), func(t *testing.T) {
			n, err := admin.Integer(tt.value)
			if (err == nil) != tt.valid || tt.valid && n != tt.want {
				t.Fatalf("integer(%v)=%d,%v", tt.value, n, err)
			}
		})
	}
}
