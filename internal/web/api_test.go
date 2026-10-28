package web_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"maildrop/internal/config"
	"maildrop/internal/inbox"
	"maildrop/internal/ipguard"
	"maildrop/internal/store"
	"maildrop/internal/web"
)

type publicFixture struct {
	cfg     *config.Config
	db      *store.Store
	blocker *ipguard.Blocker
	service *inbox.Service
	handler http.Handler
	now     time.Time
}

func newPublicFixture(t *testing.T) *publicFixture {
	t.Helper()
	cfg := config.FromEnv(func(key string) string {
		if key == "PASSWORD" {
			return "test-admin-password"
		}
		if key == "DOMAIN" {
			return "stale-config.example"
		}
		return ""
	})
	db, err := store.OpenWithConfig(filepath.Join(t.TempDir(), "public.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	f := &publicFixture{cfg: cfg, db: db, now: time.Unix(1800000000, 0)}
	clock := func() time.Time { return f.now }
	f.blocker = ipguard.NewBlockerWithClock(300, 3, 60, clock)
	f.service = inbox.New(cfg, db, clock)
	f.resetHandler(ipguard.NewWhitelist(false, ""))
	if _, err := db.AddDomain("example.com"); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *publicFixture) resetHandler(whitelist *ipguard.Whitelist) {
	f.handler = web.New(web.Deps{Config: f.cfg, Store: f.db, Blocker: f.blocker, Whitelist: whitelist,
		Frontend: fstest.MapFS{"templates/mailbox_login.html": &fstest.MapFile{Data: []byte("<html>login</html>")}},
		Now:      func() time.Time { return f.now },
	})
}

func (f *publicFixture) request(t *testing.T, method, path, body string, headers map[string]string, want int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "203.0.113.9:54321"
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, req)
	if response.Code != want {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, response.Code, want, response.Body.String())
	}
	if !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("JSON Content-Type missing: %v", response.Header())
	}
	return response
}

func publicJSON(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func publicBody(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func publicBearer(mailbox *inbox.Mailbox) map[string]string {
	return map[string]string{"Authorization": "Bearer " + mailbox.AccessToken}
}

func (f *publicFixture) mailbox(t *testing.T, address string) *inbox.Mailbox {
	t.Helper()
	days := 7
	mailbox, err := f.service.CreateOrGetMailbox(context.Background(), address, inbox.CreateMailboxOptions{RetentionDays: &days})
	if err != nil {
		t.Fatal(err)
	}
	return mailbox
}

func (f *publicFixture) email(t *testing.T, mailbox *inbox.Mailbox, subject string) string {
	t.Helper()
	id, err := f.db.AddEmail(mailbox.ID, store.Email{From: "sender@example.net", To: "untrusted-header@example.net", Subject: subject,
		Body: "<p>邮件正文</p>", ContentType: "text/html", Timestamp: f.now.Unix(), Sent: "刚刚"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPublicAuthenticationAndReadContracts(t *testing.T) {
	f := newPublicFixture(t)
	owner, other := f.mailbox(t, "owner@example.com"), f.mailbox(t, "other@example.com")
	ownID, otherID := f.email(t, owner, "own"), f.email(t, other, "other")
	expired, disabled := f.mailbox(t, "expired@example.com"), f.mailbox(t, "disabled@example.com")
	if _, err := f.db.DB().Exec("UPDATE mailboxes SET expires_at = ? WHERE id = ?", f.now.Unix()-1, expired.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.UpdateMailboxStatus(context.Background(), disabled.ID, false); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, path, authorization string
		status                    int
		errorText, message        string
	}{
		{"missing", "/api/mailbox_info_v2", "", 401, "Authentication required", "Provide an access token using the Authorization: Bearer header"},
		{"query token ignored", "/api/mailbox_info_v2?token=" + owner.AccessToken, "", 401, "Authentication required", ""},
		{"invalid", "/api/mailbox_info_v2", "Bearer invalid", 401, "Invalid access token", "The provided access token is invalid"},
		{"wrong scheme", "/api/mailbox_info_v2", "Basic " + owner.AccessToken, 401, "Authentication required", ""},
		{"expired", "/api/mailbox_info_v2", "Bearer " + expired.AccessToken, 410, "Mailbox expired", "This mailbox has expired"},
		{"disabled", "/api/mailbox_info_v2", "Bearer " + disabled.AccessToken, 423, "Mailbox disabled", "This mailbox has been disabled"},
		{"wrong address", "/api/get_inbox?address=" + url.QueryEscape(other.Address), "Bearer " + owner.AccessToken, 403, "Permission denied", "The access token does not belong to the requested mailbox"},
		{"cross mail", "/api/get_email?address=" + url.QueryEscape(owner.Address) + "&id=" + otherID, "Bearer " + owner.AccessToken, 403, "Permission denied", "The email does not belong to the authenticated mailbox"},
		{"missing mail", "/api/get_email?address=" + url.QueryEscape(owner.Address) + "&id=missing", "Bearer " + owner.AccessToken, 404, "Email not found", "The requested email does not exist"},
		{"missing address", "/api/get_inbox", "Bearer " + owner.AccessToken, 400, "Address is required", "Mailbox address is required"},
		{"missing detail fields", "/api/get_email", "Bearer " + owner.AccessToken, 400, "Missing address or email ID", "Mailbox address and email ID are required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := publicJSON(t, f.request(t, "GET", tc.path, "", map[string]string{"Authorization": tc.authorization}, tc.status))
			if data["error"] != tc.errorText || tc.message != "" && data["message"] != tc.message {
				t.Fatalf("unexpected response: %v", data)
			}
		})
	}
	info := publicJSON(t, f.request(t, "GET", "/api/mailbox_info_v2", "", publicBearer(owner), 200))
	mailbox := info["mailbox"].(map[string]any)
	if len(mailbox) != 15 || mailbox["admin_disabled"] != false || info["storage_type"] != "database" || mailbox["email_count"] != float64(1) || mailbox["unread_count"] != float64(1) {
		t.Fatalf("info shape: %v", info)
	}
	for _, key := range []string{"access_token", "mailbox_key", "created_by_ip"} {
		if _, found := mailbox[key]; found {
			t.Errorf("secret in info: %s", key)
		}
	}
	response := f.request(t, "GET", "/api/get_inbox?address="+url.QueryEscape(owner.Address), "", publicBearer(owner), 200)
	var messages []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0]["id"] != ownID || len(messages[0]) != 9 {
		t.Fatalf("inbox shape: %v", messages)
	}
	detail := publicJSON(t, f.request(t, "GET", "/api/get_email?address="+url.QueryEscape(owner.Address)+"&id="+ownID, "", publicBearer(owner), 200))
	if detail["is_read"] != true || detail["mailbox_id"] != owner.ID || len(detail) != 10 {
		t.Fatalf("detail shape: %v", detail)
	}
	stored, err := f.db.GetEmailByID(ownID)
	if err != nil || !stored.IsRead {
		t.Fatalf("read not persisted: %v %v", stored, err)
	}
}

func TestPublicMailboxTokenExchange(t *testing.T) {
	f := newPublicFixture(t)
	mailbox := f.mailbox(t, "token@example.com")
	cases := []struct {
		name, body, errorText string
		status                int
	}{
		{"empty", "", "No data provided", 400}, {"array", "[1]", "No data provided", 400},
		{"scalar", "42", "No data provided", 400}, {"null", "null", "No data provided", 400},
		{"invalid json", "{", "No data provided", 400}, {"empty object", "{}", "No data provided", 400},
		{"wrong types", `{"address":123,"mailbox_key":[]}`, "Invalid credential types", 400},
		{"null type", `{"address":null,"mailbox_key":"abcdef"}`, "Invalid credential types", 400},
		{"missing address", `{"mailbox_key":"abcdef"}`, "Address is required", 400},
		{"missing key", `{"address":"token@example.com"}`, "Mailbox key is required", 400},
		{"bad address", `{"address":"invalid","mailbox_key":"abcdef"}`, "Invalid email address format", 400},
		{"missing mailbox", `{"address":"missing@example.com","mailbox_key":"abcdef"}`, "Mailbox not found", 404},
		{"wrong key", `{"address":"token@example.com","mailbox_key":"wrong"}`, "Invalid mailbox key", 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.blocker.UnblockIP("203.0.113.9")
			data := publicJSON(t, f.request(t, "POST", "/api/get_mailbox_token", tc.body, nil, tc.status))
			if data["error"] != tc.errorText {
				t.Fatalf("%v", data)
			}
		})
	}
	exchange := func(key string, status int) map[string]any {
		return publicJSON(t, f.request(t, "POST", "/api/get_mailbox_token", publicBody(map[string]any{"address": "  " + mailbox.Address + " ", "mailbox_key": key}), nil, status))
	}
	data := exchange(mailbox.MailboxKey, 200)
	if len(data) != 6 || data["access_token"] != mailbox.AccessToken || data["mailbox_id"] != mailbox.ID {
		t.Fatalf("exchange: %v", data)
	}
	legacy := "12345678-1234-1234-1234-123456789abc"
	if _, err := f.db.DB().Exec("UPDATE mailboxes SET mailbox_key=? WHERE id=?", legacy, mailbox.ID); err != nil {
		t.Fatal(err)
	}
	exchange(legacy, 200)
	if _, err := f.service.UpdateMailboxStatus(context.Background(), mailbox.ID, false); err != nil {
		t.Fatal(err)
	}
	exchange("wrong", 401)
	exchange(legacy, 423)
	if _, err := f.db.DB().Exec("UPDATE mailboxes SET expires_at=? WHERE id=?", f.now.Unix()-1, mailbox.ID); err != nil {
		t.Fatal(err)
	}
	exchange(legacy, 410)
}

func TestPublicEveryWriteRequiresBearerAndOwnership(t *testing.T) {
	f := newPublicFixture(t)
	owner, other := f.mailbox(t, "writeowner@example.com"), f.mailbox(t, "writeother@example.com")
	ownID, otherID := f.email(t, owner, "owned"), f.email(t, other, "other")
	cases := []struct {
		action string
		body   map[string]any
	}{
		{"mark_email_read", map[string]any{"email_id": otherID, "is_read": true}},
		{"delete_email", map[string]any{"email_id": otherID}},
		{"delete_emails_batch", map[string]any{"email_ids": []string{otherID}}},
		{"mark_all_read", map[string]any{"address": other.Address}},
		{"add_sender_whitelist", map[string]any{"address": other.Address, "sender": "@example.net"}},
		{"remove_sender_whitelist", map[string]any{"address": other.Address, "sender": "@example.net"}},
		{"update_retention", map[string]any{"address": other.Address, "retention_days": 8}},
		{"regenerate_mailbox_key", map[string]any{"address": other.Address, "current_key": other.MailboxKey}},
		{"toggle_mailbox_status", map[string]any{"address": other.Address}},
		{"toggle_whitelist", map[string]any{"address": other.Address, "enabled": true}},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			f.request(t, "POST", "/api/"+tc.action, publicBody(tc.body), nil, 401)
			f.request(t, "POST", "/api/"+tc.action, publicBody(tc.body), map[string]string{"Authorization": "Bearer invalid"}, 401)
			f.request(t, "POST", "/api/"+tc.action, publicBody(tc.body), publicBearer(owner), 403)
		})
	}
	f.request(t, "POST", "/api/delete_emails_batch", publicBody(map[string]any{"email_ids": []string{ownID, otherID}}), publicBearer(owner), 403)
	if _, err := f.db.GetEmailByID(ownID); err != nil {
		t.Fatalf("batch partially deleted: %v", err)
	}
}

