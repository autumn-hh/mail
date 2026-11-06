package mcpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"maildrop/internal/config"
	"maildrop/internal/mailparse"
)

func setupSending(t *testing.T, enabled bool) *fixture {
	t.Helper()
	keyDir := t.TempDir()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(keyDir, "example.test.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	return setup(t, func(cfg *config.Config) {
		cfg.OutboundEnabled = enabled
		cfg.SMTPHostname = "mail.example.test"
		cfg.DKIMKeyDir = keyDir
		cfg.VerifyInbound = false
	})
}

func expectOutboundError(t *testing.T, f *fixture, name string, args any, want string) {
	t.Helper()
	r, err := f.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "maildrop_" + name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(r.Content)
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsError || !strings.Contains(string(raw), want) {
		t.Fatalf("%s: expected %s, got %s", name, want, raw)
	}
}

func TestMCPOutboundWorkflow(t *testing.T) {
	f := setupSending(t, true)
	ctx := context.Background()
	tools, err := f.session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
		if tool.Name == "maildrop_send_email" {
			if tool.Annotations == nil || tool.Annotations.OpenWorldHint == nil || !*tool.Annotations.OpenWorldHint || !tool.Annotations.IdempotentHint || tool.Annotations.ReadOnlyHint {
				t.Fatal("wrong sending annotations")
			}
		}
	}
	for _, name := range []string{"get_sending_policy", "set_sending_policy", "send_email", "list_sent_emails", "get_sent_email", "delete_sent_email"} {
		if !names["maildrop_"+name] {
			t.Fatalf("missing %s", name)
		}
	}
	info := f.call(t, "server_info", map[string]any{})
	if info["tool_count"] != float64(41) || info["receive_only"] != false || info["outbound_enabled"] != true {
		t.Fatal(info)
	}
	box := f.call(t, "create_mailbox", map[string]any{"address": "sender@example.test"})
	id := box["id"].(string)
	other := f.call(t, "create_mailbox", map[string]any{"address": "other@example.test"})["id"].(string)
	args := map[string]any{"mailbox": id, "idempotency_key": "one", "to": []string{"recipient@remote.invalid"}, "subject": "hello", "text": "你好世界", "html": "<p>hello</p>"}
	if f.call(t, "get_sending_policy", map[string]any{"mailbox": id})["enabled"] != false {
		t.Fatal("sending enabled by default")
	}
	expectOutboundError(t, f, "send_email", args, "403:")
	policyArgs := map[string]any{"mailbox": id, "enabled": true, "daily_limit": 3, "minute_limit": 3}
	f.call(t, "set_sending_policy", policyArgs)
	var actorName, ip string
	if err := f.db.DB().QueryRow("SELECT admin_user,ip_address FROM audit_logs WHERE action=? AND mailbox_id=?", "SENDING_POLICY", id).Scan(&actorName, &ip); err != nil {
		t.Fatal(err)
	}
	if actorName != "mcp-superadmin" || ip != "127.0.0.1" {
		t.Fatalf("wrong policy actor %s %s", actorName, ip)
	}
	args["bcc"] = []string{"private@remote.invalid"}
	args["attachments"] = []map[string]any{{"filename": "hello.txt", "content": base64.StdEncoding.EncodeToString([]byte("attachment"))}}
	if err := f.send("sender@example.test", "original", "reply body"); err != nil {
		t.Fatal(err)
	}
	emailID := f.call(t, "list_emails", map[string]any{"mailbox": id})["emails"].([]any)[0].(map[string]any)["id"].(string)
	args["reply_email_id"] = emailID
	sent := f.call(t, "send_email", args)["email"].(map[string]any)
	sentID := sent["id"].(string)
	if sent["status"] != "queued" {
		t.Fatal(sent)
	}
	if f.call(t, "send_email", args)["email"].(map[string]any)["id"] != sentID {
		t.Fatal("duplicate queue entry")
	}
	policy := f.call(t, "get_sending_policy", map[string]any{"mailbox": id})
	if policy["daily_used"] != float64(2) || policy["minute_used"] != float64(2) {
		t.Fatal(policy)
	}
	args["subject"] = "different"
	expectOutboundError(t, f, "send_email", args, "409:")
	args["subject"] = "hello"
	args["idempotency_key"] = "two"
	expectOutboundError(t, f, "send_email", args, "429:")
	args["mailbox"] = other
	f.call(t, "set_sending_policy", map[string]any{"mailbox": other, "enabled": true, "daily_limit": 3, "minute_limit": 3})
	expectOutboundError(t, f, "send_email", args, "404:")
	expectOutboundError(t, f, "get_sent_email", map[string]any{"mailbox": other, "sent_id": sentID}, "404:")
	expectOutboundError(t, f, "delete_sent_email", map[string]any{"mailbox": other, "sent_id": sentID}, "404:")
	listed := f.call(t, "list_sent_emails", map[string]any{"mailbox": id})["emails"].([]any)
	if len(listed) != 1 || listed[0].(map[string]any)["details"].(map[string]any)["text"] != nil {
		t.Fatal("list leaked body or duplicated")
	}
	got := f.call(t, "get_sent_email", map[string]any{"mailbox": id, "sent_id": sentID, "max_body_chars": 2})
	if got["body_truncated"] != true || got["text_chars"] != float64(4) || got["email"].(map[string]any)["details"].(map[string]any)["text"] != "你好" {
		t.Fatal(got)
	}
	var raw []byte
	if err := f.db.DB().QueryRow("SELECT raw FROM outbound_messages WHERE id=?", sentID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "DKIM-Signature:") || strings.Contains(string(raw), "private@remote.invalid") || strings.Contains(string(raw), "Bcc:") {
		t.Fatal("signing or Bcc privacy failure")
	}
	parsed, err := mailparse.Parse(raw, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Details.Attachments) != 1 || string(parsed.Details.Attachments[0].Data) != "attachment" {
		t.Fatal("attachment missing")
	}
	expectOutboundError(t, f, "delete_sent_email", map[string]any{"mailbox": id, "sent_id": sentID}, "409:")
	policyArgs["enabled"] = false
	f.call(t, "set_sending_policy", policyArgs)
	if f.call(t, "get_sent_email", map[string]any{"mailbox": id, "sent_id": sentID})["email"].(map[string]any)["status"] != "failed" {
		t.Fatal("revocation did not stop queue")
	}
	args["mailbox"], args["idempotency_key"] = id, "three"
	expectOutboundError(t, f, "send_email", args, "403:")
	f.call(t, "delete_sent_email", map[string]any{"mailbox": id, "sent_id": sentID})
	args["idempotency_key"] = "one"
	expectOutboundError(t, f, "send_email", args, "410:")
	if f.call(t, "get_sending_policy", map[string]any{"mailbox": id})["daily_used"] != float64(2) {
		t.Fatal("quota was refunded")
	}
	var changes string
	if err := f.db.DB().QueryRow("SELECT changes FROM audit_logs WHERE action='MCP_SEND_EMAIL' LIMIT 1").Scan(&changes); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(changes, "private@") || strings.Contains(changes, "你好") {
		t.Fatal("audit leaked content")
	}
}

