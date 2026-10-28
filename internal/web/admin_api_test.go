package web_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"maildrop/internal/config"
	"maildrop/internal/ipguard"
	"maildrop/internal/store"
	"maildrop/internal/web"
)

const adminTestPassword = "test-admin-password"
const adminTestRemote = "203.0.113.10:54321"

type adminTestFixture struct {
	cfg     *config.Config
	store   *store.Store
	blocker *ipguard.Blocker
	now     time.Time
	handler http.Handler
}

func newAdminTestFixture(t *testing.T) *adminTestFixture {
	t.Helper()
	cfg := config.FromEnv(func(key string) string {
		if key == "PASSWORD" {
			return adminTestPassword
		}
		return ""
	})
	s, err := store.OpenWithConfig(filepath.Join(t.TempDir(), "mailbox.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	f := &adminTestFixture{cfg: cfg, store: s, now: time.Unix(1800000000, 0)}
	clock := func() time.Time { return f.now }
	f.blocker = ipguard.NewBlockerWithClock(300, 3, 60, clock)
	f.handler = web.New(web.Deps{
		Config: cfg, Store: s, Blocker: f.blocker,
		Whitelist: ipguard.NewWhitelist(false, ""), Frontend: nil, Now: clock,
	})
	return f
}

func (f *adminTestFixture) raw(t *testing.T, method, path, body, auth, remote, forwarded string, want int) map[string]any {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = remote
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	if forwarded != "" {
		r.Header.Set("X-Forwarded-For", forwarded)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s: status %d, want %d; body=%s", method, path, w.Code, want, w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		// A router-level 404 need not have an application JSON envelope.
		if want == http.StatusNotFound {
			return nil
		}
		t.Fatalf("%s %s: invalid JSON: %v; body=%s", method, path, err, w.Body.String())
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Errorf("%s %s: Content-Type=%q", method, path, w.Header().Get("Content-Type"))
	}
	return result
}

func (f *adminTestFixture) request(t *testing.T, method, path string, payload any, want int) map[string]any {
	t.Helper()
	body := ""
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = string(encoded)
	}
	return f.raw(t, method, path, body, "Bearer "+adminTestPassword, adminTestRemote, "", want)
}

func adminTestObject(t *testing.T, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected JSON object, got %T (%v)", value, value)
	}
	return object
}

func adminTestArray(t *testing.T, value any) []any {
	t.Helper()
	array, ok := value.([]any)
	if !ok {
		t.Fatalf("expected JSON array, got %T (%v)", value, value)
	}
	return array
}

func adminTestEqual(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v (%T), want %#v (%T)", got, got, want, want)
	}
}

func adminTestNumber(t *testing.T, object map[string]any, key string, want int64) {
	t.Helper()
	adminTestEqual(t, object[key], float64(want))
}

func adminTestError(t *testing.T, result map[string]any, want string) {
	t.Helper()
	adminTestEqual(t, result["success"], false)
	if want != "" {
		adminTestEqual(t, result["error"], want)
	} else if message, ok := result["error"].(string); !ok || message == "" {
		t.Errorf("expected nonempty error, got %v", result)
	}
}

func (f *adminTestFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.store.DB().Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *adminTestFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var count int
	if err := f.store.DB().QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func (f *adminTestFixture) seed(t *testing.T, address, source string, created, expires int64, active bool) *store.Mailbox {
	t.Helper()
	m, err := f.store.CreateMailbox(address, store.CreateMailboxOptions{CreatedSource: source})
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, "UPDATE mailboxes SET created_at = ?, expires_at = ?, is_active = ? WHERE id = ?", created, expires, active, m.ID)
	m.CreatedAt, m.ExpiresAt, m.IsActive = created, expires, active
	return m
}

func (f *adminTestFixture) create(t *testing.T, address string, extra map[string]any) map[string]any {
	t.Helper()
	payload := map[string]any{"address": address, "retention_days": 7}
	for key, value := range extra {
		payload[key] = value
	}
	result := f.request(t, "POST", "/api/admin/mailboxes", payload, 200)
	adminTestEqual(t, result["success"], true)
	data := adminTestObject(t, result["data"])
	if id, ok := data["id"].(string); !ok || id == "" {
		t.Fatal("creation omitted mailbox ID")
	}
	return data
}

func (f *adminTestFixture) audit(t *testing.T, path, action string) map[string]any {
	t.Helper()
	result := f.request(t, "GET", path, nil, 200)
	adminTestEqual(t, result["success"], true)
	for _, entry := range adminTestArray(t, result["data"]) {
		log := adminTestObject(t, entry)
		if log["action"] == action {
			adminTestEqual(t, log["admin_user"], "admin")
			adminTestEqual(t, log["ip_address"], "203.0.113.10")
			adminTestNumber(t, log, "timestamp", f.now.Unix())
			return log
		}
	}
	t.Fatalf("missing %s audit in %v", action, result)
	return nil
}

func TestAdminAPIAuthentication(t *testing.T) {
	for _, tc := range []struct{ name, auth, message string }{
		{"missing", "", "缺少Authorization请求头"},
		{"raw password", adminTestPassword, "Authorization 请求头格式错误"},
		{"wrong scheme", "Basic " + adminTestPassword, "Authorization 请求头格式错误"},
		{"empty bearer", "Bearer ", "Authorization 请求头格式错误"},
		{"wrong password", "Bearer wrong-password", "认证失败"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdminTestFixture(t)
			result := f.raw(t, "GET", "/api/admin/mailboxes", "", tc.auth, adminTestRemote, "", 401)
			adminTestError(t, result, tc.message)
			if f.blocker.IsBlocked("203.0.113.10") {
				t.Fatal("one failed attempt prematurely blocked IP")
			}
		})
	}
	t.Run("case insensitive bearer", func(t *testing.T) {
		f := newAdminTestFixture(t)
		result := f.raw(t, "GET", "/api/admin/mailboxes", "", "bEaReR "+adminTestPassword, adminTestRemote, "", 200)
		adminTestEqual(t, result["success"], true)
	})
	for _, auth := range []string{"", "Bearer wrong-password"} {
		t.Run("failures count "+auth, func(t *testing.T) {
			f := newAdminTestFixture(t)
			for attempt := 1; attempt <= 3; attempt++ {
				f.raw(t, "GET", "/api/admin/mailboxes", "", auth, adminTestRemote, "", 401)
				adminTestEqual(t, f.blocker.IsBlocked("203.0.113.10"), attempt == 3)
			}
			blocked := f.request(t, "GET", "/api/admin/mailboxes", nil, 401)
			if message, _ := blocked["error"].(string); !strings.Contains(message, "IP") {
				t.Errorf("blocked response omitted IP error: %v", blocked)
			}
			f.now = f.now.Add(300 * time.Second)
			f.request(t, "GET", "/api/admin/mailboxes", nil, 200)
		})
	}
	t.Run("unconfigured fails closed", func(t *testing.T) {
		f := newAdminTestFixture(t)
		f.cfg.Password = ""
		for attempt := 0; attempt < 4; attempt++ {
			adminTestError(t, f.request(t, "GET", "/api/admin/mailboxes", nil, 401), "管理员密码未配置")
		}
		if f.blocker.IsBlocked("203.0.113.10") {
			t.Error("unconfigured password should fail before counting login failures")
		}
	})
}

func TestAdminAPIForwardedIP(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hops      int
		xff, want string
	}{
		{"untrusted forwarded", 0, "198.51.100.1, 198.51.100.2", "203.0.113.10"},
		{"one trusted hop", 1, "198.51.100.1, 198.51.100.2", "198.51.100.2"},
		{"two trusted hops", 2, "198.51.100.1, 198.51.100.2", "198.51.100.1"},
		{"insufficient hops", 3, "198.51.100.1, 198.51.100.2", "203.0.113.10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdminTestFixture(t)
			f.cfg.TrustProxyHops = tc.hops
			for attempt := 0; attempt < 3; attempt++ {
				f.raw(t, "GET", "/api/admin/mailboxes", "", "Bearer wrong", adminTestRemote, tc.xff, 401)
			}
			for _, ip := range []string{"203.0.113.10", "198.51.100.1", "198.51.100.2"} {
				adminTestEqual(t, f.blocker.IsBlocked(ip), ip == tc.want)
			}
		})
	}
	t.Run("spoofed values cannot evade failures", func(t *testing.T) {
		f := newAdminTestFixture(t)
		for attempt := 1; attempt <= 3; attempt++ {
			f.raw(t, "GET", "/api/admin/mailboxes", "", "Bearer wrong", adminTestRemote, fmt.Sprintf("198.51.100.%d", attempt), 401)
		}
		if !f.blocker.IsBlocked("203.0.113.10") {
			t.Error("rotating untrusted XFF bypassed failure counting")
		}
	})
}

