package maildrop

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http/httptest"
	"net/smtp"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{DatabasePath: filepath.Join(t.TempDir(), "mailbox.db"), Password: "test-admin-password", Domains: []string{"example.test"}, Protected: regexp.MustCompile("^admin.*"), RetentionDays: 30, MaxRetentionDays: 36500, EmailRetentionSeconds: 604800, MaxEmailBytes: 2 << 20, MaxMailboxBytes: 3 << 20, MaxEmails: 50, MaxAttempts: 100, BlockDuration: 300, AttemptWindow: 60}
}
func testApp(t *testing.T) (*App, *Store) {
	t.Helper()
	s, e := OpenStore(testConfig(t))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.DB.Close() })
	a, e := NewApp(s)
	if e != nil {
		t.Fatal(e)
	}
	return a, s
}
func request(t *testing.T, a *App, method, path, auth string, payload any) (int, any) {
	t.Helper()
	var raw []byte
	if payload != nil {
		raw = []byte(jsonText(payload))
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Content-Type", "application/json")
	if auth != "" {
		r.Header.Set("Authorization", "Bearer "+auth)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	var value any
	if e := json.Unmarshal(w.Body.Bytes(), &value); e != nil {
		t.Fatalf("%s %s: HTTP %d invalid JSON %q", method, path, w.Code, w.Body.String())
	}
	return w.Code, value
}
func mustStatus(t *testing.T, a *App, method, path, auth string, payload any, want int) any {
	t.Helper()
	code, v := request(t, a, method, path, auth, payload)
	if code != want {
		t.Fatalf("%s %s: got %d want %d: %v", method, path, code, want, v)
	}
	return v
}
func createTestMailbox(t *testing.T, s *Store, address string) Object {
	t.Helper()
	m, e := s.CreateMailbox(address, 30, []string{}, []string{}, "127.0.0.1", "admin")
	if e != nil {
		t.Fatal(e)
	}
	return m
}
func testEmail() Object {
	return Object{"From": "sender@remote.test", "Subject": "测试邮件", "Body": "hello 世界", "ContentType": "Text", "Timestamp": time.Now().Unix(), "Sent": "Oct 26 at 12:00:00"}
}

func TestPagesAndRemovedRoutes(t *testing.T) {
	a, _ := testApp(t)
	for _, path := range []string{"/", "/web", "/admin", "/register", "/api-test", "/web/test%40example.test----Abcd123456"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "{{") {
			t.Fatalf("unrendered template: %s", path)
		}
		if path == "/" && !strings.Contains(w.Body.String(), "管理员登录") {
			t.Fatal("missing admin entry")
		}
	}
	for _, path := range []string{"/mailbox", "/admin/mailboxes", "/api/create_mailbox_v2", "/api/user_login", "/web/bad-key"} {
		w := httptest.NewRecorder()
		a.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Errorf("removed route %s: %d", path, w.Code)
		}
		if strings.HasPrefix(path, "/web/") && w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatal("missing private headers")
		}
	}
}
func TestDomainsAuthValidationAndPersistence(t *testing.T) {
	a, s := testApp(t)
	admin := a.Config.Password
	mustStatus(t, a, "PUT", "/api/admin/domains", "", Object{"domains": []string{"evil.test"}}, 401)
	for _, v := range []any{[]string{}, Object{"domains": []string{}}, Object{"domains": []string{"https://example.test"}}, Object{"domains": []string{"-invalid.test"}}, Object{"domains": []string{"<script>"}}, Object{"domains": []any{3}}} {
		mustStatus(t, a, "PUT", "/api/admin/domains", admin, v, 400)
	}
	mustStatus(t, a, "PUT", "/api/admin/domains", admin, Object{"domains": []string{" New.TEST ", "new.test", "second.test"}}, 200)
	d, e := s.Domains()
	if e != nil || len(d) != 2 || d[0] != "new.test" {
		t.Fatalf("domains: %v %v", d, e)
	}
	mustStatus(t, a, "POST", "/api/admin/mailboxes", admin, Object{"address": "alice@example.test"}, 400)
	mustStatus(t, a, "POST", "/api/admin/mailboxes", admin, Object{"address": "alice@new.test"}, 200)
	c := s.Config
	s.DB.Close()
	c.Domains = []string{"ignored.test"}
	reopened, e := OpenStore(c)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.DB.Close()
	d, e = reopened.Domains()
	if e != nil || d[0] != "new.test" {
		t.Fatalf("lost saved domains: %v %v", d, e)
	}
}
func TestMailboxAuthOwnershipAndMutation(t *testing.T) {
	a, s := testApp(t)
	owner := createTestMailbox(t, s, "owner@example.test")
	other := createTestMailbox(t, s, "other@example.test")
	if e := s.Deliver([]string{str(owner["address"]), str(other["address"])}, testEmail()); e != nil {
		t.Fatal(e)
	}
	ownMail, _ := s.Emails(str(owner["id"]))
	otherMail, _ := s.Emails(str(other["id"]))
	access := str(owner["access_token"])
	mustStatus(t, a, "POST", "/api/get_mailbox_token", "", Object{"address": owner["address"], "mailbox_key": owner["mailbox_key"]}, 200)
	mustStatus(t, a, "POST", "/api/get_mailbox_token", "", Object{"address": owner["address"], "mailbox_key": "wrong"}, 401)
	info := mustStatus(t, a, "GET", "/api/mailbox_info_v2", access, nil, 200).(map[string]any)["mailbox"].(map[string]any)
	if _, ok := info["mailbox_key"]; ok {
		t.Fatal("leaked key")
	}
	if _, ok := info["access_token"]; ok {
		t.Fatal("leaked token")
	}
	mustStatus(t, a, "GET", "/api/mailbox_info_v2?token="+access, "", nil, 401)
	mustStatus(t, a, "GET", "/api/get_inbox?address=other@example.test", access, nil, 403)
	mustStatus(t, a, "POST", "/api/delete_emails_batch", access, Object{"email_ids": []any{ownMail[0]["id"], otherMail[0]["id"]}}, 403)
	if rows, _ := s.Emails(str(owner["id"])); len(rows) != 1 {
		t.Fatal("partial batch deletion")
	}
	for _, action := range []string{"mark_email_read", "delete_email"} {
		mustStatus(t, a, "POST", "/api/"+action, access, Object{"email_id": otherMail[0]["id"]}, 403)
	}
	mustStatus(t, a, "POST", "/api/mark_email_read", access, Object{"email_id": ownMail[0]["id"], "is_read": true}, 200)
	mustStatus(t, a, "POST", "/api/toggle_mailbox_status", access, Object{"address": owner["address"]}, 200)
	mustStatus(t, a, "GET", "/api/mailbox_info_v2", access, nil, 423)
	mustStatus(t, a, "POST", "/api/toggle_mailbox_status", access, Object{"address": owner["address"]}, 200)
	mustStatus(t, a, "POST", "/api/regenerate_mailbox_key", access, Object{"address": owner["address"], "current_key": owner["mailbox_key"]}, 200)
	after, e := s.Mailbox("id", str(owner["id"]))
	if e != nil || after["access_token"] != access || after["mailbox_key"] == owner["mailbox_key"] {
		t.Fatal("key rotation changed access token or failed")
	}
	mustStatus(t, a, "POST", "/api/delete_email", access, Object{"email_id": ownMail[0]["id"]}, 200)
	after, _ = s.Mailbox("id", str(owner["id"]))
	if num(after["storage_used"]) != 0 {
		t.Fatal("storage was not released")
	}
}
func TestLegacySQLitePreservesCredentials(t *testing.T) {
	c := testConfig(t)
	db, e := sql.Open("sqlite", c.DatabasePath)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec("CREATE TABLE mailboxes(id TEXT PRIMARY KEY,address TEXT UNIQUE,created_at INTEGER,expires_at INTEGER,retention_days INTEGER,is_active INTEGER,sender_whitelist TEXT,created_by_ip TEXT,access_token TEXT,last_accessed INTEGER,mailbox_key TEXT); INSERT INTO mailboxes VALUES('old-id','old@example.test',1,4102444800,30,1,'[]','127.0.0.1','old-access-token',1,'old-uuid-mailbox-key'); CREATE TABLE emails(id TEXT PRIMARY KEY,mailbox_id TEXT,from_address TEXT,to_address TEXT,subject TEXT,body TEXT,content_type TEXT,timestamp INTEGER,sent_formatted TEXT,is_read INTEGER); INSERT INTO emails VALUES('old-email','old-id','sender@example.test','old@example.test','旧邮件','保留正文','Text',1,'date',1)")
	if e != nil {
		t.Fatal(e)
	}
	db.Close()
	s, e := OpenStore(c)
	if e != nil {
		t.Fatal(e)
	}
	defer s.DB.Close()
	m, e := s.Mailbox("access_token", "old-access-token")
	if e != nil || m["id"] != "old-id" || m["mailbox_key"] != "old-uuid-mailbox-key" {
		t.Fatalf("legacy mailbox lost: %v %v", m, e)
	}
	emails, e := s.Emails("old-id")
	if e != nil || len(emails) != 1 || emails[0]["Body"] != "保留正文" {
		t.Fatalf("legacy mail lost: %v %v", emails, e)
	}
}
func TestSMTPEndToEndAndDynamicDomains(t *testing.T) {
	_, s := testApp(t)
	m := createTestMailbox(t, s, "smtpuser@example.test")
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	server := NewSMTP(s)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() { server.Close(); <-done }()
	send := func() error {
		return smtp.SendMail(listener.Addr().String(), nil, "sender@remote.test", []string{str(m["address"])}, []byte("From: Sender <sender@remote.test>\r\nTo: misleading@another.test\r\nSubject: =?UTF-8?B?5rWL6K+V?=\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\naGVsbG8gd29ybGQ=\r\n"))
	}
	if e = send(); e != nil {
		t.Fatal(e)
	}
	emails, e := s.Emails(str(m["id"]))
	if e != nil || len(emails) != 1 || emails[0]["To"] != m["address"] || emails[0]["Subject"] != "测试" || emails[0]["Body"] != "hello world" {
		t.Fatalf("SMTP/MIME: %v %v", emails, e)
	}
	if e = s.SaveDomains([]string{"new.test"}, "test"); e != nil {
		t.Fatal(e)
	}
	if e = send(); e == nil {
		t.Fatal("removed domain still accepted mail")
	}
	if rows, _ := s.Emails(str(m["id"])); len(rows) != 1 {
		t.Fatal("domain removal erased old mail")
	}
}
func TestConcurrentCapacityAndCleanup(t *testing.T) {
	_, s := testApp(t)
	m := createTestMailbox(t, s, "quota@example.test")
	email := testEmail()
	if _, e := s.DB.Exec("UPDATE mailboxes SET storage_limit=100 WHERE id=?", m["id"]); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.Deliver([]string{str(m["address"])}, email) }()
	}
	wg.Wait()
	after, e := s.Mailbox("id", str(m["id"]))
	if e != nil || num(after["storage_used"]) > 100 {
		t.Fatal("capacity race")
	}
	if e = s.Cleanup(time.Now().Add(8 * 24 * time.Hour)); e != nil {
		t.Fatal(e)
	}
	after, _ = s.Mailbox("id", str(m["id"]))
	if num(after["storage_used"]) != 0 {
		t.Fatal("cleanup did not release capacity")
	}
}
func TestSubAdminScopeAndAdminFailClosed(t *testing.T) {
	a, s := testApp(t)
	mustStatus(t, a, "POST", "/api/admin/sub-admins", a.Config.Password, Object{"token": "scoped-test-token", "domains": []string{"example.test"}, "max_retention_days": 7}, 200)
	for _, days := range []int{7, 8} {
		r := httptest.NewRequest("POST", "/api/register_with_token", strings.NewReader(jsonText(Object{"email": "scoped", "retention_days": days})))
		r.Header.Set("X-Sub-Admin-Token", "scoped-test-token")
		r.RemoteAddr = "127.0.0.1:1000"
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		want := 201
		if days == 8 {
			want = 400
		}
		if w.Code != want {
			t.Fatalf("scope days %d: %d %s", days, w.Code, w.Body.String())
		}
	}
	if e := s.SaveDomains([]string{"new.test"}, "test"); e != nil {
		t.Fatal(e)
	}
	r := httptest.NewRequest("POST", "/api/register_with_token", strings.NewReader(`{"email":"scoped2","retention_days":7}`))
	r.Header.Set("X-Sub-Admin-Token", "scoped-test-token")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("subadmin bypassed removed domain")
	}
	a.Config.Password = ""
	mustStatus(t, a, "POST", "/api/admin_login", "", Object{"password": ""}, 503)
	mustStatus(t, a, "GET", "/api/admin/stats", "anything", nil, 401)
}
func TestJSONImportPreservesAndIsIdempotent(t *testing.T) {
	_, s := testApp(t)
	path := filepath.Join(t.TempDir(), "inbox.json")
	raw := jsonText(Object{"legacy@example.test": Object{"created_at": 1, "expires_at": 4102444800, "access_token": "existing-token", "mailbox_key": "existing-key", "emails": []Object{{"id": "legacy-message", "From": "sender@test", "Body": "legacy body", "Timestamp": 1}}}})
	if e := os.WriteFile(path, []byte(raw), 0600); e != nil {
		t.Fatal(e)
	}
	n, e := s.ImportJSON(path)
	if e != nil || n != 1 {
		t.Fatalf("import: %d %v", n, e)
	}
	n, e = s.ImportJSON(path)
	if e != nil || n != 0 {
		t.Fatalf("repeat import: %d %v", n, e)
	}
	m, e := s.Mailbox("access_token", "existing-token")
	if e != nil || m["mailbox_key"] != "existing-key" {
		t.Fatal("import lost credentials")
	}
	f, e := os.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	if string(b) != raw {
		t.Fatal("source changed")
	}
}
func TestMalformedInputAndIPBoundary(t *testing.T) {
	a, _ := testApp(t)
	for _, v := range []any{[]string{"invalid"}, Object{"address": 12}, Object{"address": "safe@example.test", "retention_days": true}, Object{"address": "safe@example.test", "sender_whitelist": "bad"}} {
		mustStatus(t, a, "POST", "/api/admin/mailboxes", a.Config.Password, v, 400)
	}
	a.Config.IPWhitelistEnabled = true
	a.Config.IPWhitelist = []string{"192.0.2.1"}
	r := httptest.NewRequest("GET", "/api/get_domain", nil)
	r.RemoteAddr = "198.51.100.1:12"
	r.Header.Set("X-Forwarded-For", "192.0.2.1")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("spoofed forwarded IP bypassed whitelist")
	}
}