func TestPublicEmailMutations(t *testing.T) {
	f := newPublicFixture(t)
	owner := f.mailbox(t, "mutate@example.com")
	first, second, third := f.email(t, owner, "first"), f.email(t, owner, "second"), f.email(t, owner, "third")
	for _, read := range []bool{true, false} {
		data := publicJSON(t, f.request(t, "POST", "/api/mark_email_read", publicBody(map[string]any{"email_id": first, "is_read": read}), publicBearer(owner), 200))
		if data["message"] != "Email status updated" {
			t.Fatal(data)
		}
		email, err := f.db.GetEmailByID(first)
		if err != nil || email.IsRead != read {
			t.Fatalf("read=%v email=%v err=%v", read, email, err)
		}
	}
	body := publicBody(map[string]any{"address": owner.Address})
	for _, want := range []string{"Marked 3 emails as read", "Marked 0 emails as read"} {
		data := publicJSON(t, f.request(t, "POST", "/api/mark_all_read", body, publicBearer(owner), 200))
		if data["message"] != want {
			t.Fatalf("%v", data)
		}
	}
	data := publicJSON(t, f.request(t, "POST", "/api/delete_email", publicBody(map[string]any{"email_id": first}), publicBearer(owner), 200))
	if data["message"] != "Email deleted" {
		t.Fatal(data)
	}
	f.request(t, "POST", "/api/delete_email", publicBody(map[string]any{"email_id": first}), publicBearer(owner), 404)
	data = publicJSON(t, f.request(t, "POST", "/api/delete_emails_batch", publicBody(map[string]any{"email_ids": []string{second, third, third}}), publicBearer(owner), 200))
	if data["message"] != "Deleted 2 emails" {
		t.Fatal(data)
	}
	stats, err := f.db.GetMailboxStats(owner.ID)
	if err != nil || stats.TotalEmails != 0 || stats.StorageUsed != 0 {
		t.Fatalf("delete accounting: %v %v", stats, err)
	}
	empty := f.request(t, "GET", "/api/get_inbox?address="+url.QueryEscape(owner.Address), "", publicBearer(owner), 200)
	if strings.TrimSpace(empty.Body.String()) != "[]" {
		t.Fatal(empty.Body.String())
	}
}