func TestAdminAPIListFilterPagingAndStats(t *testing.T) {
	f := newAdminTestFixture(t)
	now := f.now.Unix()
	active := f.seed(t, "active@example.com", "admin", now-10, now+1, true)
	boundary := f.seed(t, "boundary@example.com", "register", now-20, now, true)
	expired := f.seed(t, "expired@example.net", "api_v2", now-30, now-1, true)
	disabled := f.seed(t, "disabled@example.net", "admin", now-40, now+10, false)
	oldDisabled := f.seed(t, "old@example.org", "unknown", now-50, now-1, false)
	for i := 0; i < 2; i++ {
		id, err := f.store.AddEmail(active.ID, store.Email{From: "sender@example.net", To: active.Address, Subject: "test", Body: "body", Timestamp: now - int64(i)})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := f.store.MarkEmailAsRead(id); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		query                    string
		ids                      []string
		total, page, size, pages int64
	}{
		{"", []string{active.ID, boundary.ID, expired.ID, disabled.ID, oldDisabled.ID}, 5, 1, 20, 1},
		{"?status=active", []string{active.ID}, 1, 1, 20, 1},
		{"?status=expired", []string{boundary.ID, expired.ID, oldDisabled.ID}, 3, 1, 20, 1},
		{"?status=disabled", []string{disabled.ID, oldDisabled.ID}, 2, 1, 20, 1},
		{"?source=admin", []string{active.ID, disabled.ID}, 2, 1, 20, 1},
		{"?status=disabled&source=admin", []string{disabled.ID}, 1, 1, 20, 1},
		{"?search=example.net", []string{expired.ID, disabled.ID}, 2, 1, 20, 1},
		{"?search=" + active.ID, []string{active.ID}, 1, 1, 20, 1},
		{"?status=all&source=all&page=2&page_size=2", []string{expired.ID, disabled.ID}, 5, 2, 2, 3},
		{"?page=4&page_size=2", []string{}, 5, 4, 2, 3},
		{"?search=does-not-exist", []string{}, 0, 1, 20, 0},
	} {
		t.Run(tc.query, func(t *testing.T) {
			data := adminTestObject(t, f.request(t, "GET", "/api/admin/mailboxes"+tc.query, nil, 200)["data"])
			for key, want := range map[string]int64{"total": tc.total, "page": tc.page, "page_size": tc.size, "total_pages": tc.pages} {
				adminTestNumber(t, data, key, want)
			}
			ids := []string{}
			for _, value := range adminTestArray(t, data["mailboxes"]) {
				m := adminTestObject(t, value)
				ids = append(ids, m["id"].(string))
				for _, key := range []string{"is_active", "is_expired", "whitelist_enabled"} {
					if _, ok := m[key].(bool); !ok {
						t.Errorf("%s must be JSON bool: %v", key, m)
					}
				}
				if m["id"] == boundary.ID {
					adminTestEqual(t, m["is_expired"], true)
				}
				if m["id"] == active.ID {
					adminTestNumber(t, m, "email_count", 2)
					adminTestNumber(t, m, "unread_count", 1)
				}
				for _, secret := range []string{"access_token", "mailbox_key"} {
					if _, found := m[secret]; found {
						t.Errorf("list exposed %s", secret)
					}
				}
			}
			adminTestEqual(t, ids, tc.ids)
		})
	}
	stats := adminTestObject(t, f.request(t, "GET", "/api/admin/stats", nil, 200)["data"])
	for key, want := range map[string]int64{"total_mailboxes": 5, "active_mailboxes": 1, "expired_mailboxes": 3, "disabled_mailboxes": 2, "total_emails": 2, "unread_emails": 1} {
		adminTestNumber(t, stats, key, want)
	}
	f.exec(t, "UPDATE mailboxes SET created_source = NULL WHERE id = ?", oldDisabled.ID)
	sources := adminTestObject(t, f.request(t, "GET", "/api/admin/source-stats", nil, 200)["data"])
	for key, want := range map[string]int64{"admin": 2, "register": 1, "api_v2": 1, "unknown": 1} {
		adminTestNumber(t, sources, key, want)
	}
	boundaryDetail := adminTestObject(t, f.request(t, "GET", "/api/admin/mailboxes/"+boundary.ID, nil, 200)["data"])
	adminTestEqual(t, boundaryDetail["is_expired"], true)
}

func TestAdminAPICreateValidation(t *testing.T) {
	for _, body := range []string{"[]", `"invalid"`, "42", "null", "true", "{"} {
		t.Run("nonobject "+body, func(t *testing.T) {
			f := newAdminTestFixture(t)
			adminTestError(t, f.raw(t, "POST", "/api/admin/mailboxes", body, "Bearer "+adminTestPassword, adminTestRemote, "", 400), "")
			adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM mailboxes"), 0)
		})
	}
	for _, tc := range []struct {
		name    string
		payload map[string]any
		message string
	}{
		{"missing address", map[string]any{}, "邮箱地址不能为空"},
		{"nonstring address", map[string]any{"address": 42}, "邮箱地址格式不正确"},
		{"address without at", map[string]any{"address": "example.com"}, "邮箱地址格式不正确"},
		{"short local", map[string]any{"address": "ab@example.com"}, "用户名仅允许英文/数字，长度 3–20"},
		{"long local", map[string]any{"address": strings.Repeat("a", 21) + "@example.com"}, "用户名仅允许英文/数字，长度 3–20"},
		{"local punctuation", map[string]any{"address": "a.b@example.com"}, "用户名仅允许英文/数字，长度 3–20"},
		{"unsafe local", map[string]any{"address": "<img_src_x>@example.com"}, "用户名仅允许英文/数字，长度 3–20"},
		{"unsafe domain", map[string]any{"address": "safe@<img>.com"}, "邮箱地址格式不正确"},
		{"protected", map[string]any{"address": "admin123@example.com"}, "该邮箱地址受保护，无法创建"},
		{"retention string", map[string]any{"address": "valid@example.com", "retention_days": "bad"}, "保留天数必须是有效的数字"},
		{"retention object", map[string]any{"address": "valid@example.com", "retention_days": map[string]any{}}, "保留天数必须是有效的数字"},
		{"zero retention", map[string]any{"address": "valid@example.com", "retention_days": 0}, "保留天数必须大于0"},
		{"negative retention", map[string]any{"address": "valid@example.com", "retention_days": -1}, "保留天数必须大于0"},
		{"excess retention", map[string]any{"address": "valid@example.com", "retention_days": 36501}, "保留天数不能超过36500天"},
		{"whitelist nonlist", map[string]any{"address": "valid@example.com", "sender_whitelist": "sender@example.net"}, "白名单必须是列表格式"},
		{"whitelist bad domain", map[string]any{"address": "valid@example.com", "sender_whitelist": []string{"@invalid"}}, "域名格式不正确: @invalid"},
		{"whitelist bad email", map[string]any{"address": "valid@example.com", "sender_whitelist": []string{"not-an-email"}}, "邮箱地址格式不正确: not-an-email"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdminTestFixture(t)
			adminTestError(t, f.request(t, "POST", "/api/admin/mailboxes", tc.payload, 400), tc.message)
			adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM mailboxes"), 0)
			adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM audit_logs"), 0)
		})
	}
}

func TestAdminAPICreatePersistenceAndDefaults(t *testing.T) {
	f := newAdminTestFixture(t)
	data := f.create(t, "Owner123@example.com", map[string]any{
		"sender_whitelist": []string{"sender@example.net", "@example.org"},
		"allowed_domains":  []string{"example.com", "example.net"},
	})
	adminTestEqual(t, data["allowed_domains"], []any{"example.com", "example.net"})
	adminTestEqual(t, data["sender_whitelist"], []any{"sender@example.net", "@example.org"})
	adminTestEqual(t, data["whitelist_enabled"], true)
	adminTestEqual(t, data["is_active"], true)
	for _, key := range []string{"access_token", "mailbox_key"} {
		if value, ok := data[key].(string); !ok || value == "" {
			t.Errorf("missing %s in creation response", key)
		}
	}
	var rawDomains, source, ip string
	if err := f.store.DB().QueryRow("SELECT allowed_domains, created_source, created_by_ip FROM mailboxes WHERE id = ?", data["id"]).Scan(&rawDomains, &source, &ip); err != nil {
		t.Fatal(err)
	}
	var domains []string
	if err := json.Unmarshal([]byte(rawDomains), &domains); err != nil {
		t.Fatal(err)
	}
	adminTestEqual(t, domains, []string{"example.com", "example.net"})
	adminTestEqual(t, source, "admin")
	adminTestEqual(t, ip, "203.0.113.10")
	createdAudit := f.audit(t, "/api/admin/mailboxes/"+data["id"].(string)+"/audit-logs", "CREATE")
	changes := adminTestObject(t, createdAudit["changes"])
	adminTestEqual(t, changes["address"], "Owner123@example.com")
	adminTestNumber(t, changes, "retention_days", 7)
	adminTestError(t, f.request(t, "POST", "/api/admin/mailboxes", map[string]any{"address": "Owner123@example.com"}, 400), "邮箱地址已存在")
	for _, tc := range []struct {
		address   string
		retention any
		include   bool
		want      int64
	}{
		{"default@example.com", nil, false, int64(f.cfg.MailboxRetentionDays)},
		{"nullretention@example.com", nil, true, int64(f.cfg.MailboxRetentionDays)},
		{"stringdays@example.com", "8", true, 8},
	} {
		payload := map[string]any{"address": tc.address}
		if tc.include {
			payload["retention_days"] = tc.retention
		}
		result := f.request(t, "POST", "/api/admin/mailboxes", payload, 200)
		adminTestNumber(t, adminTestObject(t, result["data"]), "retention_days", tc.want)
	}
}