func TestManagementLifecycleAndAudit(t *testing.T) {
	a, s := testApp(t)
	admin := a.Config.Password
	created := mustStatus(t, a, "POST", "/api/admin/mailboxes", admin, Object{"address": "manage@example.test", "allowed_domains": []string{"sender.test"}}, 200).(map[string]any)["data"].(map[string]any)
	id := str(created["id"])
	access := str(created["access_token"])
	mustStatus(t, a, "PUT", "/api/admin/mailboxes/"+id, admin, Object{"retention_days": 7, "sender_whitelist": []string{"@remote.test"}, "whitelist_enabled": true}, 200)
	mustStatus(t, a, "GET", "/api/admin/mailboxes?page=1&page_size=10&search=manage&status=active&source=admin", admin, nil, 200)
	mustStatus(t, a, "GET", "/api/admin/mailboxes?page=0", admin, nil, 400)
	mustStatus(t, a, "POST", "/api/admin/mailboxes/"+id+"/reset-token", admin, Object{}, 200)
	mustStatus(t, a, "GET", "/api/mailbox_info_v2", access, nil, 401)
	mustStatus(t, a, "DELETE", "/api/admin/mailboxes/"+id, admin, nil, 200)
	m, _ := s.Mailbox("id", id)
	if truth(m["is_active"]) {
		t.Fatal("soft delete failed")
	}
	mustStatus(t, a, "POST", "/api/admin/mailboxes/"+id+"/enable", admin, Object{}, 200)
	for _, path := range []string{"/api/admin/stats", "/api/admin/source-stats", "/api/admin/security-config", "/api/admin/blocked-ips", "/api/admin/mailboxes/" + id + "/audit-logs"} {
		mustStatus(t, a, "GET", path, admin, nil, 200)
	}
	logs := mustStatus(t, a, "GET", "/api/admin/audit-logs", admin, nil, 200)
	if strings.Contains(jsonText(logs), access) || strings.Contains(jsonText(logs), str(created["mailbox_key"])) {
		t.Fatal("audit leaked credentials")
	}
	mustStatus(t, a, "PUT", "/api/admin/security-config", admin, Object{"block_duration": 120, "max_attempts": 5, "attempt_window": 60}, 200)
	mustStatus(t, a, "DELETE", "/api/admin/mailboxes/"+id+"?soft=false", admin, nil, 200)
	mustStatus(t, a, "GET", "/api/admin/mailboxes/"+id, admin, nil, 404)
}