func TestPublicMailboxSettings(t *testing.T) {
	f := newPublicFixture(t)
	owner := f.mailbox(t, "settings@example.com")
	body := map[string]any{"address": owner.Address, "sender": "@example.net"}
	for _, action := range []string{"add_sender_whitelist", "add_sender_whitelist"} {
		f.request(t, "POST", "/api/"+action, publicBody(body), publicBearer(owner), 200)
	}
	stored, _ := f.db.GetMailboxByAddress(owner.Address)
	if !reflect.DeepEqual(stored.SenderWhitelist, []string{"@example.net"}) || stored.WhitelistEnabled {
		t.Fatalf("add: %v", stored)
	}
	for _, enabled := range []bool{true, false} {
		data := publicJSON(t, f.request(t, "POST", "/api/toggle_whitelist", publicBody(map[string]any{"address": owner.Address, "enabled": enabled}), publicBearer(owner), 200))
		stored, _ = f.db.GetMailboxByAddress(owner.Address)
		if data["whitelist_enabled"] != enabled || stored.WhitelistEnabled != enabled {
			t.Fatal(data, stored)
		}
	}
	f.request(t, "POST", "/api/remove_sender_whitelist", publicBody(body), publicBearer(owner), 200)
	stored, _ = f.db.GetMailboxByAddress(owner.Address)
	if len(stored.SenderWhitelist) != 0 {
		t.Fatal(stored.SenderWhitelist)
	}
	for _, days := range []any{0, 366, "7", 7.5, nil, false} {
		f.request(t, "POST", "/api/update_retention", publicBody(map[string]any{"address": owner.Address, "retention_days": days}), publicBearer(owner), 400)
	}
	f.request(t, "POST", "/api/update_retention", publicBody(map[string]any{"address": owner.Address, "retention_days": 8}), publicBearer(owner), 200)
	stored, _ = f.db.GetMailboxByAddress(owner.Address)
	if stored.RetentionDays != 8 || stored.ExpiresAt != f.now.Unix()+8*86400 {
		t.Fatal(stored)
	}
	f.request(t, "POST", "/api/regenerate_mailbox_key", publicBody(map[string]any{"address": owner.Address, "current_key": "wrong"}), publicBearer(owner), 400)
	data := publicJSON(t, f.request(t, "POST", "/api/regenerate_mailbox_key", publicBody(map[string]any{"address": owner.Address, "current_key": owner.MailboxKey}), publicBearer(owner), 200))
	key := data["new_key"].(string)
	publicAssertKey(t, key)
	stored, _ = f.db.GetMailboxByAddress(owner.Address)
	if stored.MailboxKey != key || stored.AccessToken != owner.AccessToken {
		t.Fatalf("rotation: %v", stored)
	}
	f.request(t, "POST", "/api/regenerate_mailbox_key", publicBody(map[string]any{"address": owner.Address, "current_key": owner.MailboxKey}), publicBearer(owner), 400)
	for _, active := range []bool{false, true} {
		data = publicJSON(t, f.request(t, "POST", "/api/toggle_mailbox_status", publicBody(map[string]any{"address": owner.Address}), publicBearer(owner), 200))
		if data["is_active"] != active {
			t.Fatal(data)
		}
		want := 423
		if active {
			want = 200
		}
		f.request(t, "GET", "/api/get_inbox?address="+url.QueryEscape(owner.Address), "", publicBearer(owner), want)
		if !active {
			f.request(t, "POST", "/api/update_retention", publicBody(map[string]any{"address": owner.Address, "retention_days": 8}), publicBearer(owner), 423)
		}
	}
}

func publicAssertKey(t *testing.T, key string) {
	t.Helper()
	upper, lower, digit := false, false, false
	for _, ch := range key {
		switch {
		case ch >= 'A' && ch <= 'Z':
			upper = true
		case ch >= 'a' && ch <= 'z':
			lower = true
		case ch >= '0' && ch <= '9':
			digit = true
		default:
			t.Fatalf("key has invalid characters")
		}
	}
	if len(key) != 10 || !upper || !lower || !digit {
		t.Fatal("key violates character-class contract")
	}
}

func TestPublicAdminLoginAndSharedBlocker(t *testing.T) {
	f := newPublicFixture(t)
	good := publicBody(map[string]any{"password": f.cfg.Password})
	data := publicJSON(t, f.request(t, "POST", "/api/admin_login", good, nil, 200))
	if data["success"] != true || data["message"] != "Login successful" {
		t.Fatal(data)
	}
	for i := 0; i < 3; i++ {
		want := 401
		if i == 2 {
			want = 403
		}
		data = publicJSON(t, f.request(t, "POST", "/api/admin_login", `{"password":"wrong"}`, map[string]string{"X-Forwarded-For": fmt.Sprintf("198.51.100.%d", i)}, want))
		if data["success"] != false {
			t.Fatal(data)
		}
		if i < 2 && data["message"] != "Invalid password" {
			t.Fatal(data)
		}
	}
	if !f.blocker.IsBlocked("203.0.113.9") || f.blocker.IsBlocked("198.51.100.0") {
		t.Fatal("spoofed XFF influenced blocker")
	}
	data = publicJSON(t, f.request(t, "POST", "/api/admin_login", good, nil, 403))
	if !strings.Contains(data["message"].(string), "剩余 300 秒") {
		t.Fatal(data)
	}
	f.request(t, "POST", "/api/register", `{"email":"blocked"}`, map[string]string{"Authorization": f.cfg.Password}, 403)
	f.blocker.UnblockIP("203.0.113.9")
	f.cfg.Password = ""
	data = publicJSON(t, f.request(t, "POST", "/api/admin_login", `{"password":""}`, nil, 503))
	if data["message"] != "Admin password is not configured" || data["success"] != false {
		t.Fatal(data)
	}
	f.request(t, "POST", "/api/register", `{"email":"blocked"}`, map[string]string{"Authorization": "anything"}, 503)
}

