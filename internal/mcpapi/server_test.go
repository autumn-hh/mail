package mcpapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"maildrop/internal/config"
	"maildrop/internal/inbox"
	"maildrop/internal/ipguard"
	"maildrop/internal/smtpd"
	"maildrop/internal/store"
)

const testToken = "test-mcp-superadmin-01234567890123456789"

func TestMCPCancellationLimitsAndAudit(t *testing.T) {
	f := setup(t)
	f.call(t, "create_mailbox", map[string]any{"address": "cancel@example.test"})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := f.session.CallTool(ctx, &mcp.CallToolParams{Name: "maildrop_wait_for_email", Arguments: map[string]any{"mailbox": "cancel@example.test", "timeout_seconds": 25}})
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("cancellation failed: %v", err)
	}
	f.call(t, "health", map[string]any{})
	var who, ip string
	if err := f.db.DB().QueryRow("SELECT admin_user,ip_address FROM audit_logs WHERE action='CREATE' LIMIT 1").Scan(&who, &ip); err != nil {
		t.Fatal(err)
	}
	if who != "mcp-superadmin" || ip != "127.0.0.1" {
		t.Fatalf("lost actor: %q %q", who, ip)
	}
	for _, args := range []map[string]any{{"mailbox": "cancel@example.test", "timeout_seconds": 26}, {"mailbox": "cancel@example.test", "after": 5, "before": 3}} {
		f.expectError(t, "wait_for_email", args)
	}
	f.expectError(t, "create_mailbox", map[string]any{"address": "not-an-address"})
	f.expectError(t, "set_mailbox_status", map[string]any{"mailbox": "cancel@example.test", "enabled": "false"})
	f.expectError(t, "create_mailboxes_batch", map[string]any{"items": []any{}})
	f.expectError(t, "search_emails", map[string]any{"mailbox": "cancel@example.test", "page_size": 101})
	request := httptest.NewRequest("POST", "http://localhost/mcp", strings.NewReader(strings.Repeat("x", (1<<20)+1)))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	if response.Code != 413 {
		t.Fatalf("oversize status %d", response.Code)
	}
	logs, err := f.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "maildrop_list_audit_logs", Arguments: map[string]any{}})
	if err != nil || logs.IsError {
		t.Fatal("audit", err)
	}
	raw, _ := json.Marshal(logs.StructuredContent)
	if strings.Contains(string(raw), testToken) {
		t.Fatal("MCP credential leak")
	}
}

type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}