func TestSenderFilterLimitsAndExpiredMailbox(t *testing.T) {
	a, s := testApp(t)
	m := createTestMailbox(t, s, "filtered@example.test")
	id := str(m["id"])
	if e := s.UpdateMailbox(id, Object{"sender_whitelist": []string{"@allowed.test"}, "whitelist_enabled": true}, "UPDATE", "test"); e != nil {
		t.Fatal(e)
	}
	if e := s.Deliver([]string{str(m["address"])}, testEmail()); e == nil {
		t.Fatal("sender whitelist bypass")
	}
	email := testEmail()
	email["From"] = "sender@allowed.test"
	s.Config.MaxEmails = 2
	for i := 0; i < 3; i++ {
		if e := s.Deliver([]string{str(m["address"])}, email); e != nil {
			t.Fatal(e)
		}
	}
	emails, e := s.Emails(id)
	if e != nil || len(emails) != 2 {
		t.Fatalf("count limit: %d %v", len(emails), e)
	}
	email["Body"] = strings.Repeat("x", s.Config.MaxEmailBytes+1)
	if e = s.Deliver([]string{str(m["address"])}, email); e == nil {
		t.Fatal("size limit bypass")
	}
	if _, e = s.DB.Exec("UPDATE mailboxes SET expires_at=? WHERE id=?", time.Now().Unix()-1, id); e != nil {
		t.Fatal(e)
	}
	mustStatus(t, a, "GET", "/api/mailbox_info_v2", str(m["access_token"]), nil, 410)
	if e = s.Cleanup(time.Now()); e != nil {
		t.Fatal(e)
	}
	rows, e := s.Emails(id)
	if e != nil || len(rows) != 0 {
		t.Fatal("expired emails left behind")
	}
}