func TestPublicLiveDatabaseDomainsAndRegistration(t *testing.T) {
	f := newPublicFixture(t)
	random := publicJSON(t, f.request(t, "GET", "/api/get_random_address", "", nil, 200))
	address := random["address"].(string)
	parts := strings.Split(address, "@")
	if len(parts) != 2 || len(parts[0]) != 16 || parts[1] != "example.com" || !reflect.DeepEqual(random["available_domains"], []any{"example.com"}) {
		t.Fatal(random)
	}
	for _, ch := range parts[0] {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9') {
			t.Fatalf("invalid random prefix: %q", parts[0])
		}
	}
	if _, err := f.db.AddDomain("new.example"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetDomainEnabled("example.com", false); err != nil {
		t.Fatal(err)
	}
	random = publicJSON(t, f.request(t, "GET", "/api/get_random_address", "", nil, 200))
	if !strings.HasSuffix(random["address"].(string), "@new.example") || !reflect.DeepEqual(random["available_domains"], []any{"new.example"}) {
		t.Fatal(random)
	}
	domain := publicJSON(t, f.request(t, "GET", "/api/get_domain", "", nil, 200))
	if domain["domain"] != "new.example" {
		t.Fatal(domain)
	}
	headers := map[string]string{"Authorization": f.cfg.Password}
	data := publicJSON(t, f.request(t, "POST", "/api/register", `{"email":"fresh","retention_days":7}`, headers, 201))
	if len(data) != 9 || data["mailbox_address"] != "fresh@new.example" || data["success"] != true || data["mailbox_created"] != true {
		t.Fatal(data)
	}
	publicAssertKey(t, data["mailbox_key"].(string))
	stored, err := f.db.GetMailboxByAddress("fresh@new.example")
	if err != nil || stored.CreatedSource != "register" || stored.CreatedByIP == nil || *stored.CreatedByIP != "203.0.113.9" || stored.RetentionDays != 7 || stored.ExpiresAt != f.now.Unix()+7*86400 {
		t.Fatalf("creation: %v %v", stored, err)
	}
	conflict := publicJSON(t, f.request(t, "POST", "/api/register", `{"email":"fresh","retention_days":7}`, headers, 409))
	if len(conflict["existing_mailbox"].(map[string]any)) != 4 {
		t.Fatal(conflict)
	}
	for _, email := range []string{"disabled@example.com", "outside@elsewhere.example", "stale@stale-config.example"} {
		data = publicJSON(t, f.request(t, "POST", "/api/register", publicBody(map[string]any{"email": email, "retention_days": 7}), headers, 403))
		if data["error"] != "Domain not allowed" {
			t.Fatal(data)
		}
	}
	full := publicJSON(t, f.request(t, "POST", "/api/register", `{"email":"explicit@new.example","retention_days":365}`, headers, 201))
	if full["mailbox_address"] != "explicit@new.example" {
		t.Fatal(full)
	}
	f.request(t, "POST", "/api/register", `{"email":"defaultdays"}`, headers, 201)
	// 仅测试临时库的空启用域名状态；生产 CRUD 会保护最后一个启用域名。
	if _, err := f.db.DB().Exec("UPDATE domains SET enabled=0"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, body string
		headers            map[string]string
	}{
		{"GET", "/api/get_random_address", "", nil}, {"GET", "/api/get_domain", "", nil},
		{"POST", "/api/register", `{"email":"none","retention_days":7}`, headers},
		{"POST", "/api/register", `{"email":"none@new.example","retention_days":7}`, headers},
	} {
		data = publicJSON(t, f.request(t, tc.method, tc.path, tc.body, tc.headers, 503))
		if !reflect.DeepEqual(data, map[string]any{"error": "No domain configured", "message": "请先在管理后台添加并启用域名"}) {
			t.Fatal(data)
		}
	}
}

func TestPublicSubAdminRegistrationDomainsAndRetention(t *testing.T) {
	f := newPublicFixture(t)
	// 子管理员授权域名：已启用域名、不在域名表中的自有域名、已被管理员禁用的域名。
	if _, err := f.db.AddDomain("second.example"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.AddDomain("blocked.example"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetDomainEnabled("blocked.example", false); err != nil {
		t.Fatal(err)
	}
	max := 10
	_, err := f.db.CreateSubAdmin("sub-token", []string{"example.com", "own.example", "blocked.example"}, store.CreateSubAdminOptions{SenderWhitelist: []string{"@trusted.example"}, MaxRetentionDays: &max})
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"X-Sub-Admin-Token": "sub-token"}
	data := publicJSON(t, f.request(t, "POST", "/api/register_with_token", `{"email":"subbox@Example.COM","retention_days":7}`, headers, 201))
	if data["mailbox_address"] != "subbox@example.com" {
		t.Fatal(data)
	}
	publicAssertKey(t, data["mailbox_key"].(string))
	stored, err := f.db.GetMailboxByAddress("subbox@example.com")
	if err != nil || stored.CreatedSource != "sub_admin_token" || !reflect.DeepEqual(stored.SenderWhitelist, []string{"@trusted.example"}) || !stored.WhitelistEnabled {
		t.Fatalf("sub mailbox: %v %v", stored, err)
	}
	conflict := publicJSON(t, f.request(t, "POST", "/api/register_with_token", `{"email":"subbox@example.com","retention_days":7}`, headers, 409))
	if len(conflict["existing_mailbox"].(map[string]any)) != 3 {
		t.Fatal(conflict)
	}
	// 不在域名表中的授权域名沿用 Python 语义可用。
	f.request(t, "POST", "/api/register_with_token", `{"email":"fullsub@own.example","retention_days":7}`, headers, 201)
	// 随机域名只会落在可用域名上，绝不选到已禁用的 blocked.example。
	for i := 0; i < 20; i++ {
		data = publicJSON(t, f.request(t, "POST", "/api/register_with_token", publicBody(map[string]any{"email": fmt.Sprintf("rand%02d", i), "retention_days": 7}), headers, 201))
		if address := data["mailbox_address"].(string); strings.HasSuffix(address, "@blocked.example") {
			t.Fatalf("disabled domain chosen: %s", address)
		}
	}
	for _, email := range []string{"outside@second.example", "disabled@blocked.example"} {
		f.request(t, "POST", "/api/register_with_token", publicBody(map[string]any{"email": email, "retention_days": 7}), headers, 403)
	}
	data = publicJSON(t, f.request(t, "POST", "/api/register_with_token", `{"email":"limited","retention_days":11}`, headers, 400))
	if data["error"] != "Retention days exceeds limit" || data["message"] != "保留天数不能超过 10 天" {
		t.Fatal(data)
	}
	if err := f.db.SetDomainEnabled("example.com", false); err != nil {
		t.Fatal(err)
	}
	data = publicJSON(t, f.request(t, "POST", "/api/register_with_token", `{"email":"noallowed@example.com","retention_days":7}`, headers, 403))
	if data["error"] != "Domain not allowed" {
		t.Fatal(data)
	}
	// 授权域名全部被禁用时，前缀注册也被拒绝。
	if _, err := f.db.DB().Exec("UPDATE domains SET enabled=0"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.DB().Exec("INSERT INTO domains (domain, enabled, created_at, updated_at) VALUES ('own.example', 0, 0, 0)"); err != nil {
		t.Fatal(err)
	}
	data = publicJSON(t, f.request(t, "POST", "/api/register_with_token", `{"email":"nodomains","retention_days":7}`, headers, 403))
	if data["error"] != "Domain not allowed" {
		t.Fatal(data)
	}
}

func TestPublicRegistrationValidationAndAuth(t *testing.T) {
	f := newPublicFixture(t)
	good := map[string]string{"Authorization": f.cfg.Password}
	cases := []struct{ body, errorText string }{
		{`[]`, "No data provided"}, {`null`, "No data provided"}, {`42`, "No data provided"}, {`{}`, "No data provided"},
		{`{"email":42}`, "Email address or prefix must be a string"}, {`{"email":null}`, "Email address or prefix must be a string"},
		{`{"retention_days":7}`, "Email address or prefix is required"}, {`{"email":"  "}`, "Email address or prefix is required"},
		{`{"email":"ab"}`, "Email prefix must be 3-20 characters, letters, numbers, and underscores only"},
		{`{"email":"<img_src_x>@example.com"}`, "Invalid email address format"}, {`{"email":"safe@<img>.com"}`, "Invalid email address format"},
		{`{"email":"abc@@example.com"}`, "Invalid email address format"},
		{`{"email":"abc","retention_days":0}`, "Retention days must be between 1 and 365"},
		{`{"email":"abc","retention_days":366}`, "Retention days must be between 1 and 365"},
		{`{"email":"abc","retention_days":"7"}`, "Retention days must be between 1 and 365"},
		{`{"email":"abc","retention_days":7.0}`, "Retention days must be between 1 and 365"},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			data := publicJSON(t, f.request(t, "POST", "/api/register", tc.body, good, 400))
			if data["error"] != tc.errorText {
				t.Fatal(data)
			}
		})
	}
	// Python bool/int 兼容性固定在 API 边界。
	f.request(t, "POST", "/api/register", `{"email":"booldays","retention_days":true}`, good, 201)
	for _, tc := range []struct {
		path    string
		headers map[string]string
	}{
		{"/api/register", nil}, {"/api/register", map[string]string{"Authorization": "wrong"}},
		{"/api/register_with_token", nil}, {"/api/register_with_token", map[string]string{"X-Sub-Admin-Token": "wrong"}},
	} {
		f.blocker.UnblockIP("203.0.113.9")
		f.request(t, "POST", tc.path, `{"email":"authbox"}`, tc.headers, 401)
		// 缺失凭据与错误凭据都计数，第三次失败即封禁。
		f.request(t, "POST", tc.path, `{"email":"authbox"}`, tc.headers, 401)
		third := 401
		if len(tc.headers) > 0 {
			third = 403
		}
		f.request(t, "POST", tc.path, `{"email":"authbox"}`, tc.headers, third)
		if !f.blocker.IsBlocked("203.0.113.9") {
			t.Fatal("credential failures were not counted")
		}
		f.request(t, "POST", tc.path, `{"email":"authbox"}`, tc.headers, 403)
	}
}

