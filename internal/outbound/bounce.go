package outbound

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/textproto"
	"strings"

	"github.com/google/uuid"
	"maildrop/internal/mailparse"
	"maildrop/internal/store"
)

func (s *Service) IsBounceRecipient(ctx context.Context, address string) (bool, error) {
	if !strings.HasPrefix(address, "md-bounce-") {
		return false, nil
	}
	var count int
	err := s.db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_messages WHERE bounce_address=?`, address).Scan(&count)
	return count > 0, err
}

// ReceiveBounce 接收随机退信地址上的报告；原邮件归属来自本机数据库，不信任报告里的 Message-ID。
func (s *Service) ReceiveBounce(ctx context.Context, recipients []string, raw []byte) (bool, error) {
	if len(recipients) == 0 {
		return false, nil
	}
	for _, r := range recipients {
		special, err := s.IsBounceRecipient(ctx, r)
		if err != nil {
			return false, err
		}
		if !special {
			return false, nil
		}
	}
	message, err := mailparse.Parse(raw, s.now())
	if err != nil {
		return false, err
	}
	failures := map[string]string{}
	for _, part := range message.Details.Attachments {
		if part.ContentType != "message/delivery-status" {
			continue
		}
		reader := textproto.NewReader(bufio.NewReader(bytes.NewReader(append(append([]byte{}, part.Data...), []byte("\r\n\r\n")...))))
		for {
			h, err := reader.ReadMIMEHeader()
			if err != nil || len(h) == 0 {
				break
			}
			if strings.EqualFold(h.Get("Action"), "failed") {
				_, addr, _ := strings.Cut(h.Get("Final-Recipient"), ";")
				diagnostic := h.Get("Diagnostic-Code")
				if diagnostic == "" {
					diagnostic = h.Get("Status")
				}
				local, domain, ok := strings.Cut(strings.TrimSpace(addr), "@")
				if ok {
					failures[local+"@"+strings.ToLower(domain)] = diagnostic
				}
			}
		}
	}
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		seen := map[string]bool{}
		for _, r := range recipients {
			var id, mailboxID string
			if err := tx.QueryRowContext(ctx, `SELECT id,mailbox_id FROM outbound_messages WHERE bounce_address=?`, r).Scan(&id, &mailboxID); err != nil {
				return err
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			p := Policy{}
			if err := usageTx(ctx, tx, mailboxID, s.now(), &p); err != nil {
				return err
			}
			if p.StorageUsed+int64(len(raw)) > ArchiveLimit {
				return errors.New("outbound report storage full")
			}
			summary := message.Subject
			if len(failures) > 0 {
				summary = "收到失败退信：" + message.Subject
			}
			if len(summary) > 1000 {
				summary = string([]rune(summary)[:min(250, len([]rune(summary)))])
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO outbound_bounces(id,message_id,received_at,raw,summary) VALUES (?,?,?,?,?)`, uuid.NewString(), id, s.now().Unix(), raw, summary); err != nil {
				return err
			}
			for addr, diagnostic := range failures {
				// 只允许更新原信已有的收件人。报告只表示收到的声明，不证明最终投递。
				if _, err := tx.ExecContext(ctx, `UPDATE outbound_recipients SET status='bounced',last_error=? WHERE message_id=? AND address=? AND status IN ('accepted','sending','retry')`, diagnostic, id, addr); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return err == nil, err
}
func (s *Service) ReportRaw(ctx context.Context, mailboxID, id string) ([]byte, error) {
	var raw []byte
	err := s.db.DB().QueryRowContext(ctx, `SELECT b.raw FROM outbound_bounces b JOIN outbound_messages m ON m.id=b.message_id WHERE b.id=? AND m.mailbox_id=?`, id, mailboxID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		err = store.ErrNotFound
	}
	return raw, err
}
