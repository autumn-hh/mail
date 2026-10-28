package smtpd

import (
	"context"
	"errors"
	"io"
	"net"
	stdsmtp "net/smtp"
	"net/textproto"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maildrop/internal/config"
	"maildrop/internal/inbox"
	"maildrop/internal/ipguard"
	"maildrop/internal/store"
)

type smtpFixture struct {
	server  *Server
	service *inbox.Service
	db      *store.Store
	address string
	cfg     *config.Config
}

func newSMTPFixture(t *testing.T, adjust func(*config.Config)) smtpFixture {
	t.Helper()
	cfg := config.FromEnv(func(string) string { return "" })
	cfg.MaxEmailSizeBytes = 512
	if adjust != nil {
		adjust(cfg)
	}
	db, err := store.OpenWithConfig(filepath.Join(t.TempDir(), "smtp.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	svc := inbox.New(cfg, db, time.Now)
	if _, err := svc.CreateOrGetMailbox(context.Background(), "in@test.example", inbox.CreateMailboxOptions{}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	srv := New(cfg, svc, ipguard.NewWhitelist(cfg.EnableIPWhitelist, cfg.IPWhitelist))
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Serve did not stop")
		}
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return smtpFixture{srv, svc, db, listener.Addr().String(), cfg}
}

func (f smtpFixture) client(t *testing.T) *stdsmtp.Client {
	t.Helper()
	conn, err := net.DialTimeout("tcp", f.address, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(8 * time.Second)); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	client, err := stdsmtp.NewClient(conn, "localhost")
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Hello("sender.example"); err != nil {
		t.Fatal(err)
	}
	return client
}

func send(client *stdsmtp.Client, recipients []string, raw string) error {
	if err := client.Mail("sender@origin.example"); err != nil {
		return err
	}
	for _, recipient := range recipients {
		if err := client.Rcpt(recipient); err != nil {
			return err
		}
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(writer, raw); err != nil {
		return err
	}
	return writer.Close()
}

func TestSMTPRealTCPDelivery(t *testing.T) {
	for _, tc := range []struct {
		name       string
		recipients []string
		whitelist  bool
		raw        string
		code       int
		reason     string
		count      int
	}{
		{"accepted bcc", []string{"in@test.example"}, false, "From: sender@origin.example\r\nSubject: bcc\r\n\r\nhello", 0, "", 1},
		{"forwarded", []string{"in@test.example"}, false, "From: sender@origin.example\r\nTo: external@other.example\r\n\r\nforwarded", 0, "", 1},
		{"mixed", []string{"unknown@test.example", "in@test.example"}, false, "From: sender@origin.example\r\n\r\nhello", 0, "", 1},
		{"unknown recipient", []string{"unknown@test.example"}, false, "From: sender@origin.example\r\n\r\nhello", 550, "Mailbox unknown@test.example does not exist", 0},
		{"IP whitelist", []string{"in@test.example"}, true, "From: sender@origin.example\r\n\r\nhello", 550, "Access denied - IP not whitelisted", 0},
		{"parsed oversize", []string{"in@test.example"}, false, "From: sender@origin.example\r\n\r\n" + strings.Repeat("a", 600), 550, "Failed to save email: 邮件大小", 0},
		{"raw oversize", []string{"in@test.example"}, false, "Content-Type: application/octet-stream\r\n\r\n" + strings.Repeat(strings.Repeat("a", 998)+"\r\n", 1060), 552, "Maximum message size exceeded", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSMTPFixture(t, func(cfg *config.Config) { cfg.EnableIPWhitelist = tc.whitelist; cfg.IPWhitelist = "192.0.2.0/24" })
			client := f.client(t)
			if enabled, _ := client.Extension("AUTH"); enabled {
				t.Fatal("AUTH advertised")
			}
			err := send(client, tc.recipients, tc.raw)
			if tc.code == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var smtpErr *textproto.Error
				if !errors.As(err, &smtpErr) || smtpErr.Code != tc.code || !strings.Contains(smtpErr.Msg, tc.reason) {
					t.Fatalf("SMTP error=%v, want %d %q", err, tc.code, tc.reason)
				}
			}
			emails, err := f.service.GetInboxEmails(context.Background(), "in@test.example")
			if err != nil || len(emails) != tc.count {
				t.Fatalf("emails=%+v err=%v", emails, err)
			}
			if tc.name == "accepted bcc" && emails[0].To != "" {
				t.Fatalf("Bcc changed header To=%q", emails[0].To)
			}
			if err := client.Quit(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSMTPSessionResetAndRecipientLimit(t *testing.T) {
	f := newSMTPFixture(t, nil)
	client := f.client(t)
	for range 2 {
		if err := send(client, []string{"in@test.example"}, "From: sender@origin.example\r\n\r\nbody"); err != nil {
			t.Fatal(err)
		}
	}
	emails, err := f.service.GetInboxEmails(context.Background(), "in@test.example")
	if err != nil || len(emails) != 2 {
		t.Fatalf("emails=%d err=%v", len(emails), err)
	}
	if err := client.Mail("sender@origin.example"); err != nil {
		t.Fatal(err)
	}
	for range maxRecipients {
		if err := client.Rcpt("in@test.example"); err != nil {
			t.Fatal(err)
		}
	}
	var smtpErr *textproto.Error
	if err := client.Rcpt("in@test.example"); !errors.As(err, &smtpErr) || smtpErr.Code != 452 {
		t.Fatalf("recipient limit=%v", err)
	}
	if err := client.Reset(); err != nil {
		t.Fatal(err)
	}
	if err := client.Quit(); err != nil {
		t.Fatal(err)
	}
}

func TestSMTPInternalErrorIsRetryable(t *testing.T) {
	f := newSMTPFixture(t, nil)
	client := f.client(t)
	if _, err := f.db.DB().Exec(`CREATE TRIGGER reject_email BEFORE INSERT ON emails BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	err := send(client, []string{"in@test.example"}, "From: sender@origin.example\r\n\r\nbody")
	var smtpErr *textproto.Error
	if !errors.As(err, &smtpErr) || smtpErr.Code != 451 || !strings.Contains(smtpErr.Msg, "Could not process email") {
		t.Fatalf("internal error=%v", err)
	}
	emails, err := f.service.GetInboxEmails(context.Background(), "in@test.example")
	if err != nil || len(emails) != 0 {
		t.Fatalf("emails=%d err=%v", len(emails), err)
	}
	if err := client.Quit(); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownDeadlineClosesActiveConnection(t *testing.T) {
	cfg := config.FromEnv(func(string) string { return "" })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, nil, nil)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	greeting := make([]byte, 1024)
	if _, err := conn.Read(greeting); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := srv.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown=%v", err)
	}
	if _, err := conn.Read(greeting); err == nil {
		t.Fatal("active connection stayed open")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("active connection only timed out instead of closing")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not stop")
	}
}