func TestPublicWhitelistAllNineteenRoutes(t *testing.T) {
	f := newPublicFixture(t)
	if _, err := f.db.CreateSubAdmin("sub-token", []string{"example.com"}, store.CreateSubAdminOptions{}); err != nil {
		t.Fatal(err)
	}
	routes := []struct {
		method, path string
		detailed     bool
	}{
		{"GET", "get_random_address", false}, {"POST", "get_mailbox_token", true}, {"POST", "register_with_token", false},
		{"POST", "register", false}, {"GET", "mailbox_info_v2", true}, {"GET", "get_domain", false}, {"GET", "get_inbox", true}, {"GET", "get_email", true},
		{"POST", "admin_login", false}, {"POST", "mark_email_read", false}, {"POST", "delete_email", false}, {"POST", "delete_emails_batch", false},
		{"POST", "mark_all_read", false}, {"POST", "add_sender_whitelist", false}, {"POST", "remove_sender_whitelist", false},
		{"POST", "update_retention", false}, {"POST", "regenerate_mailbox_key", false}, {"POST", "toggle_mailbox_status", false}, {"POST", "toggle_whitelist", false},
	}
	f.resetHandler(ipguard.NewWhitelist(true, "198.51.100.0/24"))
	for _, tc := range routes {
		t.Run(tc.path, func(t *testing.T) {
			data := publicJSON(t, f.request(t, tc.method, "/api/"+tc.path, `{"anything":true}`, map[string]string{
				"Authorization": f.cfg.Password, "X-Sub-Admin-Token": "sub-token", "X-Forwarded-For": "198.51.100.1",
			}, 403))
			if tc.path == "admin_login" {
				if data["message"] != "IP not whitelisted" || data["success"] != false {
					t.Fatal(data)
				}
				return
			}
			if data["error"] != "Access denied - IP not whitelisted" {
				t.Fatal(data)
			}
			if tc.detailed && data["message"] != "Your IP address is not allowed" {
				t.Fatal(data)
			}
			if !tc.detailed && len(data) != 1 {
				t.Fatal(data)
			}
		})
	}
	f.cfg.TrustProxyHops = 1
	f.resetHandler(ipguard.NewWhitelist(true, "198.51.100.0/24"))
	f.request(t, "GET", "/api/get_domain", "", map[string]string{"X-Forwarded-For": "198.51.100.1"}, 200)
}

func TestPublicInvalidMutationBodiesAndBodyLimit(t *testing.T) {
	f := newPublicFixture(t)
	owner := f.mailbox(t, "validation@example.com")
	actions := []string{"mark_email_read", "delete_email", "delete_emails_batch", "mark_all_read", "add_sender_whitelist", "remove_sender_whitelist", "update_retention", "regenerate_mailbox_key", "toggle_mailbox_status", "toggle_whitelist"}
	for _, action := range actions {
		t.Run(action, func(t *testing.T) {
			for _, body := range []string{"", `[]`, `[1]`, `null`, `42`, `"text"`, `{}`, `{`} {
				data := publicJSON(t, f.request(t, "POST", "/api/"+action, body, publicBearer(owner), 400))
				if data["error"] != "No data provided" {
					t.Fatal(data)
				}
			}
			f.request(t, "POST", "/api/"+action, `{"other":1}`, publicBearer(owner), 400)
		})
	}
	f.request(t, "POST", "/api/delete_emails_batch", `{"email_ids":"not-an-array"}`, publicBearer(owner), 400)
	f.request(t, "POST", "/api/delete_emails_batch", `{"email_ids":[null]}`, publicBearer(owner), 400)
	f.request(t, "POST", "/api/add_sender_whitelist", `{"address":"validation@example.com","sender":{}}`, publicBearer(owner), 400)
	huge := `{"password":"` + strings.Repeat("a", 1<<20) + `"}`
	for _, path := range []string{"get_mailbox_token", "admin_login", "register", "mark_email_read"} {
		headers := publicBearer(owner)
		if path == "register" {
			headers = map[string]string{"Authorization": f.cfg.Password}
		}
		f.request(t, "POST", "/api/"+path, huge, headers, 413)
	}
}

