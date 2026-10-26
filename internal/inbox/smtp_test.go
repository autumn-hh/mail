package inbox_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maildrop/internal/config"
	"maildrop/internal/inbox"
	"maildrop/internal/smtpd"
	"maildrop/internal/store"
)

func TestSMTPDeliverySenderRulesAndRecipientDomainCase(t *testing.T) {
	cfg := config.FromEnv(func(string) string { return "" })
	cfg.EnableIPWhitelist = false
	db, err := store.OpenWithConfig(filepath.Join(t.TempDir(), "smtp.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	svc := inbox.New(cfg, db, time.Now)
	for _, address := range []string{"lower@example.com", "legacy@ExAmPlE.CoM"} {
		if _, err := svc.CreateOrGetMailbox(context.Background(), address, inbox.CreateMailboxOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := smtpd.New(cfg, svc, nil)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("SMTP server did not stop")
		}
	})
	for _, tc := range []struct {
		name, address, recipient, from, domains, whitelist string
		enabled                                            bool
		code                                               int
	}{
		{"bare domain and lowercase storage", "lower@example.com", "lower@EXAMPLE.COM", "Sender@ALLOWED.EXAMPLE", `["allowed.example"]`, `["Allowed.Example"]`, true, 250},
		{"bare domain and legacy storage", "legacy@ExAmPlE.CoM", "legacy@EXAMPLE.COM", "Sender@ALLOWED.EXAMPLE", `["allowed.example"]`, `["allowed.example"]`, true, 250},
		{"subdomain with whitelist disabled", "lower@example.com", "lower@EXAMPLE.COM", "sender@sub.ALLOWED.EXAMPLE.", `["Allowed.Example"]`, `["unrelated.example"]`, false, 250},
		{"domain restriction without whitelist", "lower@example.com", "lower@EXAMPLE.COM", "sender@other.example", `["allowed.example"]`, "[]", false, 550},
		{"whitelist passes domain rejects", "legacy@ExAmPlE.CoM", "legacy@EXAMPLE.COM", "sender@other.example", `["allowed.example"]`, `["other.example"]`, true, 550},
		{"domain passes whitelist rejects", "lower@example.com", "lower@EXAMPLE.COM", "sender@allowed.example", `["allowed.example"]`, `["other@allowed.example"]`, true, 550},
		{"local part stays exact", "lower@example.com", "Lower@EXAMPLE.COM", "sender@allowed.example", "[]", "[]", false, 550},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.DB().Exec("DELETE FROM emails"); err != nil {
				t.Fatal(err)
			}
			if _, err := db.DB().Exec(`UPDATE mailboxes SET storage_used = 0, allowed_domains = ?,
 sender_whitelist = ?, whitelist_enabled = ? WHERE address = ?`, tc.domains, tc.whitelist, tc.enabled, tc.address); err != nil {
				t.Fatal(err)
			}
			conn, err := net.DialTimeout("tcp", listener.Addr().String(), 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(8 * time.Second)); err != nil {
				t.Fatal(err)
			}
			client, err := smtp.NewClient(conn, "localhost")
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if err := client.Hello("sender.example"); err != nil {
				t.Fatal(err)
			}
			// 信封退信域名不参与头部 From 的限制。
			if err := client.Mail("bounce@unrelated.example"); err != nil {
				t.Fatal(err)
			}
			if err := client.Rcpt(tc.recipient); err != nil {
				t.Fatal(err)
			}
			writer, err := client.Data()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(writer, "From: Sender <"+tc.from+">\r\nTo: elsewhere@example.net\r\nSubject: policies\r\n\r\nbody"); err != nil {
				t.Fatal(err)
			}
			err = writer.Close()
			if tc.code == 250 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var reply *textproto.Error
				reason := fmt.Sprintf("Sender %s not allowed for mailbox %s", tc.from, tc.recipient)
				if tc.name == "local part stays exact" {
					reason = "Mailbox " + tc.recipient + " does not exist"
				}
				if !errors.As(err, &reply) || reply.Code != tc.code || !strings.Contains(reply.Msg, reason) {
					t.Fatalf("SMTP reply = %v, want %d %q", err, tc.code, reason)
				}
			}
			emails, err := svc.GetInboxEmails(context.Background(), tc.address)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.code == 250 {
				want = 1
			}
			if len(emails) != want {
				t.Fatalf("stored emails = %d, want %d", len(emails), want)
			}
			if want == 1 && (emails[0].From != tc.from || emails[0].To != "elsewhere@example.net") {
				t.Fatalf("stored headers = %+v", emails[0])
			}
			if err := client.Quit(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