func TestAdminAPIDetailUpdateAndAudit(t *testing.T) {
	f := newAdminTestFixture(t)
	created := f.create(t, "detail@example.com", nil)
	id := created["id"].(string)
	path := "/api/admin/mailboxes/" + id
	before := adminTestObject(t, f.request(t, "GET", path, nil, 200)["data"])
	adminTestEqual(t, before["access_token"], created["access_token"])
	adminTestEqual(t, before["mailbox_key"], created["mailbox_key"])
	adminTestEqual(t, before["sender_whitelist"], []any{})
	adminTestEqual(t, before["allowed_domains"], []any{})
	adminTestEqual(t, before["last_email_time"], nil)
	adminTestNumber(t, before, "email_count", 0)
	f.request(t, "GET", "/api/admin/mailboxes/missing", nil, 404)
	for _, payload := range []map[string]any{{}, {"unknown": "ignored"}, {"retention_days": 0}, {"sender_whitelist": "bad"}} {
		adminTestError(t, f.request(t, "PUT", path, payload, 400), "")
	}
	adminTestError(t, f.request(t, "PUT", "/api/admin/mailboxes/missing", map[string]any{"is_active": false}, 400), "邮箱不存在")
	f.now = f.now.Add(10 * time.Second)
	update := map[string]any{"retention_days": 9, "sender_whitelist": []string{"@example.net"}, "whitelist_enabled": true, "is_active": false, "allowed_domains": []string{"example.org"}, "address": "ignored@example.com"}
	adminTestEqual(t, f.request(t, "PUT", path, update, 200)["success"], true)
	after := adminTestObject(t, f.request(t, "GET", path, nil, 200)["data"])
	adminTestEqual(t, after["address"], before["address"])
	adminTestEqual(t, after["access_token"], before["access_token"])
	adminTestEqual(t, after["mailbox_key"], before["mailbox_key"])
	adminTestEqual(t, after["is_active"], false)
	adminTestEqual(t, after["whitelist_enabled"], true)
	adminTestEqual(t, after["sender_whitelist"], []any{"@example.net"})
	adminTestEqual(t, after["allowed_domains"], []any{"example.org"})
	adminTestEqual(t, after["updated_by_admin"], "admin")
	adminTestNumber(t, after, "updated_at", f.now.Unix())
	adminTestNumber(t, after, "expires_at", f.now.Unix()+9*86400)
	log := f.audit(t, path+"/audit-logs", "UPDATE")
	changes := adminTestObject(t, log["changes"])
	for _, key := range []string{"retention_days", "sender_whitelist", "whitelist_enabled", "is_active", "allowed_domains"} {
		change := adminTestObject(t, changes[key])
		adminTestEqual(t, change["old"], before[key])
		adminTestEqual(t, change["new"], after[key])
	}
	if _, ok := changes["address"]; ok {
		t.Error("ignored address field was recorded as an update")
	}
	limited := f.request(t, "GET", "/api/admin/audit-logs?limit=1", nil, 200)
	adminTestEqual(t, len(adminTestArray(t, limited["data"])), 1)
	adminTestEqual(t, adminTestObject(t, adminTestArray(t, limited["data"])[0])["action"], "UPDATE")
}