func TestPublicExpiredRecreationAndDisabledDatabase(t *testing.T) {
	f := newPublicFixture(t)
	expired := f.mailbox(t, "recreated@example.com")
	oldID := f.email(t, expired, "old")
	if _, err := f.db.DB().Exec("UPDATE mailboxes SET expires_at=? WHERE id=?", f.now.Unix()-1, expired.ID); err != nil {
		t.Fatal(err)
	}
	data := publicJSON(t, f.request(t, "POST", "/api/register", `{"email":"recreated@example.com","retention_days":7}`, map[string]string{"Authorization": f.cfg.Password}, 201))
	stored, err := f.db.GetMailboxByAddress(expired.Address)
	if err != nil || stored.ID == expired.ID || stored.AccessToken == expired.AccessToken || data["mailbox_key"] == expired.MailboxKey {
		t.Fatalf("recreated: %v %v", stored, err)
	}
	if _, err := f.db.GetEmailByID(oldID); err != store.ErrNotFound {
		t.Fatalf("old mail retained: %v", err)
	}
	f.cfg.UseDatabase = false
	f.request(t, "GET", "/api/mailbox_info_v2", "", publicBearer(expired), 400)
	f.request(t, "POST", "/api/get_mailbox_token", publicBody(map[string]any{"address": stored.Address, "mailbox_key": stored.MailboxKey}), nil, 400)
	f.request(t, "POST", "/api/register", `{"email":"nodb"}`, map[string]string{"Authorization": f.cfg.Password}, 400)
	f.request(t, "POST", "/api/register_with_token", `{"email":"nodb"}`, map[string]string{"X-Sub-Admin-Token": "anything"}, 500)
	f.request(t, "POST", "/api/mark_email_read", `{"email_id":"anything"}`, publicBearer(expired), 400)
}

