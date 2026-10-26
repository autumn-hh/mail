package maildrop

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/mail"
	"strings"
	"time"

	message "github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	mimeMail "github.com/emersion/go-message/mail"
	smtp "github.com/emersion/go-smtp"
)

type smtpBackend struct{ store *Store }
type smtpSession struct {
	store      *Store
	from       string
	recipients []string
}

func smtpReject(message string) error { return &smtp.SMTPError{Code: 550, Message: message} }
func (b *smtpBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	ip, _, e := net.SplitHostPort(c.Conn().RemoteAddr().String())
	if e != nil || !b.store.Config.IPAllowed(ip) {
		return nil, smtpReject("IP not whitelisted")
	}
	return &smtpSession{store: b.store}, nil
}
func (s *smtpSession) Reset()        { s.from = ""; s.recipients = nil }
func (s *smtpSession) Logout() error { return nil }
func (s *smtpSession) Mail(from string, _ *smtp.MailOptions) error {
	s.Reset()
	s.from = from
	return nil
}
func (s *smtpSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	domains, e := s.store.Domains()
	if e != nil {
		return temporarySMTP(e)
	}
	p := strings.Split(to, "@")
	if len(p) != 2 || !contains(domains, p[1]) {
		return smtpReject("Recipient domain not enabled")
	}
	m, e := s.store.Mailbox("address", to)
	if errors.Is(e, sql.ErrNoRows) {
		return smtpReject("Mailbox does not exist")
	}
	if e != nil {
		return temporarySMTP(e)
	}
	if truth(m["is_expired"]) || !truth(m["is_active"]) {
		return smtpReject("Mailbox expired or disabled")
	}
	if !contains(s.recipients, to) {
		s.recipients = append(s.recipients, to)
	}
	return nil
}
func temporarySMTP(e error) error {
	slog.Error("SMTP storage operation failed", "error", e)
	return &smtp.SMTPError{Code: 451, Message: "Temporary local storage error"}
}
func (s *smtpSession) Data(r io.Reader) error {
	raw, e := io.ReadAll(io.LimitReader(r, int64(s.store.Config.MaxEmailBytes)+1))
	if e != nil {
		return &smtp.SMTPError{Code: 451, Message: "Unable to read message"}
	}
	if len(raw) > s.store.Config.MaxEmailBytes {
		return &smtp.SMTPError{Code: 552, Message: "Message exceeds size limit"}
	}
	parsed, e := ParseMessage(raw)
	if e != nil {
		return smtpReject("Invalid MIME message")
	}
	e = s.store.Deliver(s.recipients, parsed)
	if e != nil {
		var he *HTTPError
		if errors.As(e, &he) {
			return smtpReject(he.Message)
		}
		return temporarySMTP(e)
	}
	return nil
}
func NewSMTP(s *Store) *smtp.Server {
	server := smtp.NewServer(&smtpBackend{store: s})
	server.Addr = s.Config.SMTPAddr
	server.Domain = "maildrop"
	server.MaxRecipients = 100
	server.MaxMessageBytes = int64(s.Config.MaxEmailBytes)
	server.ReadTimeout = time.Minute
	server.WriteTimeout = time.Minute
	return server
}