type fixture struct {
	db          *store.Store
	cfg         *config.Config
	blocker     *ipguard.Blocker
	handler     http.Handler
	session     *mcp.ClientSession
	smtpAddress string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	cfg := config.FromEnv(func(k string) string {
		switch k {
		case "MCP_ADMIN_TOKEN":
			return testToken
		case "DOMAINS":
			return "example.test"
		case "PASSWORD":
			return "web-admin-password"
		}
		return ""
	})
	db, err := store.OpenWithConfig(filepath.Join(t.TempDir(), "mailbox.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.SeedDomainsIfEmpty(cfg.Domains); err != nil {
		t.Fatal(err)
	}
	f := &fixture{db: db, cfg: cfg, blocker: ipguard.NewBlocker(300, 100, 60)}
	f.handler = New(cfg, db, f.blocker)
	server := httptest.NewServer(f.handler)
	t.Cleanup(server.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "integration-test", Version: "1"}, nil)
	f.session, err = client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{http.DefaultTransport, testToken}}, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.session.Close() })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	smtpServer := smtpd.New(cfg, inbox.New(cfg, db, nil), ipguard.NewWhitelist(false, ""))
	go smtpServer.Serve(listener)
	f.smtpAddress = listener.Addr().String()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		smtpServer.Shutdown(ctx)
	})
	return f
}
func (f *fixture) call(t *testing.T, name string, args any) map[string]any {
	t.Helper()
	r, err := f.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "maildrop_" + name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if r.IsError {
		t.Fatalf("%s: %v", name, r.GetError())
	}
	raw, err := json.Marshal(r.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s output %s", name, raw)
	}
	return out
}
func (f *fixture) expectError(t *testing.T, name string, args any) {
	t.Helper()
	r, err := f.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "maildrop_" + name, Arguments: args})
	if err == nil && !r.IsError {
		t.Fatalf("%s unexpectedly succeeded", name)
	}
}
func (f *fixture) send(address, subject, body string) error {
	return smtp.SendMail(f.smtpAddress, nil, "sender@remote.test", []string{address}, []byte("From: sender@remote.test\r\nTo: "+address+"\r\nSubject: "+subject+"\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n"+body))
}
func TestMCPMailboxSMTPWorkflow(t *testing.T) {
	f := setup(t)
	tools, err := f.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 35 {
		t.Fatalf("tool count %d", len(tools.Tools))
	}
	for _, tool := range tools.Tools {
		if strings.Contains(tool.Name, "sub_admin") || strings.Contains(tool.Name, "login") {
			t.Fatalf("Unexpected tool: %s", tool.Name)
		}
	}
	f.call(t, "health", map[string]any{})
	f.call(t, "server_info", map[string]any{})
	box := f.call(t, "create_mailbox", map[string]any{"address": "alpha@example.test"})
	id := box["id"].(string)
	address := box["address"].(string)
	if len(box["access_token"].(string)) != 43 {
		t.Fatal("weak token")
	}
	detail := f.call(t, "get_mailbox", map[string]any{"mailbox": id})
	if detail["access_token"] != nil || detail["mailbox_key"] != nil {
		t.Fatal("credential leak")
	}
	if f.call(t, "get_mailbox", map[string]any{"mailbox": id, "include_credentials": true})["access_token"] != box["access_token"] {
		t.Fatal("explicit credentials unavailable")
	}
	f.call(t, "update_mailbox", map[string]any{"mailbox": id, "retention_days": 7, "sender_whitelist": []string{}})
	f.call(t, "set_mailbox_status", map[string]any{"mailbox": id, "enabled": false})
	f.call(t, "set_mailbox_status", map[string]any{"mailbox": id, "enabled": true})
	f.call(t, "add_sender_whitelist", map[string]any{"mailbox": id, "sender": "@remote.test"})
	f.call(t, "set_whitelist_enabled", map[string]any{"mailbox": id, "enabled": true})
	f.call(t, "get_sender_whitelist", map[string]any{"mailbox": id})
	f.call(t, "remove_sender_whitelist", map[string]any{"mailbox": id, "sender": "@remote.test"})
	f.call(t, "set_whitelist_enabled", map[string]any{"mailbox": id, "enabled": false})
	if err := f.send(address, "hello", "你好 MCP"); err != nil {
		t.Fatal(err)
	}
	listing := f.call(t, "search_emails", map[string]any{"mailbox": id, "query": "你好", "page_size": 1})
	if listing["total"].(float64) != 1 {
		t.Fatal(listing)
	}
	emailID := listing["emails"].([]any)[0].(map[string]any)["id"].(string)
	content := f.call(t, "get_email", map[string]any{"mailbox": id, "email_id": emailID, "max_body_chars": 2})
	if content["body_truncated"] != true {
		t.Fatal(content)
	}
	f.call(t, "set_email_read", map[string]any{"mailbox": id, "email_id": emailID, "is_read": true})
	if f.call(t, "list_emails", map[string]any{"mailbox": id, "unread_only": true})["total"].(float64) != 0 {
		t.Fatal("not marked")
	}
	f.call(t, "set_email_read", map[string]any{"mailbox": id, "email_id": emailID, "is_read": false})
	f.call(t, "mark_all_read", map[string]any{"mailbox": id})
	f.call(t, "create_mailbox", map[string]any{"address": "beta@example.test"})
	f.expectError(t, "get_email", map[string]any{"mailbox": "beta@example.test", "email_id": emailID})
	f.expectError(t, "delete_emails", map[string]any{"mailbox": id, "email_ids": []string{emailID, "missing"}})
	if f.call(t, "list_emails", map[string]any{"mailbox": id})["total"].(float64) != 1 {
		t.Fatal("partial deletion")
	}
	go func() { time.Sleep(400 * time.Millisecond); f.send(address, "new mail", "wait success") }()
	waited := f.call(t, "wait_for_email", map[string]any{"mailbox": id, "subject": "new mail", "timeout_seconds": 3, "ignore_ids": []string{emailID}})
	if waited["found"] != true {
		t.Fatal(waited)
	}
	timed := f.call(t, "wait_for_email", map[string]any{"mailbox": id, "subject": "not present", "timeout_seconds": 1})
	if timed["timed_out"] != true {
		t.Fatal(timed)
	}
	newToken := f.call(t, "reset_mailbox_token", map[string]any{"mailbox": id})["access_token"].(string)
	if len(newToken) != 64 || newToken == box["access_token"] {
		t.Fatal("bad rotation")
	}
	if _, err := f.db.GetMailboxByToken(box["access_token"].(string)); err != store.ErrNotFound {
		t.Fatal("old token valid")
	}
	f.call(t, "rotate_mailbox_key", map[string]any{"mailbox": id})
	f.call(t, "get_stats", map[string]any{})
	f.call(t, "get_source_stats", map[string]any{})
	r, err := f.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "maildrop_list_audit_logs", Arguments: map[string]any{"mailbox": id}})
	if err != nil || r.IsError {
		t.Fatal("audit failed", err)
	}
	deleted := f.call(t, "delete_emails", map[string]any{"mailbox": id, "email_ids": []string{emailID}})
	if deleted["deleted"].(float64) != 1 {
		t.Fatal(deleted)
	}
	f.call(t, "delete_mailbox", map[string]any{"mailbox": "beta@example.test", "permanent": true})
}
func TestMCPAuthBoundaries(t *testing.T) {
	f := setup(t)
	for _, test := range []struct {
		name, token, host, origin string
		status                    int
	}{{"missing", "", "localhost", "", 401}, {"web password", f.cfg.Password, "localhost", "", 401}, {"foreign host", testToken, "evil.test", "", 403}, {"foreign origin", testToken, "localhost", "https://evil.test", 403}, {"null origin", testToken, "localhost", "null", 403}} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://localhost/mcp", strings.NewReader("{}"))
			r.Host = test.host
			r.Header.Set("Authorization", "Bearer "+test.token)
			r.Header.Set("Origin", test.origin)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("got %d", w.Code)
			}
		})
	}
	f.cfg.MCPAdminToken = ""
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, httptest.NewRequest("POST", "http://localhost/mcp", nil))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}
func TestMCPDomainsBatchSecurity(t *testing.T) {
	f := setup(t)
	batch := f.call(t, "create_mailboxes_batch", map[string]any{"items": []any{map[string]any{"address": "batch1@example.test"}, map[string]any{"address": "bad@disabled.test"}}})
	if len(batch["results"].([]any)) != 2 {
		t.Fatal(batch)
	}
	f.call(t, "delete_mailboxes_batch", map[string]any{"mailboxes": []string{"batch1@example.test", "missing@example.test"}})
	added := f.call(t, "add_domains", map[string]any{"domains": []string{"SECOND.TEST", "bad..test", "second.test"}})
	if len(added["added"].([]any)) != 1 {
		t.Fatal(added)
	}
	f.call(t, "set_domain_enabled", map[string]any{"domain": "second.test", "enabled": false})
	f.call(t, "set_domain_enabled", map[string]any{"domain": "second.test", "enabled": true})
	f.expectError(t, "delete_domains", map[string]any{"domains": []string{"example.test", "second.test"}})
	f.call(t, "delete_domains", map[string]any{"domains": []string{"second.test"}})
	f.expectError(t, "set_domain_enabled", map[string]any{"domain": "example.test", "enabled": false})
	f.call(t, "get_security_config", map[string]any{})
	f.call(t, "update_security_config", map[string]any{"max_attempts": 5})
	if f.blocker.GetConfig().MaxAttempts != 5 {
		t.Fatal("not updated")
	}
	f.expectError(t, "update_security_config", map[string]any{"max_attempts": 100})
	f.blocker.RecordFailedAttempt("192.0.2.1")
	f.call(t, "unblock_ip", map[string]any{"ip": "192.0.2.1"})
	for _, name := range []string{"list_domains", "list_blocked_ips", "list_audit_logs"} {
		r, err := f.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "maildrop_" + name, Arguments: map[string]any{}})
		if err != nil || r.IsError {
			t.Fatalf("%s %v", name, err)
		}
	}
}