func TestPublicConcurrentRegistrationsDoNotDiscloseExistingCredentials(t *testing.T) {
	f := newPublicFixture(t)
	for _, subAdmin := range []bool{false, true} {
		t.Run(fmt.Sprintf("sub_admin=%v", subAdmin), func(t *testing.T) {
			path, authName, authValue, prefix := "/api/register", "Authorization", f.cfg.Password, "concurrent"
			if subAdmin {
				path, authName, authValue, prefix = "/api/register_with_token", "X-Sub-Admin-Token", "concurrent-sub-token", "concurrentsub"
				if _, err := f.db.CreateSubAdmin(authValue, []string{"example.com"}, store.CreateSubAdminOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			const count = 12
			responses := make(chan *httptest.ResponseRecorder, count)
			ready := make(chan struct{})
			for range count {
				go func() {
					<-ready
					request := httptest.NewRequest("POST", path, strings.NewReader(publicBody(map[string]any{"email": prefix, "retention_days": 7})))
					request.RemoteAddr = "203.0.113.9:54321"
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set(authName, authValue)
					response := httptest.NewRecorder()
					f.handler.ServeHTTP(response, request)
					responses <- response
				}()
			}
			close(ready)
			created, conflicts := 0, 0
			for range count {
				response := <-responses
				data := publicJSON(t, response)
				switch response.Code {
				case 201:
					created++
				case 409:
					conflicts++
					for _, secret := range []string{"access_token", "mailbox_key"} {
						if _, found := data[secret]; found {
							t.Fatalf("conflict disclosed %s", secret)
						}
					}
				default:
					t.Fatalf("concurrent registration: %d %s", response.Code, response.Body.String())
				}
			}
			if created != 1 || conflicts != count-1 {
				t.Fatalf("created=%d conflicts=%d", created, conflicts)
			}
		})
	}
}

func TestPublicNullableHistoricalValuesAndExactExpiration(t *testing.T) {
	f := newPublicFixture(t)
	owner := f.mailbox(t, "history@example.com")
	id := f.email(t, owner, "nullable")
	if _, err := f.db.DB().Exec("UPDATE emails SET subject=NULL, body=NULL, content_type=NULL, sent_formatted=NULL WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	detail := publicJSON(t, f.request(t, "GET", "/api/get_email?address="+url.QueryEscape(owner.Address)+"&id="+id, "", publicBearer(owner), 200))
	for _, key := range []string{"Subject", "Body", "ContentType", "Sent"} {
		if detail[key] != nil {
			t.Fatalf("%s not null: %v", key, detail)
		}
	}
	if _, err := f.db.DB().Exec("UPDATE mailboxes SET retention_days=NULL, expires_at=? WHERE id=?", f.now.Unix(), owner.ID); err != nil {
		t.Fatal(err)
	}
	info := publicJSON(t, f.request(t, "GET", "/api/mailbox_info_v2", "", publicBearer(owner), 200))
	if info["mailbox"].(map[string]any)["retention_days"] != nil {
		t.Fatal(info)
	}
	f.now = f.now.Add(time.Second)
	f.request(t, "GET", "/api/mailbox_info_v2", "", publicBearer(owner), 410)
	if _, err := f.db.DB().Exec("UPDATE mailboxes SET access_token=NULL, expires_at=? WHERE id=?", f.now.Unix()+3600, owner.ID); err != nil {
		t.Fatal(err)
	}
	exchange := publicJSON(t, f.request(t, "POST", "/api/get_mailbox_token", publicBody(map[string]any{"address": "\u001c" + owner.Address + "\u001f", "mailbox_key": "\u001d" + owner.MailboxKey + "\u001e"}), nil, 200))
	if token, present := exchange["access_token"]; !present || token != nil {
		t.Fatal(exchange)
	}
}

func TestPublicRemovedRoutesAndMethodPatterns(t *testing.T) {
	f := newPublicFixture(t)
	removed := []string{"login", "mailbox", "mailbox/demo", "api-test", "demo/get_token", "demo/mailbox_info", "demo/emails", "create_mailbox_v2", "user_login", "send_test_email", "migrate_to_database", "export_from_database", "admin/whitelist", "admin/test_ip"}
	for _, suffix := range removed {
		for _, method := range []string{"GET", "POST"} {
			request := httptest.NewRequest(method, "/api/"+suffix, nil)
			response := httptest.NewRecorder()
			f.handler.ServeHTTP(response, request)
			if response.Code != 404 {
				t.Fatalf("%s /api/%s: %d", method, suffix, response.Code)
			}
		}
	}
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/get_random_address"}, {"GET", "/api/get_mailbox_token"}, {"POST", "/api/mailbox_info_v2"},
		{"GET", "/api/register"}, {"GET", "/api/register_with_token"}, {"GET", "/api/admin_login"},
		{"GET", "/api/delete_email"}, {"POST", "/api/get_inbox"}, {"PUT", "/api/toggle_whitelist"},
	} {
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
		if response.Code != 405 {
			t.Fatalf("%s %s: %d", tc.method, tc.path, response.Code)
		}
	}
}

func TestPublicSubAdminEmptyAndDisabledPermissions(t *testing.T) {
	f := newPublicFixture(t)
	id, err := f.db.CreateSubAdmin("empty-token", []string{}, store.CreateSubAdminOptions{})
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"X-Sub-Admin-Token": "empty-token"}
	data := publicJSON(t, f.request(t, "POST", "/api/register_with_token", `{"email":"emptydomains","retention_days":7}`, headers, 403))
	if data["error"] != "Domain not allowed" {
		t.Fatal(data)
	}
	inactive := 0
	if err := f.db.UpdateSubAdmin(id, store.SubAdminUpdate{IsActive: &inactive}); err != nil {
		t.Fatal(err)
	}
	// Python 与 Store 均只查询活跃 token，禁用 token 进入 Invalid token 分支。
	data = publicJSON(t, f.request(t, "POST", "/api/register_with_token", `{"email":"inactive","retention_days":7}`, headers, 401))
	if data["error"] != "Invalid token" {
		t.Fatal(data)
	}
}

func TestPublicMailboxMutationsKeepAuthenticatedIDAfterRecreation(t *testing.T) {
	for _, tc := range []struct {
		action string
		fields map[string]any
	}{
		{"add_sender_whitelist", map[string]any{"sender": "@added.example"}},
		{"remove_sender_whitelist", map[string]any{"sender": "@kept.example"}},
		{"update_retention", map[string]any{"retention_days": 31}},
		{"regenerate_mailbox_key", nil},
		{"toggle_mailbox_status", nil},
		{"toggle_whitelist", map[string]any{"enabled": false}},
	} {
		t.Run(tc.action, func(t *testing.T) {
			f := newPublicFixture(t)
			owner := f.mailbox(t, "reused@example.com")
			body := map[string]any{"address": owner.Address}
			for key, value := range tc.fields {
				body[key] = value
			}
			if tc.action == "regenerate_mailbox_key" {
				body["current_key"] = owner.MailboxKey
			}
			var replacement, before *store.Mailbox
			armed := false
			f.handler = web.New(web.Deps{Config: f.cfg, Store: f.db, Blocker: f.blocker,
				Whitelist: ipguard.NewWhitelist(false, ""), Now: func() time.Time {
					if armed {
						armed = false
						// 认证已读出旧行，写入前管理员重建同名邮箱。
						if _, err := f.db.DB().Exec("DELETE FROM mailboxes WHERE id = ?", owner.ID); err != nil {
							t.Fatal(err)
						}
						fresh, err := f.service.CreateOrGetMailbox(context.Background(), owner.Address,
							inbox.CreateMailboxOptions{SenderWhitelist: []string{"@kept.example"}})
						if err != nil {
							t.Fatal(err)
						}
						// 即使新邮箱恰有相同密钥，也必须按旧 ID 拒绝轮换。
						if _, err := f.db.DB().Exec("UPDATE mailboxes SET mailbox_key = ? WHERE id = ?", owner.MailboxKey, fresh.ID); err != nil {
							t.Fatal(err)
						}
						before, err = f.db.GetMailboxByID(fresh.ID)
						if err != nil {
							t.Fatal(err)
						}
						replacement = before
					}
					return f.now
				}})
			armed = true
			f.request(t, "POST", "/api/"+tc.action, publicBody(body), publicBearer(owner), 400)
			if replacement == nil || replacement.ID == owner.ID {
				t.Fatal("mailbox was not recreated after authentication")
			}
			after, err := f.db.GetMailboxByID(replacement.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("stale authenticated mutation changed replacement: before=%+v after=%+v", before, after)
			}
			f.request(t, "POST", "/api/"+tc.action, publicBody(body), publicBearer(owner), 401)
		})
	}
}

func TestPublicAdminDisabledMailboxCannotResume(t *testing.T) {
	f := newPublicFixture(t)
	owner := f.mailbox(t, "adminpaused@example.com")
	if _, err := f.db.DB().Exec("UPDATE mailboxes SET is_active = 0, admin_disabled = 1 WHERE id = ?", owner.ID); err != nil {
		t.Fatal(err)
	}
	body := publicBody(map[string]any{"address": owner.Address})
	data := publicJSON(t, f.request(t, "POST", "/api/toggle_mailbox_status", body, publicBearer(owner), 423))
	if !reflect.DeepEqual(data, map[string]any{"error": "Mailbox disabled by administrator", "message": "该邮箱已被管理员禁用，请联系管理员"}) {
		t.Fatal(data)
	}
	var active, disabled int
	if err := f.db.DB().QueryRow("SELECT is_active, admin_disabled FROM mailboxes WHERE id = ?", owner.ID).Scan(&active, &disabled); err != nil {
		t.Fatal(err)
	}
	if active != 0 || disabled != 1 {
		t.Fatalf("public toggle changed administrator state: active=%d disabled=%d", active, disabled)
	}
	// 管理员解除禁用后，持有者仍可自行恢复、暂停和再次恢复。
	if _, err := f.db.DB().Exec("UPDATE mailboxes SET admin_disabled = 0 WHERE id = ?", owner.ID); err != nil {
		t.Fatal(err)
	}
	for _, wantActive := range []bool{true, false, true} {
		data = publicJSON(t, f.request(t, "POST", "/api/toggle_mailbox_status", body, publicBearer(owner), 200))
		if data["is_active"] != wantActive {
			t.Fatal(data)
		}
		if err := f.db.DB().QueryRow("SELECT admin_disabled FROM mailboxes WHERE id = ?", owner.ID).Scan(&disabled); err != nil || disabled != 0 {
			t.Fatalf("holder changed admin_disabled=%d err=%v", disabled, err)
		}
	}
	info := publicJSON(t, f.request(t, "GET", "/api/mailbox_info_v2", "", publicBearer(owner), 200))
	if info["mailbox"].(map[string]any)["admin_disabled"] != false {
		t.Fatalf("missing boolean administrator flag: %v", info)
	}
}

func TestPublicRegistrationProtectedAddresses(t *testing.T) {
	for _, token := range []bool{false, true} {
		for _, tc := range []struct {
			name, pattern, input string
			status               int
		}{
			{"full protected", "^admin.*", "admin123@example.com", 400},
			{"prefix protected", "^admin.*", "admin123", 400},
			{"local part full match", "^postmaster$", "postmaster@example.com", 400},
			{"local part random domain", "^postmaster$", "postmaster", 400},
			{"invalid policy full", "(", "allowed@example.com", 500},
			{"invalid policy prefix", "(", "allowed", 500},
			{"nonmatching prefix", "admin", "myadmin", 201},
			{"unprotected domain", "^admin", "allowed@admin.example", 201},
			{"empty policy", "", "admin123", 201},
		} {
			t.Run(fmt.Sprintf("sub_admin=%v/%s", token, tc.name), func(t *testing.T) {
				f := newPublicFixture(t)
				f.cfg.ProtectedAddresses = tc.pattern
				f.resetHandler(ipguard.NewWhitelist(false, ""))
				if _, err := f.db.AddDomain("admin.example"); err != nil {
					t.Fatal(err)
				}
				path, headers := "/api/register", map[string]string{"Authorization": f.cfg.Password}
				if token {
					if _, err := f.db.CreateSubAdmin("protected-sub", []string{"example.com", "admin.example", "own.example"}, store.CreateSubAdminOptions{}); err != nil {
						t.Fatal(err)
					}
					path, headers = "/api/register_with_token", map[string]string{"X-Sub-Admin-Token": "protected-sub"}
				}
				data := publicJSON(t, f.request(t, "POST", path, publicBody(map[string]any{"email": tc.input, "retention_days": 7}), headers, tc.status))
				if tc.status == 201 {
					return
				}
				want := map[string]any{"error": "Address is protected", "message": "该邮箱地址受保护，无法创建"}
				if tc.status == 500 {
					want = map[string]any{"error": "Registration failed", "message": "保护地址正则表达式无效"}
				}
				if !reflect.DeepEqual(data, want) {
					t.Fatal(data)
				}
				var count int
				if err := f.db.DB().QueryRow("SELECT COUNT(*) FROM mailboxes").Scan(&count); err != nil || count != 0 {
					t.Fatalf("rejected registration wrote mailbox: count=%d err=%v", count, err)
				}
			})
		}
	}
}

func TestPublicMailboxTokenFailureThreshold(t *testing.T) {
	for _, tc := range []struct {
		name, body, text, message string
		status                    int
	}{
		// 状态码与原接口一致；每种失败都计入封禁次数。
		{"unknown mailbox", `{"address":"missing@example.com","mailbox_key":"wrong"}`, "Mailbox not found", "Mailbox does not exist", 404},
		{"wrong key", `{"address":"threshold@example.com","mailbox_key":"wrong"}`, "Invalid mailbox key", "Mailbox key is incorrect", 401},
		{"address type", `{"address":123,"mailbox_key":"wrong"}`, "Invalid credential types", "Mailbox address and key must be strings", 400},
		{"key type", `{"address":"threshold@example.com","mailbox_key":[]}`, "Invalid credential types", "Mailbox address and key must be strings", 400},
		{"null key", `{"address":"threshold@example.com","mailbox_key":null}`, "Invalid credential types", "Mailbox address and key must be strings", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublicFixture(t)
			owner := f.mailbox(t, "threshold@example.com")
			for range 2 {
				data := publicJSON(t, f.request(t, "POST", "/api/get_mailbox_token", tc.body, nil, tc.status))
				if !reflect.DeepEqual(data, map[string]any{"error": tc.text, "message": tc.message}) {
					t.Fatal(data)
				}
			}
			data := publicJSON(t, f.request(t, "POST", "/api/get_mailbox_token", tc.body, nil, 403))
			if !reflect.DeepEqual(data, map[string]any{"error": "Too many failed attempts", "message": "认证失败次数过多，IP已被封禁 300 秒"}) || !f.blocker.IsBlocked("203.0.113.9") {
				t.Fatal(data)
			}
			good := publicBody(map[string]any{"address": owner.Address, "mailbox_key": owner.MailboxKey})
			f.now = f.now.Add(time.Second)
			data = publicJSON(t, f.request(t, "POST", "/api/get_mailbox_token", good, nil, 403))
			if !reflect.DeepEqual(data, map[string]any{"error": "IP blocked", "message": "IP已被临时封禁，剩余 299 秒"}) {
				t.Fatal(data)
			}
			// 已封禁请求不延长时间；到期后正确凭据可再次交换。
			f.now = f.now.Add(299 * time.Second)
			f.request(t, "POST", "/api/get_mailbox_token", good, nil, 200)
		})
	}
}