func TestMCPOutboundValidation(t *testing.T) {
	f := setupSending(t, true)
	id := f.call(t, "create_mailbox", map[string]any{"address": "checks@example.test"})["id"].(string)
	f.call(t, "set_sending_policy", map[string]any{"mailbox": id, "enabled": true, "daily_limit": 100, "minute_limit": 10})
	base := map[string]any{"mailbox": id, "idempotency_key": "check", "to": []string{"target@recipient.invalid"}, "subject": "check"}
	for _, tc := range []struct {
		key   string
		value any
	}{{"from", "other@example.test"}, {"idempotency_key", ""}, {"idempotency_key", strings.Repeat("x", 129)}, {"subject", "bad\r\nBcc: hidden@remote.invalid"}, {"attachments", []map[string]any{{"filename": "a", "content": "!!invalid!!"}}}} {
		t.Run(tc.key+fmt.Sprint(tc.value)[:min(16, len(fmt.Sprint(tc.value)))], func(t *testing.T) {
			args := map[string]any{}
			for k, v := range base {
				args[k] = v
			}
			args[tc.key] = tc.value
			f.expectError(t, "send_email", args)
		})
	}
	f.expectError(t, "set_sending_policy", map[string]any{"mailbox": id, "daily_limit": 100, "minute_limit": 10})
	f.expectError(t, "set_sending_policy", map[string]any{"mailbox": id, "enabled": true, "daily_limit": 0, "minute_limit": 10})
	f.expectError(t, "list_sent_emails", map[string]any{"mailbox": id, "offset": -1})
	f.expectError(t, "get_sent_email", map[string]any{"mailbox": id, "sent_id": "missing", "max_body_chars": 200001})
	// Large attachment crosses the former 1 MiB MCP limit without returning its bytes.
	base["attachments"] = []map[string]any{{"filename": "large.bin", "content": base64.StdEncoding.EncodeToString(make([]byte, 900000))}}
	out := f.call(t, "send_email", base)
	encoded, _ := json.Marshal(out)
	if len(encoded) > 5000 {
		t.Fatal("attachment bytes leaked in response")
	}
	f.call(t, "set_mailbox_status", map[string]any{"mailbox": id, "enabled": false})
	expectOutboundError(t, f, "send_email", base, "423:")
	f.call(t, "set_mailbox_status", map[string]any{"mailbox": id, "enabled": true})
	if _, err := f.db.DB().Exec("UPDATE mailboxes SET expires_at=1 WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	expectOutboundError(t, f, "send_email", base, "423:")
}

func TestMCPOutboundDisabledAndMissingDKIM(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			f := setup(t, func(cfg *config.Config) {
				cfg.OutboundEnabled = enabled
				cfg.SMTPHostname = "mail.example.test"
				cfg.DKIMKeyDir = t.TempDir()
			})
			id := f.call(t, "create_mailbox", map[string]any{"address": "disabled@example.test"})["id"].(string)
			f.call(t, "set_sending_policy", map[string]any{"mailbox": id, "enabled": true, "daily_limit": 100, "minute_limit": 10})
			expectOutboundError(t, f, "send_email", map[string]any{"mailbox": id, "idempotency_key": "disabled", "to": []string{"a@remote.invalid"}, "subject": "none"}, "503:")
			var count int
			if err := f.db.DB().QueryRow("SELECT COUNT(*) FROM outbound_requests").Scan(&count); err != nil || count != 0 {
				t.Fatal("failed request consumed quota", err, count)
			}
		})
	}
}