func TestAdminAPIDeleteAndAuditForeignKeys(t *testing.T) {
	f := newAdminTestFixture(t)
	created := f.create(t, "delete@example.com", nil)
	id := created["id"].(string)
	path := "/api/admin/mailboxes/" + id
	mailID, err := f.store.AddEmail(id, store.Email{From: "sender@example.net", To: "delete@example.com", Body: "preserve on soft delete", Timestamp: f.now.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	adminTestEqual(t, f.request(t, "DELETE", path, nil, 200)["success"], true)
	m, err := f.store.GetMailboxByID(id)
	if err != nil || m.IsActive {
		t.Fatalf("soft delete: mailbox=%v, error=%v", m, err)
	}
	if _, err := f.store.GetEmailByID(mailID); err != nil {
		t.Errorf("soft delete removed mail: %v", err)
	}
	softAudit := f.audit(t, path+"/audit-logs", "SOFT_DELETE")
	adminTestEqual(t, softAudit["mailbox_id"], id)
	adminTestEqual(t, adminTestObject(t, softAudit["changes"])["address"], "delete@example.com")
	adminTestEqual(t, f.request(t, "DELETE", path+"?soft=false", nil, 200)["success"], true)
	if _, err := f.store.GetMailboxByID(id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("hard delete mailbox: %v", err)
	}
	if _, err := f.store.GetEmailByID(mailID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("hard delete mail: %v", err)
	}
	hardAudit := f.audit(t, "/api/admin/audit-logs", "HARD_DELETE")
	adminTestEqual(t, hardAudit["mailbox_id"], nil)
	changes := adminTestObject(t, hardAudit["changes"])
	adminTestEqual(t, changes["mailbox_id"], id)
	adminTestEqual(t, changes["address"], "delete@example.com")
	var retainedID sql.NullString
	if err := f.store.DB().QueryRow("SELECT mailbox_id FROM audit_logs WHERE id = ?", softAudit["id"]).Scan(&retainedID); err != nil {
		t.Fatal(err)
	}
	if retainedID.Valid {
		t.Error("old audit mailbox foreign key was not set NULL")
	}
	adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM audit_logs WHERE mailbox_id IS NOT NULL"), 0)
	adminTestError(t, f.request(t, "DELETE", path, nil, 400), "邮箱不存在")
}

func TestAdminAPIBatchDeleteLiteralRoute(t *testing.T) {
	f := newAdminTestFixture(t)
	one := f.create(t, "batchone@example.com", nil)["id"].(string)
	two := f.create(t, "batchtwo@example.com", nil)["id"].(string)
	for _, payload := range []map[string]any{{}, {"mailbox_ids": []string{}}} {
		adminTestError(t, f.request(t, "POST", "/api/admin/mailboxes/batch-delete", payload, 400), "未提供邮箱ID")
	}
	result := f.request(t, "POST", "/api/admin/mailboxes/batch-delete", map[string]any{"mailbox_ids": []string{one, "missing", two}}, 200)
	adminTestEqual(t, result["success"], true)
	adminTestNumber(t, result, "success_count", 2)
	adminTestNumber(t, result, "failed_count", 1)
	failures := adminTestArray(t, result["errors"])
	if len(failures) != 1 {
		t.Fatalf("per-item errors=%v", failures)
	}
	failure := adminTestObject(t, failures[0])
	adminTestEqual(t, failure["mailbox_id"], "missing")
	adminTestEqual(t, failure["error"], "邮箱不存在")
	for _, id := range []string{one, two} {
		m, err := f.store.GetMailboxByID(id)
		if err != nil || m.IsActive {
			t.Errorf("batch soft delete %s: mailbox=%v, error=%v", id, m, err)
		}
	}
	result = f.request(t, "POST", "/api/admin/mailboxes/batch-delete", map[string]any{"mailbox_ids": []string{one, two}, "soft_delete": false}, 200)
	adminTestNumber(t, result, "success_count", 2)
	adminTestNumber(t, result, "failed_count", 0)
	adminTestEqual(t, len(adminTestArray(t, result["errors"])), 0)
	adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM mailboxes"), 0)
}

func TestAdminAPIResetEnableAndMissingMailbox(t *testing.T) {
	f := newAdminTestFixture(t)
	created := f.create(t, "reset@example.com", nil)
	id := created["id"].(string)
	path := "/api/admin/mailboxes/" + id
	reset := adminTestObject(t, f.request(t, "POST", path+"/reset-token", nil, 200)["data"])
	adminTestEqual(t, reset["mailbox_id"], id)
	token, ok := reset["new_token"].(string)
	if !ok || token == "" || token == created["access_token"] {
		t.Fatalf("reset did not rotate token: %v", reset)
	}
	m, err := f.store.GetMailboxByID(id)
	if err != nil {
		t.Fatal(err)
	}
	adminTestEqual(t, m.AccessToken, token)
	adminTestEqual(t, m.MailboxKey, created["mailbox_key"])
	resetAudit := f.audit(t, path+"/audit-logs", "RESET_TOKEN")
	adminTestEqual(t, adminTestObject(t, resetAudit["changes"])["new_token"], token)
	f.request(t, "DELETE", path, nil, 200)
	adminTestEqual(t, f.request(t, "POST", path+"/enable", nil, 200)["success"], true)
	m, err = f.store.GetMailboxByID(id)
	if err != nil || !m.IsActive {
		t.Fatalf("enable: mailbox=%v, error=%v", m, err)
	}
	restore := f.audit(t, path+"/audit-logs", "RESTORE")
	adminTestEqual(t, adminTestObject(t, restore["changes"])["is_active"], true)
	for _, endpoint := range []struct{ suffix, action string }{{"reset-token", "RESET_TOKEN"}, {"enable", "RESTORE"}} {
		missingID := "missing-" + endpoint.suffix
		adminTestEqual(t, f.request(t, "POST", "/api/admin/mailboxes/"+missingID+"/"+endpoint.suffix, nil, 200)["success"], true)
		var foreignKey sql.NullString
		var rawChanges string
		if err := f.store.DB().QueryRow("SELECT mailbox_id, changes FROM audit_logs WHERE action = ? AND mailbox_id IS NULL", endpoint.action).Scan(&foreignKey, &rawChanges); err != nil {
			t.Fatal(err)
		}
		var changes map[string]any
		if err := json.Unmarshal([]byte(rawChanges), &changes); err != nil {
			t.Fatal(err)
		}
		if foreignKey.Valid {
			t.Error("missing mailbox audit must have NULL foreign key")
		}
		adminTestEqual(t, changes["mailbox_id"], missingID)
		adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM mailboxes WHERE id = ?", missingID), 0)
	}
}

func TestAdminAPISecurityConfiguration(t *testing.T) {
	f := newAdminTestFixture(t)
	initial := adminTestObject(t, f.request(t, "GET", "/api/admin/security-config", nil, 200)["data"])
	for key, want := range map[string]int64{"block_duration": 300, "max_attempts": 3, "attempt_window": 60} {
		adminTestNumber(t, initial, key, want)
	}
	for _, tc := range []struct {
		field, message string
		invalid        []any
	}{
		{"block_duration", "封禁时长必须在60-86400秒之间", []any{59, 86401, "120", 120.5, true, []any{}, map[string]any{}}},
		{"max_attempts", "最大失败次数必须在1-10次之间", []any{0, 11, "3", 2.5, true, []any{}, map[string]any{}}},
		{"attempt_window", "尝试窗口时间必须在30-600秒之间", []any{29, 601, "60", 60.5, false, []any{}, map[string]any{}}},
	} {
		for _, value := range tc.invalid {
			t.Run(fmt.Sprintf("%s/%v", tc.field, value), func(t *testing.T) {
				adminTestError(t, f.request(t, "PUT", "/api/admin/security-config", map[string]any{tc.field: value}, 400), tc.message)
				current := adminTestObject(t, f.request(t, "GET", "/api/admin/security-config", nil, 200)["data"])
				adminTestEqual(t, current, initial)
			})
		}
	}
	adminTestError(t, f.request(t, "PUT", "/api/admin/security-config", map[string]any{"block_duration": 120, "max_attempts": 11}, 400), "最大失败次数必须在1-10次之间")
	adminTestEqual(t, adminTestObject(t, f.request(t, "GET", "/api/admin/security-config", nil, 200)["data"]), initial)
	for _, body := range []string{"[]", "null", `"bad"`, "42"} {
		adminTestError(t, f.raw(t, "PUT", "/api/admin/security-config", body, "Bearer "+adminTestPassword, adminTestRemote, "", 400), "")
	}
	adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM audit_logs WHERE action='UPDATE_CONFIG'"), 0)
	for _, limits := range []map[string]any{
		{"block_duration": 60, "max_attempts": 1, "attempt_window": 30},
		{"block_duration": 86400, "max_attempts": 10, "attempt_window": 600},
	} {
		updated := adminTestObject(t, f.request(t, "PUT", "/api/admin/security-config", limits, 200)["data"])
		for key, want := range limits {
			adminTestNumber(t, updated, key, int64(want.(int)))
		}
	}
	before := adminTestObject(t, f.request(t, "GET", "/api/admin/security-config", nil, 200)["data"])
	adminTestEqual(t, adminTestObject(t, f.request(t, "PUT", "/api/admin/security-config", map[string]any{"block_duration": nil}, 200)["data"]), before)
	f.now = f.now.Add(time.Second)
	f.request(t, "PUT", "/api/admin/security-config", map[string]any{"max_attempts": 4}, 200)
	log := f.audit(t, "/api/admin/audit-logs", "UPDATE_CONFIG")
	adminTestEqual(t, log["mailbox_id"], nil)
	adminTestNumber(t, adminTestObject(t, log["changes"]), "max_attempts", 4)
}

func TestAdminAPIBlockedIPs(t *testing.T) {
	f := newAdminTestFixture(t)
	for _, ip := range []string{"198.51.100.20", "2001:db8::1"} {
		for attempt := 0; attempt < 3; attempt++ {
			f.blocker.RecordFailedAttempt(ip)
		}
	}
	result := adminTestObject(t, f.request(t, "GET", "/api/admin/blocked-ips", nil, 200)["data"])
	adminTestNumber(t, result, "total", 2)
	ips := []string{}
	for _, value := range adminTestArray(t, result["blocked_ips"]) {
		entry := adminTestObject(t, value)
		ips = append(ips, entry["ip"].(string))
		adminTestNumber(t, entry, "remaining_seconds", 300)
	}
	sort.Strings(ips)
	adminTestEqual(t, ips, []string{"198.51.100.20", "2001:db8::1"})
	for _, ip := range ips {
		adminTestEqual(t, f.request(t, "DELETE", "/api/admin/blocked-ips/"+url.PathEscape(ip), nil, 200)["success"], true)
		if f.blocker.IsBlocked(ip) {
			t.Errorf("failed to unblock %s", ip)
		}
	}
	result = adminTestObject(t, f.request(t, "GET", "/api/admin/blocked-ips", nil, 200)["data"])
	adminTestNumber(t, result, "total", 0)
	adminTestEqual(t, len(adminTestArray(t, result["blocked_ips"])), 0)
}

func TestAdminAPISubAdminCRUD(t *testing.T) {
	f := newAdminTestFixture(t)
	adminTestEqual(t, len(adminTestArray(t, f.request(t, "GET", "/api/admin/sub-admins", nil, 200)["data"])), 0)
	result := f.request(t, "POST", "/api/admin/sub-admins", map[string]any{
		"token": "  scoped-token  ", "domains": []string{" Example.NET. ", "BÜCHER.DE"},
		"sender_whitelist": []string{"@example.org", "sender@example.net"}, "max_retention_days": "8", "notes": "  scoped notes  ",
	}, 200)
	data := adminTestObject(t, result["data"])
	adminTestEqual(t, data["token"], "scoped-token")
	id, ok := data["id"].(string)
	if !ok || id == "" {
		t.Fatalf("missing subadmin ID: %v", data)
	}
	adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM domains"), 0)
	items := adminTestArray(t, f.request(t, "GET", "/api/admin/sub-admins", nil, 200)["data"])
	if len(items) != 1 {
		t.Fatalf("subadmin list=%v", items)
	}
	item := adminTestObject(t, items[0])
	adminTestEqual(t, item["domains"], []any{"example.net", "xn--bcher-kva.de"})
	adminTestEqual(t, item["sender_whitelist"], []any{"@example.org", "sender@example.net"})
	adminTestEqual(t, item["notes"], "scoped notes")
	adminTestEqual(t, item["created_by"], "admin")
	adminTestNumber(t, item, "max_retention_days", 8)
	adminTestError(t, f.request(t, "POST", "/api/admin/sub-admins", map[string]any{"token": "scoped-token", "domains": []string{"example.net"}}, 400), "Token已存在")
	path := "/api/admin/sub-admins/" + id
	f.request(t, "PUT", path, map[string]any{"domains": " New.EXAMPLE. , Other.NET ", "sender_whitelist": []string{}, "max_retention_days": 36500, "is_active": false, "notes": "changed"}, 200)
	items = adminTestArray(t, f.request(t, "GET", "/api/admin/sub-admins", nil, 200)["data"])
	item = adminTestObject(t, items[0])
	adminTestEqual(t, item["domains"], []any{"new.example", "other.net"})
	adminTestEqual(t, item["sender_whitelist"], []any{})
	adminTestNumber(t, item, "max_retention_days", 36500)
	adminTestNumber(t, item, "is_active", 0)
	adminTestEqual(t, item["notes"], "changed")
	adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM domains"), 0)
	for _, invalid := range []any{[]any{"valid.example", "bad/domain"}, []any{42}, "https://example.com", []string{"a..example"}} {
		adminTestError(t, f.request(t, "PUT", path, map[string]any{"domains": invalid}, 400), "")
	}
	var persisted string
	if err := f.store.DB().QueryRow("SELECT domains FROM sub_admins WHERE id=?", id).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	adminTestEqual(t, persisted, "new.example,other.net")
	f.request(t, "DELETE", path, nil, 200)
	adminTestEqual(t, len(adminTestArray(t, f.request(t, "GET", "/api/admin/sub-admins", nil, 200)["data"])), 0)
}

func TestAdminAPISubAdminValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload map[string]any
		message string
	}{
		{"no token", map[string]any{"domains": []string{"example.net"}}, "Token不能为空"},
		{"no domains", map[string]any{"token": "new-token"}, "至少需要分配一个域名"},
		{"empty domains", map[string]any{"token": "new-token", "domains": []string{}}, "至少需要分配一个域名"},
		{"invalid domain", map[string]any{"token": "new-token", "domains": []string{"https://example.net"}}, ""},
		{"nonstring domain", map[string]any{"token": "new-token", "domains": []any{42}}, ""},
		{"short retention", map[string]any{"token": "new-token", "domains": []string{"example.net"}, "max_retention_days": 0}, "最长保留天数必须在1-36500之间"},
		{"long retention", map[string]any{"token": "new-token", "domains": []string{"example.net"}, "max_retention_days": 36501}, "最长保留天数必须在1-36500之间"},
		{"invalid retention", map[string]any{"token": "new-token", "domains": []string{"example.net"}, "max_retention_days": "bad"}, "最长保留天数必须是有效的数字"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdminTestFixture(t)
			adminTestError(t, f.request(t, "POST", "/api/admin/sub-admins", tc.payload, 400), tc.message)
			adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM sub_admins"), 0)
		})
	}
}

