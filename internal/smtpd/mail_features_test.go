package smtpd

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"testing"
	"time"

	"maildrop/internal/config"
	"maildrop/internal/inbox"
	"maildrop/internal/mailauth"
)

func TestSTARTTLSCertificateAndReset(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	os.WriteFile(certPath, encoded, 0600)
	os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600)
	f := newSMTPFixture(t, func(cfg *config.Config) { cfg.SMTPTLSCertFile = certPath; cfg.SMTPTLSKeyFile = keyPath })
	client := f.client(t)
	if ok, _ := client.Extension("STARTTLS"); !ok {
		t.Fatal("STARTTLS not advertised")
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(encoded)
	if err := client.StartTLS(&tls.Config{ServerName: "localhost", RootCAs: roots, MinVersion: tls.VersionTLS12}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := client.Extension("STARTTLS"); ok {
		t.Fatal("STARTTLS still advertised over TLS")
	}
	if err := send(client, []string{"in@test.example"}, "From: sender@test.example\r\n\r\nTLS body"); err != nil {
		t.Fatal(err)
	}
}
func TestTLSConfigurationFailureClosesListener(t *testing.T) {
	cfg := config.FromEnv(func(string) string { return "" })
	cfg.SMTPTLSCertFile = filepath.Join(t.TempDir(), "missing.pem")
	server := New(cfg, nil, nil)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err = server.Serve(listener); err == nil {
		t.Fatal("invalid TLS configuration accepted")
	}
	if conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("listener leaked on TLS failure")
	}
}
func TestDATARechecksEveryRecipientAtomically(t *testing.T) {
	for _, mode := range []string{"disabled", "sender-policy", "capacity"} {
		t.Run(mode, func(t *testing.T) {
			f := newSMTPFixture(t, nil)
			second, err := f.service.CreateOrGetMailbox(context.Background(), "second@test.example", inbox.CreateMailboxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			client := f.client(t)
			if err = client.Mail("sender@origin.example"); err != nil {
				t.Fatal(err)
			}
			for _, address := range []string{"in@test.example", second.Address} {
				if err = client.Rcpt(address); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "disabled":
				_, err = f.db.DB().Exec(`UPDATE mailboxes SET is_active=0 WHERE id=?`, second.ID)
			case "sender-policy":
				_, err = f.db.DB().Exec(`UPDATE mailboxes SET whitelist_enabled=1,sender_whitelist='["other@origin.example"]' WHERE id=?`, second.ID)
			case "capacity":
				_, err = f.db.DB().Exec(`UPDATE mailboxes SET storage_limit=1 WHERE id=?`, second.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			writer, err := client.Data()
			if err != nil {
				t.Fatal(err)
			}
			io.WriteString(writer, "From: sender@origin.example\r\n\r\nbody")
			err = writer.Close()
			var reply *textproto.Error
			if !errors.As(err, &reply) || reply.Code < 400 {
				t.Fatal("DATA reported success", err)
			}
			var count int
			if err = f.db.DB().QueryRow(`SELECT COUNT(*) FROM emails`).Scan(&count); err != nil || count != 0 {
				t.Fatal("partial batch committed", count, err)
			}
			if err = f.db.DB().QueryRow(`SELECT COUNT(*) FROM mail_content`).Scan(&count); err != nil || count != 0 {
				t.Fatal("raw content leaked", count, err)
			}
		})
	}
}

type authResolver struct {
	*net.Resolver
	fail bool
}

func (r authResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if r.fail {
		return nil, &net.DNSError{IsTemporary: true, Name: name, Err: "fixture DNS failure"}
	}
	if name == "_dmarc.origin.example" {
		return []string{"v=DMARC1; p=reject"}, nil
	}
	return []string{"v=spf1 -all"}, nil
}
func TestAuthenticationFailureStillReceivedAndStored(t *testing.T) {
	for _, dnsFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "validation fails", true: "DNS unavailable"}[dnsFailure], func(t *testing.T) {
			f := newSMTPFixture(t, nil)
			backend := f.server.smtp.Backend.(*backend)
			backend.verify = true
			backend.verifier = mailauth.Verifier{Resolver: authResolver{Resolver: net.DefaultResolver, fail: dnsFailure}}
			client := f.client(t)
			if err := send(client, []string{"in@test.example"}, "From: sender@origin.example\r\nAuthentication-Results: forged; spf=pass\r\n\r\nbody"); err != nil {
				t.Fatal(err)
			}
			mails, err := f.service.GetInboxEmails(context.Background(), "in@test.example")
			if err != nil || len(mails) != 1 {
				t.Fatal(mails, err)
			}
			mailbox, _ := f.service.GetMailboxInfo(context.Background(), "in@test.example")
			content, err := f.db.ContentForEmail(context.Background(), mailbox.ID, mails[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			expected := "fail"
			if dnsFailure {
				expected = "temperror"
			}
			if content.Details.Authentication == nil || content.Details.Authentication.DMARC.Result != expected {
				t.Fatal(content.Details.Authentication)
			}
		})
	}
}
