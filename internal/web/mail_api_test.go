package web_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maildrop/internal/ipguard"
	"maildrop/internal/mailparse"
	"maildrop/internal/outbound"
)

func TestRawAndAttachmentPerRequestOwnership(t *testing.T) {
	f := newPublicFixture(t)
	owner := f.mailbox(t, "owner@example.com")
	other := f.mailbox(t, "other@example.com")
	raw := []byte("From: a@sender.test\r\nTo: ignored@sender.test\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nbody\r\n--x\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=test.bin\r\nContent-Transfer-Encoding: base64\r\n\r\nAAH/\r\n--x--\r\n")
	message, err := mailparse.Parse(raw, f.now)
	if err != nil {
		t.Fatal(err)
	}
	message.Details.Authentication = &mailparse.Authentication{SPF: mailparse.AuthResult{Result: "fail"}, DMARC: mailparse.AuthResult{Result: "temperror"}}
	delivery, err := f.service.Deliver(context.Background(), "", []string{owner.Address}, message)
	if err != nil || delivery.Accepted != 1 {
		t.Fatal(delivery, err)
	}
	content := publicJSON(t, f.request(t, "GET", "/api/emails/"+message.ID+"/content", "", publicBearer(owner), 200))
	if content["authentication"] == nil || len(content["attachments"].([]any)) != 1 {
		t.Fatal(content)
	}
	for _, suffix := range []string{"/raw", "/attachments/1", "/content"} {
		f.request(t, "GET", "/api/emails/"+message.ID+suffix, "", publicBearer(other), 404)
		f.request(t, "GET", "/api/emails/"+message.ID+suffix+"?token="+owner.AccessToken, "", nil, 401)
	}
	for _, tc := range []struct {
		suffix string
		want   []byte
	}{{"/raw", raw}, {"/attachments/1", []byte{0, 1, 255}}} {
		request := httptest.NewRequest("GET", "/api/emails/"+message.ID+tc.suffix, nil)
		request.Header.Set("Authorization", "Bearer "+owner.AccessToken)
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), tc.want) || !strings.HasPrefix(response.Header().Get("Content-Disposition"), "attachment;") || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("download %s headers=%v body=%q", tc.suffix, response.Header(), response.Body.Bytes())
		}
	}
	if _, err = f.db.DB().Exec(`UPDATE mailboxes SET access_token='rotated' WHERE id=?`, owner.ID); err != nil {
		t.Fatal(err)
	}
	f.request(t, "GET", "/api/emails/"+message.ID+"/raw", "", publicBearer(owner), 401)
}

func TestOnlyMainAdminGrantsSendingAndSenderIsFixed(t *testing.T) {
	f := newPublicFixture(t)
	owner := f.mailbox(t, "owner@example.com")
	other := f.mailbox(t, "other@example.com")
	f.cfg.OutboundEnabled = true
	f.cfg.SMTPHostname = "mail.example.com"
	f.cfg.DKIMKeyDir = t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.cfg.DKIMKeyDir, "example.com.pem"), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	f.resetHandler(ipguard.NewWhitelist(false, ""))
	path := "/api/admin/mailboxes/" + owner.ID + "/sending-policy"
	policy := `{"enabled":true,"daily_limit":100,"minute_limit":10}`
	f.request(t, "PUT", path, policy, publicBearer(owner), 401)
	f.request(t, "PUT", path, policy, map[string]string{"X-Sub-Admin-Token": "fixture-sub-token"}, 401)
	request := `{"to":["dest@recipient.test"],"subject":"subject","text":"hello"}`
	headers := publicBearer(owner)
	headers["Idempotency-Key"] = "web-send"
	f.request(t, "POST", "/api/send", request, headers, 403)
	adminHeaders := map[string]string{"Authorization": "Bearer " + f.cfg.Password}
	f.request(t, "PUT", path, policy, adminHeaders, 200)
	f.request(t, "POST", "/api/send", `{"from":"spoof@attacker.test","to":["dest@recipient.test"]}`, headers, 400)
	first := publicJSON(t, f.request(t, "POST", "/api/send", request, headers, 202))
	second := publicJSON(t, f.request(t, "POST", "/api/send", request, headers, 202))
	if first["id"] != second["id"] {
		t.Fatal("idempotency failed")
	}
	id := first["id"].(string)
	for _, path := range []string{"/api/sent/" + id, "/api/sent/" + id + "/raw", "/api/sent/" + id + "/attachments/1"} {
		f.request(t, "GET", path, "", publicBearer(other), 404)
	}
	f.request(t, "DELETE", "/api/sent/"+id, "", publicBearer(other), 404)
	f.request(t, "PUT", path, `{"enabled":false,"daily_limit":100,"minute_limit":10}`, adminHeaders, 200)
	headers["Idempotency-Key"] = "after-revoke"
	f.request(t, "POST", "/api/send", request, headers, 403)
	record := publicJSON(t, f.request(t, "GET", "/api/sent/"+id, "", publicBearer(owner), 200))
	if record["status"] != "failed" {
		t.Fatal(record)
	}
	p, err := outbound.New(f.cfg, f.db, func() time.Time { return f.now }).Policy(context.Background(), owner.ID)
	if err != nil || p.DailyUsed != 1 {
		t.Fatal(p, err)
	}
}