func TestAdminAPIDomainCRUD(t *testing.T) {
	f := newAdminTestFixture(t)
	adminTestEqual(t, len(adminTestArray(t, f.request(t, "GET", "/api/admin/domains", nil, 200)["data"])), 0)
	created := adminTestObject(t, f.request(t, "POST", "/api/admin/domains", map[string]any{"domain": " Example.COM. "}, 201)["data"])
	adminTestEqual(t, created["domain"], "example.com")
	adminTestEqual(t, created["enabled"], true)
	if value, ok := created["created_at"].(float64); !ok || value <= 0 {
		t.Errorf("invalid created_at: %v", created)
	}
	adminTestError(t, f.request(t, "POST", "/api/admin/domains", map[string]any{"domain": "EXAMPLE.com."}, 409), "域名已存在")
	adminTestError(t, f.request(t, "PUT", "/api/admin/domains/example.com", map[string]any{"enabled": false}, 409), "至少需要保留一个启用的域名")
	adminTestError(t, f.request(t, "DELETE", "/api/admin/domains/example.com", nil, 409), "至少需要保留一个启用的域名")
	idn := adminTestObject(t, f.request(t, "POST", "/api/admin/domains", map[string]any{"domain": "BÜCHER.DE."}, 201)["data"])
	adminTestEqual(t, idn["domain"], "xn--bcher-kva.de")
	adminTestEqual(t, idn["enabled"], true)
	encoded := "/api/admin/domains/" + url.PathEscape("BÜCHER.DE.")
	disabled := adminTestObject(t, f.request(t, "PUT", encoded, map[string]any{"enabled": false}, 200)["data"])
	adminTestEqual(t, disabled["domain"], "xn--bcher-kva.de")
	adminTestEqual(t, disabled["enabled"], false)
	adminTestError(t, f.request(t, "PUT", "/api/admin/domains/example.com", map[string]any{"enabled": false}, 409), "至少需要保留一个启用的域名")
	adminTestError(t, f.request(t, "DELETE", "/api/admin/domains/example.com", nil, 409), "至少需要保留一个启用的域名")
	items := adminTestArray(t, f.request(t, "GET", "/api/admin/domains", nil, 200)["data"])
	if len(items) != 2 {
		t.Fatalf("domain list=%v", items)
	}
	for _, value := range items {
		if _, ok := adminTestObject(t, value)["enabled"].(bool); !ok {
			t.Error("domain enabled must be JSON bool")
		}
	}
	f.request(t, "PUT", encoded, map[string]any{"enabled": true}, 200)
	f.request(t, "DELETE", encoded, nil, 200)
	adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM domains"), 1)
	adminTestError(t, f.request(t, "DELETE", encoded, nil, 404), "域名不存在")
	for _, path := range []string{"missing.example", "bad_domain", "bad%2Fdomain", "%3Cimg%3E.com"} {
		adminTestError(t, f.request(t, "PUT", "/api/admin/domains/"+path, map[string]any{"enabled": false}, 404), "域名不存在")
		adminTestError(t, f.request(t, "DELETE", "/api/admin/domains/"+path, nil, 404), "域名不存在")
	}
}

func TestAdminAPIDomainValidation(t *testing.T) {
	f := newAdminTestFixture(t)
	for _, body := range []string{`{}`, `{"domain":null}`, `{"DOMAIN":"example.com"}`, `{"domain":42}`, `{"domain":true}`, `{"domain":""}`, `{"domain":"https://example.com"}`, `{"domain":"a..example"}`, `{"domain":"127.0.0.1"}`, `[]`, `null`, `"example.com"`} {
		adminTestError(t, f.raw(t, "POST", "/api/admin/domains", body, "Bearer "+adminTestPassword, adminTestRemote, "", 400), "")
	}
	f.request(t, "POST", "/api/admin/domains", map[string]any{"domain": "enabled.example"}, 201)
	for _, body := range []string{`{}`, `{"enabled":null}`, `{"ENABLED":false}`, `{"enabled":"false"}`, `{"enabled":0}`, `{"enabled":1}`, `{"enabled":[]}`, `{"enabled":{}}`, `[]`, `null`, `true`} {
		adminTestError(t, f.raw(t, "PUT", "/api/admin/domains/enabled.example", body, "Bearer "+adminTestPassword, adminTestRemote, "", 400), "")
	}
	var enabled bool
	if err := f.store.DB().QueryRow("SELECT enabled FROM domains WHERE domain='enabled.example'").Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	adminTestEqual(t, enabled, true)
}

func TestAdminAPIAllRoutesRequireAuthentication(t *testing.T) {
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/admin/mailboxes"},
		{"POST", "/api/admin/mailboxes"},
		{"POST", "/api/admin/mailboxes/batch-delete"},
		{"GET", "/api/admin/mailboxes/missing"},
		{"PUT", "/api/admin/mailboxes/missing"},
		{"DELETE", "/api/admin/mailboxes/missing"},
		{"GET", "/api/admin/mailboxes/missing/audit-logs"},
		{"POST", "/api/admin/mailboxes/missing/reset-token"},
		{"POST", "/api/admin/mailboxes/missing/enable"},
		{"GET", "/api/admin/audit-logs"},
		{"GET", "/api/admin/stats"},
		{"GET", "/api/admin/source-stats"},
		{"GET", "/api/admin/blocked-ips"},
		{"DELETE", "/api/admin/blocked-ips/198.51.100.20"},
		{"GET", "/api/admin/security-config"},
		{"PUT", "/api/admin/security-config"},
		{"GET", "/api/admin/sub-admins"},
		{"POST", "/api/admin/sub-admins"},
		{"PUT", "/api/admin/sub-admins/missing"},
		{"DELETE", "/api/admin/sub-admins/missing"},
		{"GET", "/api/admin/domains"},
		{"POST", "/api/admin/domains"},
		{"POST", "/api/admin/domains/batch-add"},
		{"POST", "/api/admin/domains/batch-delete"},
		{"PUT", "/api/admin/domains/example.com"},
		{"DELETE", "/api/admin/domains/example.com"},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			// Isolate failure counters so every route exercises missing-header auth.
			f := newAdminTestFixture(t)
			result := f.raw(t, route.method, route.path, "{}", "", adminTestRemote, "", http.StatusUnauthorized)
			adminTestError(t, result, "缺少Authorization请求头")
		})
	}
}