func TestLegacySubAdminTableAndJSONRollback(t *testing.T) {
	a, s := testApp(t)
	if _, e := s.DB.Exec("ALTER TABLE sub_admins ADD COLUMN password_hash TEXT NOT NULL DEFAULT ''"); e != nil {
		t.Fatal(e)
	}
	result := mustStatus(t, a, "POST", "/api/admin/sub-admins", a.Config.Password, Object{"token": "legacy-sub-token", "domains": []string{"example.test"}}, 200).(map[string]any)["data"].(map[string]any)
	id := str(result["id"])
	mustStatus(t, a, "PUT", "/api/admin/sub-admins/"+id, a.Config.Password, Object{"is_active": false, "notes": "updated"}, 200)
	mustStatus(t, a, "DELETE", "/api/admin/sub-admins/"+id, a.Config.Password, nil, 200)
	path := filepath.Join(t.TempDir(), "invalid.json")
	raw := jsonText(Object{"rollback@example.test": Object{"emails": []Object{{"id": "duplicate", "Body": "one"}, {"id": "duplicate", "Body": "two"}}}})
	if e := os.WriteFile(path, []byte(raw), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ImportJSON(path); e == nil {
		t.Fatal("expected import error")
	}
	if _, e := s.Mailbox("address", "rollback@example.test"); e != sql.ErrNoRows {
		t.Fatal("partial JSON import committed")
	}
}