// ParseMessage 保留旧 API 字段；MIME 解码由 go-message 处理，附件不作为正文显示。
func ParseMessage(raw []byte) (Object, error) {
	reader, e := mimeMail.CreateReader(bytes.NewReader(raw))
	if e != nil && !message.IsUnknownCharset(e) {
		return nil, e
	}
	if reader == nil {
		return nil, fmt.Errorf("empty message")
	}
	defer reader.Close()
	subject, e := reader.Header.Subject()
	if e != nil {
		subject = reader.Header.Get("Subject")
	}
	if subject == "" {
		subject = "No Subject"
	}
	sender := reader.Header.Get("From")
	if parsed, e := mail.ParseAddress(sender); e == nil {
		sender = parsed.Address
	}
	plain, htmlBody := "", ""
	hasHTML := false
	parts := 0
	for {
		part, e := reader.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil && !message.IsUnknownCharset(e) {
			return nil, e
		}
		if part == nil {
			return nil, fmt.Errorf("invalid MIME part")
		}
		parts++
		if parts > 1000 {
			return nil, fmt.Errorf("too many MIME parts")
		}
		if header, ok := part.Header.(*mimeMail.InlineHeader); ok {
			contentType, _, e := header.ContentType()
			if e != nil {
				return nil, e
			}
			if contentType != "text/plain" && contentType != "text/html" {
				continue
			}
			data, e := io.ReadAll(io.LimitReader(part.Body, int64(len(raw))*4+1))
			if e != nil {
				return nil, e
			}
			if len(data) > len(raw)*4 {
				return nil, fmt.Errorf("decoded body too large")
			}
			if contentType == "text/html" {
				htmlBody = string(data)
				hasHTML = true
			} else {
				plain = string(data)
			}
		}
	}
	contentType, body := "Text", plain
	if hasHTML {
		contentType, body = "HTML", htmlBody
	}
	now := time.Now()
	return Object{"From": sender, "Subject": subject, "Body": body, "ContentType": contentType, "Timestamp": now.Unix(), "Sent": now.UTC().Format("Jan 02 at 15:04:05")}, nil
}
func senderAllowed(m Object, sender string) bool {
	if !truth(m["whitelist_enabled"]) {
		return true
	}
	items := stringsValue(m["sender_whitelist"])
	if len(items) == 0 {
		return true
	}
	p := strings.Split(sender, "@")
	domain := ""
	if len(p) == 2 {
		domain = p[1]
	}
	for _, rule := range items {
		if strings.Contains(strings.TrimPrefix(rule, "@"), "@") {
			if strings.EqualFold(rule, sender) {
				return true
			}
		} else if strings.EqualFold(strings.TrimPrefix(rule, "@"), domain) {
			return true
		}
	}
	return false
}
func (s *Store) Deliver(recipients []string, email Object) error {
	if len(recipients) == 0 {
		return bad("No recipients")
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	setting, e := one(tx, "SELECT value FROM settings WHERE key='domains'")
	if e != nil {
		return e
	}
	domains := stringsValue(setting["value"])
	for _, address := range recipients {
		p := strings.Split(address, "@")
		if len(p) != 2 || !contains(domains, p[1]) {
			return bad("Recipient domain not enabled")
		}
		m, e := one(tx, "SELECT * FROM mailboxes WHERE address=?", address)
		if errors.Is(e, sql.ErrNoRows) {
			return bad("Mailbox does not exist")
		}
		if e != nil {
			return e
		}
		if num(m["expires_at"]) <= time.Now().Unix() || !truth(m["is_active"]) {
			return bad("Mailbox expired or disabled")
		}
		if !senderAllowed(m, str(email["From"])) {
			return bad("Sender not allowed")
		}
		size := len(str(email["From"])) + len(address) + len(str(email["Subject"])) + len(str(email["Body"])) + len(str(email["Sent"]))
		if size > s.Config.MaxEmailBytes {
			return bad("Message exceeds size limit")
		}
		used, e := one(tx, "SELECT COALESCE(SUM(size_bytes),0) AS size FROM emails WHERE mailbox_id=?", m["id"])
		if e != nil {
			return e
		}
		if num(used["size"])+int64(size) > num(m["storage_limit"]) {
			return bad("Mailbox storage full")
		}
		if _, e = tx.Exec("INSERT INTO emails(id,mailbox_id,from_address,to_address,subject,body,content_type,timestamp,sent_formatted,is_read,size_bytes) VALUES(?,?,?,?,?,?,?,?,?,0,?)", token(), m["id"], email["From"], address, email["Subject"], email["Body"], email["ContentType"], email["Timestamp"], email["Sent"], size); e != nil {
			return e
		}
		if _, e = tx.Exec("DELETE FROM emails WHERE mailbox_id=? AND id NOT IN (SELECT id FROM emails WHERE mailbox_id=? ORDER BY timestamp DESC,rowid DESC LIMIT ?)", m["id"], m["id"], s.Config.MaxEmails); e != nil {
			return e
		}
		if _, e = tx.Exec("UPDATE mailboxes SET storage_used=COALESCE((SELECT SUM(size_bytes) FROM emails WHERE mailbox_id=?),0),last_accessed=? WHERE id=?", m["id"], time.Now().Unix(), m["id"]); e != nil {
			return e
		}
	}
	return tx.Commit()
}