func TestAdminAPIAdminDisabledTransitions(t *testing.T) {
	f := newAdminTestFixture(t)
	id := f.create(t, "stateflag@example.com", nil)["id"].(string)
	path := "/api/admin/mailboxes/" + id
	assertState := func(active, disabled bool) {
		t.Helper()
		detail := adminTestObject(t, f.request(t, "GET", path, nil, 200)["data"])
		list := adminTestObject(t, f.request(t, "GET", "/api/admin/mailboxes?search="+id, nil, 200)["data"])
		items := adminTestArray(t, list["mailboxes"])
		if len(items) != 1 {
			t.Fatalf("mailboxes=%v", items)
		}
		for _, item := range []map[string]any{detail, adminTestObject(t, items[0])} {
			adminTestEqual(t, item["is_active"], active)
			adminTestEqual(t, item["admin_disabled"], disabled)
		}
		var stored bool
		if err := f.store.DB().QueryRow("SELECT admin_disabled FROM mailboxes WHERE id=?", id).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		adminTestEqual(t, stored, disabled)
	}
	assertState(true, false)
	f.request(t, "DELETE", path+"?soft=true", nil, 200)
	assertState(false, true)
	adminTestEqual(t, adminTestObject(t, f.audit(t, path+"/audit-logs", "SOFT_DELETE")["changes"])["admin_disabled"], true)
	f.request(t, "POST", path+"/enable", nil, 200)
	assertState(true, false)
	adminTestEqual(t, adminTestObject(t, f.audit(t, path+"/audit-logs", "RESTORE")["changes"])["admin_disabled"], false)
	for _, active := range []bool{false, true} {
		f.now = f.now.Add(time.Second)
		f.request(t, "PUT", path, map[string]any{"is_active": active}, 200)
		assertState(active, !active)
		changes := adminTestObject(t, f.audit(t, path+"/audit-logs", "UPDATE")["changes"])
		adminTestEqual(t, changes["admin_disabled"], map[string]any{"old": active, "new": !active})
	}
	result := f.request(t, "POST", "/api/admin/mailboxes/batch-delete", map[string]any{"mailbox_ids": []string{id}, "soft_delete": true}, 200)
	adminTestNumber(t, result, "success_count", 1)
	assertState(false, true)
}

func TestAdminAPIStrictMailboxLists(t *testing.T) {
	f := newAdminTestFixture(t)
	id := f.create(t, "strictwrite@example.com", map[string]any{"sender_whitelist": []string{"sender@example.net"}, "allowed_domains": []string{"example.net"}})["id"].(string)
	path := "/api/admin/mailboxes/" + id
	before := adminTestObject(t, f.request(t, "GET", path, nil, 200)["data"])
	for _, tt := range []struct {
		name, field, message string
		value                any
	}{
		{"whitelist false item", "sender_whitelist", "白名单条目必须是字符串", []any{false}},
		{"whitelist zero item", "sender_whitelist", "白名单条目必须是字符串", []any{0}},
		{"whitelist null item", "sender_whitelist", "白名单条目必须是字符串", []any{nil}},
		{"whitelist true item", "sender_whitelist", "白名单条目必须是字符串", []any{true}},
		{"whitelist mixed item", "sender_whitelist", "白名单条目必须是字符串", []any{"sender@example.net", false}},
		{"whitelist string", "sender_whitelist", "白名单必须是列表格式", "sender@example.net"},
		{"whitelist null", "sender_whitelist", "白名单必须是列表格式", nil},
		{"whitelist false", "sender_whitelist", "白名单必须是列表格式", false},
		{"whitelist zero", "sender_whitelist", "白名单必须是列表格式", 0},
		{"whitelist empty object", "sender_whitelist", "白名单必须是列表格式", map[string]any{}},
		{"domains string", "allowed_domains", "允许的域名必须是字符串列表", "example.com"},
		{"domains null", "allowed_domains", "允许的域名必须是字符串列表", nil},
		{"domains false item", "allowed_domains", "允许的域名必须是字符串列表", []any{false}},
		{"domains zero item", "allowed_domains", "允许的域名必须是字符串列表", []any{0}},
		{"domains null item", "allowed_domains", "允许的域名必须是字符串列表", []any{nil}},
		{"domains object", "allowed_domains", "允许的域名必须是字符串列表", map[string]any{}},
		{"domains invalid", "allowed_domains", "允许的域名格式不正确: https://example.com", []string{"https://example.com"}},
		{"domains invalid hostname", "allowed_domains", "允许的域名格式不正确: a..example", []string{"a..example"}},
		{"domains blank", "allowed_domains", "允许的域名格式不正确: ", []string{"  "}},
		{"domains two trailing dots", "allowed_domains", "允许的域名格式不正确: example.com..", []string{"example.com.."}},
	} {
		for _, method := range []string{"POST", "PUT"} {
			t.Run(method+"/"+tt.name, func(t *testing.T) {
				endpoint := path
				payload := map[string]any{tt.field: tt.value}
				if method == "POST" {
					endpoint = "/api/admin/mailboxes"
					payload["address"] = "rejected@example.com"
				} else {
					payload["is_active"] = false
				}
				adminTestError(t, f.request(t, method, endpoint, payload, 400), tt.message)
				adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM mailboxes"), 1)
				adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM audit_logs"), 1)
				adminTestEqual(t, adminTestObject(t, f.request(t, "GET", path, nil, 200)["data"]), before)
			})
		}
	}
}

func TestAdminAPIMailboxListsNormalized(t *testing.T) {
	f := newAdminTestFixture(t)
	input := map[string]any{
		"sender_whitelist": []string{" Sender@Example.NET ", "sender@example.net", "", "  ", " @Example.ORG ", "@example.org"},
		"allowed_domains":  []string{" Example.COM. ", "example.com", " BÜCHER.DE. ", "xn--bcher-kva.de"},
	}
	created := f.create(t, "listnormalize@example.com", input)
	path := "/api/admin/mailboxes/" + created["id"].(string)
	assertLists := func(data map[string]any) {
		t.Helper()
		adminTestEqual(t, data["sender_whitelist"], []any{"Sender@Example.NET", "@Example.ORG"})
		adminTestEqual(t, data["allowed_domains"], []any{"example.com", "xn--bcher-kva.de"})
	}
	assertLists(created)
	assertLists(adminTestObject(t, f.request(t, "GET", path, nil, 200)["data"]))
	f.now = f.now.Add(time.Second)
	f.request(t, "PUT", path, input, 200)
	assertLists(adminTestObject(t, f.request(t, "GET", path, nil, 200)["data"]))
	changes := adminTestObject(t, f.audit(t, path+"/audit-logs", "UPDATE")["changes"])
	for _, field := range []string{"sender_whitelist", "allowed_domains"} {
		adminTestEqual(t, adminTestObject(t, changes[field])["new"], created[field])
	}
	f.request(t, "PUT", path, map[string]any{"sender_whitelist": []string{}, "allowed_domains": []string{}}, 200)
	empty := adminTestObject(t, f.request(t, "GET", path, nil, 200)["data"])
	adminTestEqual(t, empty["sender_whitelist"], []any{})
	adminTestEqual(t, empty["allowed_domains"], []any{})
	blank := f.create(t, "blankwhitelist@example.com", map[string]any{"sender_whitelist": []string{"", "  "}})
	adminTestEqual(t, blank["sender_whitelist"], []any{})
	adminTestEqual(t, blank["whitelist_enabled"], false)
}

func TestAdminAPISubAdminStrictWhitelist(t *testing.T) {
	f := newAdminTestFixture(t)
	created := adminTestObject(t, f.request(t, "POST", "/api/admin/sub-admins", map[string]any{
		"token": "normal-whitelist", "domains": []string{"example.com"},
		"sender_whitelist": []string{" Example.COM ", "example.com", "", " @Example.NET ", "@example.net", " Sender@Example.ORG "},
	}, 200)["data"])
	path := "/api/admin/sub-admins/" + created["id"].(string)
	before := adminTestArray(t, f.request(t, "GET", "/api/admin/sub-admins", nil, 200)["data"])
	adminTestEqual(t, adminTestObject(t, before[0])["sender_whitelist"], []any{"Example.COM", "@Example.NET", "Sender@Example.ORG"})
	for _, tt := range []struct {
		name, message string
		value         any
	}{
		{"string", "白名单必须是列表格式", "example.com"},
		{"false", "白名单必须是列表格式", false},
		{"zero", "白名单必须是列表格式", 0},
		{"null", "白名单必须是列表格式", nil},
		{"empty string", "白名单必须是列表格式", ""},
		{"object", "白名单必须是列表格式", map[string]any{}},
		{"false item", "白名单条目必须是字符串", []any{false}},
		{"zero item", "白名单条目必须是字符串", []any{0}},
		{"null item", "白名单条目必须是字符串", []any{nil}},
	} {
		for _, method := range []string{"POST", "PUT"} {
			t.Run(method+"/"+tt.name, func(t *testing.T) {
				endpoint := path
				payload := map[string]any{"sender_whitelist": tt.value}
				if method == "POST" {
					endpoint = "/api/admin/sub-admins"
					payload["token"] = "rejected-whitelist"
					payload["domains"] = []string{"example.com"}
				}
				adminTestError(t, f.request(t, method, endpoint, payload, 400), tt.message)
				adminTestEqual(t, adminTestArray(t, f.request(t, "GET", "/api/admin/sub-admins", nil, 200)["data"]), before)
			})
		}
	}
	f.request(t, "PUT", path, map[string]any{"sender_whitelist": []string{" Other.COM ", "other.com", " ", " sender@example.net "}}, 200)
	items := adminTestArray(t, f.request(t, "GET", "/api/admin/sub-admins", nil, 200)["data"])
	adminTestEqual(t, adminTestObject(t, items[0])["sender_whitelist"], []any{"Other.COM", "sender@example.net"})
	f.request(t, "PUT", path, map[string]any{"sender_whitelist": []string{}}, 200)
	items = adminTestArray(t, f.request(t, "GET", "/api/admin/sub-admins", nil, 200)["data"])
	adminTestEqual(t, adminTestObject(t, items[0])["sender_whitelist"], []any{})
}