func TestMCPOutboundRetryAfterAuditFailure(t *testing.T) {
	f := setupSending(t, true)
	id := f.call(t, "create_mailbox", map[string]any{"address": "audit@example.test"})["id"].(string)
	f.call(t, "set_sending_policy", map[string]any{"mailbox": id, "enabled": true, "daily_limit": 10, "minute_limit": 10})
	if _, err := f.db.DB().Exec("CREATE TRIGGER fail_send_audit BEFORE INSERT ON audit_logs WHEN NEW.action='MCP_SEND_EMAIL' BEGIN SELECT RAISE(ABORT,'private-db-failure'); END"); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"mailbox": id, "idempotency_key": "stable", "to": []string{"a@recipient.invalid"}, "subject": "audit"}
	expectOutboundError(t, f, "send_email", args, "Operation failed")
	var count, recipients int
	var sentID string
	if err := f.db.DB().QueryRow("SELECT COUNT(*),MAX(message_id),SUM(recipient_count) FROM outbound_requests").Scan(&count, &sentID, &recipients); err != nil || count != 1 || recipients != 1 {
		t.Fatal("enqueue lost or doubled", count, recipients, err)
	}
	if _, err := f.db.DB().Exec("DROP TRIGGER fail_send_audit"); err != nil {
		t.Fatal(err)
	}
	if f.call(t, "send_email", args)["email"].(map[string]any)["id"] != sentID {
		t.Fatal("audit retry enqueued a second message")
	}
	if f.call(t, "get_sending_policy", map[string]any{"mailbox": id})["daily_used"] != float64(1) {
		t.Fatal("audit retry charged twice")
	}
}
