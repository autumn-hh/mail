package outbound

import (
	"context"
	"errors"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"testing"
	"time"

	"maildrop/internal/inbox"
	"maildrop/internal/smtpd"
)

func TestBounceThroughLocalSMTP(t *testing.T) {
	f := setup(t)
	f.enable(t, 100, 10)
	sent := f.submit(t, "smtp-bounce", simpleRequest())
	f.s.transport = transportFunc(func(context.Context, string, string, []byte) error { return nil })
	f.process(t)
	var address string
	if err := f.db.DB().QueryRow(`SELECT bounce_address FROM outbound_messages WHERE id=?`, sent.ID).Scan(&address); err != nil {
		t.Fatal(err)
	}
	server := smtpd.New(f.cfg, inbox.New(f.cfg, f.db, f.s.now), nil)
	server.SetBounceHandler(f.s)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, net.ErrClosed) {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Error("SMTP server did not stop")
		}
	}()
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	client, err := smtp.NewClient(conn, "localhost")
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	defer client.Close()
	if err = client.Mail(""); err != nil {
		t.Fatal(err)
	}
	var reply *textproto.Error
	if err = client.Rcpt("md-bounce-not-a-real-request@example.com"); !errors.As(err, &reply) || reply.Code != 550 {
		t.Fatal("unknown bounce address accepted", err)
	}
	if err = client.Rcpt(address); err != nil {
		t.Fatal(err)
	}
	if err = client.Rcpt(f.mailbox.Address); !errors.As(err, &reply) || reply.Code != 553 {
		t.Fatal("bounce mixed with normal delivery", err)
	}
	writer, err := client.Data()
	if err != nil {
		t.Fatal(err)
	}
	raw := "From: MAILER-DAEMON@recipient.test\r\nSubject: delayed failure\r\nContent-Type: multipart/report; report-type=delivery-status; boundary=r\r\n\r\n--r\r\nContent-Type: text/plain\r\n\r\nFailure\r\n--r\r\nContent-Type: message/delivery-status\r\n\r\nReporting-MTA: dns; mx.recipient.test\r\n\r\nFinal-Recipient: rfc822; a@recipient.test\r\nAction: failed\r\nStatus: 5.1.1\r\n\r\n--r--\r\n"
	if _, err = io.WriteString(writer, raw); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	record, err := f.s.Get(context.Background(), f.mailbox.ID, sent.ID)
	if err != nil || record.Status != "bounced" || record.BounceCount != 1 {
		t.Fatal(record, err)
	}
	var inboxCount int
	if err = f.db.DB().QueryRow(`SELECT COUNT(*) FROM emails`).Scan(&inboxCount); err != nil || inboxCount != 0 {
		t.Fatal("report leaked into normal inbox", inboxCount, err)
	}
	if err = client.Quit(); err != nil {
		t.Fatal(err)
	}
}