func TestAdminAPIAuditLogLimits(t *testing.T) {
	f := newAdminTestFixture(t)
	id := f.create(t, "auditbounded@example.com", nil)["id"].(string)
	otherID := f.create(t, "auditother@example.com", nil)["id"].(string)
	f.exec(t, `WITH RECURSIVE entries(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM entries WHERE n<1005)
 INSERT INTO audit_logs (id,timestamp,action,mailbox_id,admin_user,changes)
 SELECT 'http-limit-' || n,?+n,'UPDATE',?,'admin','{}' FROM entries`, f.now.Unix(), id)
	f.exec(t, "INSERT INTO audit_logs (id,timestamp,action,mailbox_id,admin_user,changes) VALUES ('other-newest',?,'UPDATE',?,'admin','{}')", f.now.Unix()+2000, otherID)
	for _, route := range []struct {
		path     string
		fallback int
		firstID  string
	}{
		{"/api/admin/audit-logs", 100, "other-newest"},
		{"/api/admin/mailboxes/" + id + "/audit-logs", 50, "http-limit-1005"},
	} {
		for _, tt := range []struct {
			query string
			want  int
		}{
			{"", route.fallback}, {"?limit=-1", route.fallback}, {"?limit=0", route.fallback},
			{"?limit=not-a-number", route.fallback}, {"?limit=", route.fallback},
			{"?limit=5000", 1000}, {"?limit=1", 1}, {"?limit=1000", 1000},
		} {
			t.Run(route.path+tt.query, func(t *testing.T) {
				logs := adminTestArray(t, f.request(t, "GET", route.path+tt.query, nil, 200)["data"])
				if len(logs) != tt.want {
					t.Fatalf("logs=%d want=%d", len(logs), tt.want)
				}
				adminTestEqual(t, adminTestObject(t, logs[0])["id"], route.firstID)
				if route.fallback == 50 {
					for _, value := range logs {
						adminTestEqual(t, adminTestObject(t, value)["mailbox_id"], id)
					}
				}
			})
		}
	}
}
func TestAdminAPIDomainBatchValidation(t *testing.T) {
	for _, operation := range []string{"batch-add", "batch-delete"} {
		t.Run(operation, func(t *testing.T) {
			f := newAdminTestFixture(t)
			if err := f.store.SeedDomainsIfEmpty([]string{"keeper.test", "target.test"}); err != nil {
				t.Fatal(err)
			}
			before := f.request(t, "GET", "/api/admin/domains", nil, 200)
			path := "/api/admin/domains/" + operation
			for _, tc := range []struct{ name, body string }{
				{"empty body", ""}, {"whitespace body", " \n\t "}, {"malformed JSON", "{"},
				{"array body", `[]`}, {"string body", `"example.com"`}, {"number body", `42`},
				{"boolean body", `true`}, {"null body", `null`}, {"missing domains", `{}`},
				{"wrong field case", `{"DOMAINS":["target.test"]}`},
				{"null domains", `{"domains":null}`}, {"string domains", `{"domains":"target.test"}`},
				{"object domains", `{"domains":{}}`}, {"number domains", `{"domains":42}`},
				{"boolean domains", `{"domains":false}`}, {"empty domains", `{"domains":[]}`},
				{"only empty items", `{"domains":[""," ","\t\r\n","\u3000"]}`},
				{"null item", `{"domains":["target.test",null]}`},
				{"number item", `{"domains":["target.test",42]}`},
				{"boolean item", `{"domains":["target.test",false]}`},
				{"object item", `{"domains":["target.test",{}]}`},
				{"array item", `{"domains":["target.test",[]]}`},
				{"trailing JSON", `{"domains":["target.test"]} {}`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					result := f.raw(t, "POST", path, tc.body, "Bearer "+adminTestPassword, adminTestRemote, "", 400)
					adminTestEqual(t, result, map[string]any{"success": false, "error": "domains必须是非空字符串列表"})
				})
			}
			values := make([]string, 201)
			for i := range values {
				values[i] = "target.test"
			}
			result := f.request(t, "POST", path, map[string]any{"domains": values}, 400)
			adminTestEqual(t, result, map[string]any{"success": false, "error": "单次最多处理 200 个域名"})
			// 非字符串优先于数量限制，不能在读到第 201 项时提前接受或返回。
			mixed := make([]any, 202)
			for i := range mixed {
				mixed[i] = "target.test"
			}
			mixed[len(mixed)-1] = nil
			result = f.request(t, "POST", path, map[string]any{"domains": mixed}, 400)
			adminTestEqual(t, result, map[string]any{"success": false, "error": "domains必须是非空字符串列表"})
			oversized := `{"domains":["` + strings.Repeat("a", 1<<20) + `.test"]}`
			result = f.raw(t, "POST", path, oversized, "Bearer "+adminTestPassword, adminTestRemote, "", 413)
			adminTestEqual(t, result, map[string]any{"success": false, "error": "请求体过大"})
			adminTestEqual(t, f.request(t, "GET", "/api/admin/domains", nil, 200), before)
			adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM audit_logs"), 0)
		})
	}
}

func TestAdminAPIDomainBatchAdd(t *testing.T) {
	f := newAdminTestFixture(t)
	if _, err := f.store.AddDomain("existing.test"); err != nil {
		t.Fatal(err)
	}
	result := f.request(t, "POST", "/api/admin/domains/batch-add", map[string]any{"domains": []string{
		"", " \t ", " ZETA.TEST. ", " EXISTING.TEST. ", " invalid ", "zeta.test", " B\u00dcCHER.DE. ", "xn--bcher-kva.de", "alpha.test", "existing.test",
	}}, 200)
	adminTestEqual(t, result["success"], true)
	data := adminTestObject(t, result["data"])
	added := adminTestArray(t, data["added"])
	if len(added) != 3 {
		t.Fatalf("added = %#v", added)
	}
	names := []string{"zeta.test", "xn--bcher-kva.de", "alpha.test"}
	first := adminTestObject(t, added[0])
	if timestamp, ok := first["created_at"].(float64); !ok || timestamp <= 0 {
		t.Fatalf("invalid timestamp: %#v", first)
	}
	for i, value := range added {
		adminTestEqual(t, value, map[string]any{
			"domain": names[i], "enabled": true, "created_at": first["created_at"], "updated_at": first["created_at"],
		})
	}
	adminTestEqual(t, data["skipped"], []any{
		map[string]any{"domain": "EXISTING.TEST.", "reason": "域名已存在"},
		map[string]any{"domain": "invalid", "reason": "域名格式不正确"},
		map[string]any{"domain": "zeta.test", "reason": "重复输入"},
		map[string]any{"domain": "xn--bcher-kva.de", "reason": "重复输入"},
		map[string]any{"domain": "existing.test", "reason": "重复输入"},
	})
	items := adminTestArray(t, f.request(t, "GET", "/api/admin/domains", nil, 200)["data"])
	adminTestEqual(t, len(items), 4)
	adminTestEqual(t, items[1:], added)
	adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM audit_logs"), 1)
	log := f.audit(t, "/api/admin/audit-logs", "BATCH_ADD_DOMAINS")
	adminTestEqual(t, log["mailbox_id"], nil)
	adminTestEqual(t, log["changes"], map[string]any{"domains": []any{"zeta.test", "xn--bcher-kva.de", "alpha.test"}, "skipped": float64(5)})
	adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM audit_logs WHERE mailbox_id IS NULL"), 1)
	unchanged := adminTestObject(t, f.request(t, "POST", "/api/admin/domains/batch-add", map[string]any{"domains": []string{"existing.test", "bad/domain"}}, 200)["data"])
	adminTestEqual(t, unchanged["added"], []any{})
	adminTestEqual(t, len(adminTestArray(t, unchanged["skipped"])), 2)
	adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM audit_logs"), 1)
	allAdded := adminTestObject(t, f.request(t, "POST", "/api/admin/domains/batch-add", map[string]any{"domains": []string{"new.test"}}, 200)["data"])
	adminTestEqual(t, allAdded["skipped"], []any{})
	adminTestEqual(t, len(adminTestArray(t, allAdded["added"])), 1)
	adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM audit_logs WHERE action='BATCH_ADD_DOMAINS'"), 2)
}