func TestPublicMailboxTokenSharesHistoryWithoutClearingOnSuccess(t *testing.T) {
	f := newPublicFixture(t)
	owner := f.mailbox(t, "sharedhistory@example.com")
	f.request(t, "POST", "/api/get_mailbox_token", publicBody(map[string]any{"address": owner.Address, "mailbox_key": "wrong"}), nil, 401)
	f.request(t, "POST", "/api/get_mailbox_token", publicBody(map[string]any{"address": owner.Address, "mailbox_key": owner.MailboxKey}), nil, 200)
	f.request(t, "POST", "/api/admin_login", `{"password":"wrong"}`, nil, 401)
	f.request(t, "POST", "/api/get_mailbox_token", `{"address":"unknown@example.com","mailbox_key":"wrong"}`, nil, 403)
	f.request(t, "POST", "/api/register", `{"email":"blocked"}`, map[string]string{"Authorization": f.cfg.Password}, 403)
	// 共享封禁先于白名单和请求体校验，沿用 admin_login 顺序。
	f.resetHandler(ipguard.NewWhitelist(true, "198.51.100.0/24"))
	data := publicJSON(t, f.request(t, "POST", "/api/get_mailbox_token", "{", nil, 403))
	if !reflect.DeepEqual(data, map[string]any{"error": "IP blocked", "message": "IP已被临时封禁，剩余 300 秒"}) {
		t.Fatal(data)
	}
}

func TestPublicMailboxStatusHonorsAdministratorDisableAfterAuthentication(t *testing.T) {
	f := newPublicFixture(t)
	owner := f.mailbox(t, "lateadmin@example.com")
	if changed, err := f.service.UpdateMailboxStatus(context.Background(), owner.ID, false); err != nil || !changed {
		t.Fatalf("holder pause changed=%v err=%v", changed, err)
	}
	armed := false
	f.handler = web.New(web.Deps{Config: f.cfg, Store: f.db, Blocker: f.blocker,
		Whitelist: ipguard.NewWhitelist(false, ""), Now: func() time.Time {
			if armed {
				armed = false
				// 认证快照尚未禁用；管理员在状态写入前禁用该行。
				if _, err := f.db.DB().Exec("UPDATE mailboxes SET is_active = 0, admin_disabled = 1 WHERE id = ?", owner.ID); err != nil {
					t.Fatal(err)
				}
			}
			return f.now
		}})
	armed = true
	data := publicJSON(t, f.request(t, "POST", "/api/toggle_mailbox_status", publicBody(map[string]any{"address": owner.Address}), publicBearer(owner), 423))
	if armed || !reflect.DeepEqual(data, map[string]any{"error": "Mailbox disabled by administrator", "message": "该邮箱已被管理员禁用，请联系管理员"}) {
		t.Fatal(data)
	}
	var active, disabled int
	if err := f.db.DB().QueryRow("SELECT is_active, admin_disabled FROM mailboxes WHERE id = ?", owner.ID).Scan(&active, &disabled); err != nil || active != 0 || disabled != 1 {
		t.Fatalf("administrator disable lost: active=%d disabled=%d err=%v", active, disabled, err)
	}
}

func TestPublicMailboxInfoExposesAdministratorFlag(t *testing.T) {
	f := newPublicFixture(t)
	owner := f.mailbox(t, "adminflag@example.com")
	// 标记独立于活动状态，公开暂停不得清除它。
	if _, err := f.db.DB().Exec("UPDATE mailboxes SET admin_disabled = 1 WHERE id = ?", owner.ID); err != nil {
		t.Fatal(err)
	}
	info := publicJSON(t, f.request(t, "GET", "/api/mailbox_info_v2", "", publicBearer(owner), 200))
	if info["mailbox"].(map[string]any)["admin_disabled"] != true {
		t.Fatal(info)
	}
	body := publicBody(map[string]any{"address": owner.Address})
	data := publicJSON(t, f.request(t, "POST", "/api/toggle_mailbox_status", body, publicBearer(owner), 200))
	if data["is_active"] != false {
		t.Fatal(data)
	}
	f.request(t, "POST", "/api/toggle_mailbox_status", body, publicBearer(owner), 423)
}