func TestAdminAPIDomainBatchDelete(t *testing.T) {
	f := newAdminTestFixture(t)
	if err := f.store.SeedDomainsIfEmpty([]string{"keeper.test", "alpha.test", "xn--bcher-kva.de", "disabled.test", "zeta.test"}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetDomainEnabled("disabled.test", false); err != nil {
		t.Fatal(err)
	}
	result := f.request(t, "POST", "/api/admin/domains/batch-delete", map[string]any{"domains": []string{
		"", " \t ", " ZETA.TEST. ", " missing.test ", " bad/domain ", "zeta.test", " B\u00dcCHER.DE. ", "MISSING.TEST.", "xn--bcher-kva.de", "alpha.test", "disabled.test",
	}}, 200)
	adminTestEqual(t, result, map[string]any{"success": true, "data": map[string]any{
		"deleted": []any{"zeta.test", "xn--bcher-kva.de", "alpha.test", "disabled.test"},
		"failed": []any{
			map[string]any{"domain": " missing.test ", "reason": "域名不存在"},
			map[string]any{"domain": " bad/domain ", "reason": "域名不存在"},
		},
	}})
	items := adminTestArray(t, f.request(t, "GET", "/api/admin/domains", nil, 200)["data"])
	if len(items) != 1 {
		t.Fatalf("remaining domains = %#v", items)
	}
	adminTestEqual(t, adminTestObject(t, items[0])["domain"], "keeper.test")
	adminTestEqual(t, adminTestObject(t, items[0])["enabled"], true)
	adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM audit_logs"), 1)
	log := f.audit(t, "/api/admin/audit-logs", "BATCH_DELETE_DOMAINS")
	adminTestEqual(t, log["mailbox_id"], nil)
	adminTestEqual(t, log["changes"], map[string]any{"domains": []any{"zeta.test", "xn--bcher-kva.de", "alpha.test", "disabled.test"}, "failed": float64(2)})
	adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM audit_logs WHERE mailbox_id IS NULL"), 1)
	unchanged := adminTestObject(t, f.request(t, "POST", "/api/admin/domains/batch-delete", map[string]any{"domains": []string{"missing.test", "MISSING.TEST.", "invalid"}}, 200)["data"])
	adminTestEqual(t, unchanged["deleted"], []any{})
	adminTestEqual(t, len(adminTestArray(t, unchanged["failed"])), 2)
	adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM audit_logs"), 1)
	if _, err := f.store.AddDomain("fresh.test"); err != nil {
		t.Fatal(err)
	}
	allDeleted := f.request(t, "POST", "/api/admin/domains/batch-delete", map[string]any{"domains": []string{"fresh.test", " FRESH.TEST. "}}, 200)
	adminTestEqual(t, allDeleted, map[string]any{"success": true, "data": map[string]any{"deleted": []any{"fresh.test"}, "failed": []any{}}})
	adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM audit_logs WHERE action='BATCH_DELETE_DOMAINS'"), 2)
}

func TestAdminAPIDomainBatchDeleteGuardAndSingleRoutes(t *testing.T) {
	f := newAdminTestFixture(t)
	if err := f.store.SeedDomainsIfEmpty([]string{"first.test", "second.test", "disabled.test"}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetDomainEnabled("disabled.test", false); err != nil {
		t.Fatal(err)
	}
	before := f.request(t, "GET", "/api/admin/domains", nil, 200)
	result := f.request(t, "POST", "/api/admin/domains/batch-delete", map[string]any{"domains": []string{
		"disabled.test", "first.test", "missing.test", "second.test", "FIRST.TEST.",
	}}, 409)
	adminTestEqual(t, result, map[string]any{"success": false, "error": "至少需要保留一个启用的域名"})
	adminTestEqual(t, f.request(t, "GET", "/api/admin/domains", nil, 200), before)
	adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM audit_logs"), 0)
	// 字面批量路由不能影响原有单域名更新、删除和创建。
	updated := adminTestObject(t, f.request(t, "PUT", "/api/admin/domains/SECOND.TEST.", map[string]any{"enabled": false}, 200)["data"])
	adminTestEqual(t, updated["domain"], "second.test")
	adminTestEqual(t, updated["enabled"], false)
	adminTestEqual(t, f.request(t, "DELETE", "/api/admin/domains/SECOND.TEST.", nil, 200), map[string]any{"success": true})
	created := adminTestObject(t, f.request(t, "POST", "/api/admin/domains", map[string]any{"domain": "single.test"}, 201)["data"])
	adminTestEqual(t, created["domain"], "single.test")
	adminTestEqual(t, created["enabled"], true)
	for _, literal := range []string{"batch-add", "batch-delete"} {
		adminTestError(t, f.request(t, "PUT", "/api/admin/domains/"+literal, map[string]any{"enabled": false}, 404), "域名不存在")
		adminTestError(t, f.request(t, "DELETE", "/api/admin/domains/"+literal, nil, 404), "域名不存在")
	}
}

func TestAdminAPIDomainBatchTwoHundredNonemptyItems(t *testing.T) {
	for _, operation := range []string{"batch-add", "batch-delete"} {
		t.Run(operation, func(t *testing.T) {
			f := newAdminTestFixture(t)
			if _, err := f.store.AddDomain("keeper.test"); err != nil {
				t.Fatal(err)
			}
			values := make([]string, 0, 202)
			values = append(values, "", " \t\u3000 ")
			for i := range 200 {
				value := fmt.Sprintf("domain-%03d.test", i)
				values = append(values, value)
				if operation == "batch-delete" {
					if _, err := f.store.AddDomain(value); err != nil {
						t.Fatal(err)
					}
				}
			}
			result := f.request(t, "POST", "/api/admin/domains/"+operation, map[string]any{"domains": values}, 200)
			adminTestEqual(t, result["success"], true)
			data := adminTestObject(t, result["data"])
			changedKey, failedKey := "added", "skipped"
			if operation == "batch-delete" {
				changedKey, failedKey = "deleted", "failed"
			}
			changes := adminTestArray(t, data[changedKey])
			if len(changes) != 200 {
				t.Fatalf("%s count = %d", changedKey, len(changes))
			}
			for i, value := range changes {
				if operation == "batch-add" {
					value = adminTestObject(t, value)["domain"]
				}
				adminTestEqual(t, value, values[i+2])
			}
			adminTestEqual(t, data[failedKey], []any{})
			adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM audit_logs"), 1)
		})
	}
}

func TestAdminAPIDomainBatchDatabaseErrorRollsBack(t *testing.T) {
	for _, operation := range []string{"batch-add", "batch-delete"} {
		t.Run(operation, func(t *testing.T) {
			f := newAdminTestFixture(t)
			values := []string{"first.test", "reject.test", "last.test"}
			if err := f.store.SeedDomainsIfEmpty([]string{"keeper.test"}); err != nil {
				t.Fatal(err)
			}
			trigger := `CREATE TRIGGER reject_domain_batch BEFORE INSERT ON domains
				WHEN NEW.domain = 'reject.test' BEGIN SELECT RAISE(ABORT, 'insert unavailable'); END`
			if operation == "batch-delete" {
				for _, value := range values {
					if _, err := f.store.AddDomain(value); err != nil {
						t.Fatal(err)
					}
				}
				trigger = `CREATE TRIGGER reject_domain_batch BEFORE DELETE ON domains
					WHEN OLD.domain = 'reject.test' BEGIN SELECT RAISE(ABORT, 'delete unavailable'); END`
			}
			before := f.request(t, "GET", "/api/admin/domains", nil, 200)
			f.exec(t, trigger)
			result := f.request(t, "POST", "/api/admin/domains/"+operation, map[string]any{"domains": values}, 500)
			adminTestError(t, result, "")
			adminTestEqual(t, f.request(t, "GET", "/api/admin/domains", nil, 200), before)
			adminTestEqual(t, f.count(t, "SELECT COUNT(*) FROM audit_logs"), 0)
		})
	}
}
