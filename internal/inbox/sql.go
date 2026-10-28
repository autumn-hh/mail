package inbox

import (
	"context"
	"database/sql"
	"strings"

	"maildrop/internal/store"
)

type scanner interface{ Scan(...any) error }
type emailQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

const mailboxSelect = `SELECT id, address, created_at, expires_at, retention_days,
 sender_whitelist, whitelist_enabled, access_token, mailbox_key, is_active,
 created_by_ip, last_accessed, COALESCE(storage_used, 0),
 COALESCE(NULLIF(storage_limit, 0), ?), created_source, allowed_domains, admin_disabled FROM mailboxes`

func scanMailbox(row scanner) (*Mailbox, error) {
	var m Mailbox
	var whitelist, domains, token, key, source sql.NullString
	var retention, enabled, active, accessed, adminDisabled store.LegacyInt64
	err := row.Scan(&m.ID, &m.Address, store.ScanInt64(&m.CreatedAt), store.ScanInt64(&m.ExpiresAt), &retention, &whitelist,
		&enabled, &token, &key, &active, &m.CreatedByIP, &accessed,
		store.ScanInt64(&m.StorageUsed), store.ScanInt64(&m.StorageLimit), &source, &domains, &adminDisabled)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.RetentionDays, m.WhitelistEnabled, m.IsActive = int(retention.Int64), enabled.Int64 != 0, active.Int64 != 0
	m.AdminDisabled = adminDisabled.Int64 != 0
	m.AccessToken, m.MailboxKey, m.CreatedSource = token.String, key.String, source.String
	m.nullToken, m.nullRetention = !token.Valid, !retention.Valid
	m.LastAccessed = accessed.Pointer()
	m.SenderWhitelist = store.DecodeMailboxList(whitelist, m.ID, "sender_whitelist")
	m.AllowedDomains = store.DecodeMailboxList(domains, m.ID, "allowed_domains")
	return &m, nil
}

func (s *Service) mailbox(ctx context.Context, tx *sql.Tx, address string) (*Mailbox, error) {
	return scanMailbox(tx.QueryRowContext(ctx, mailboxSelect+" WHERE address = ?", s.cfg.MaxMailboxSizeBytes, address))
}

func (s *Service) mailboxByID(ctx context.Context, tx *sql.Tx, mailboxID string) (*Mailbox, error) {
	return scanMailbox(tx.QueryRowContext(ctx, mailboxSelect+" WHERE id = ?", s.cfg.MaxMailboxSizeBytes, mailboxID))
}

// deliveryMailbox 仅投递按域名忽略大小写，本地部分始终精确匹配。
func (s *Service) deliveryMailbox(ctx context.Context, tx *sql.Tx, address string) (*Mailbox, error) {
	m, err := s.mailbox(ctx, tx, address)
	if err != nil || m != nil {
		return m, err
	}
	at := strings.LastIndexByte(address, '@')
	if at < 0 {
		return nil, nil
	}
	local, domain := address[:at+1], strings.ToLower(address[at+1:])
	if normalized := local + domain; normalized != address {
		m, err = s.mailbox(ctx, tx, normalized)
		if err != nil || m != nil {
			return m, err
		}
	}
	return scanMailbox(tx.QueryRowContext(ctx, mailboxSelect+` WHERE substr(address, 1, instr(address, '@')) = ?
 AND lower(substr(address, instr(address, '@') + 1)) = ? ORDER BY rowid ASC LIMIT 1`,
		s.cfg.MaxMailboxSizeBytes, local, domain))
}

const emailSelect = `SELECT id, mailbox_id, from_address, to_address, subject, body,
 content_type, timestamp, sent_formatted, is_read FROM emails`

func scanEmail(row scanner) (*Email, error) {
	var e Email
	var subject, body, kind, sent sql.NullString
	var read store.LegacyInt64
	if err := row.Scan(&e.ID, &e.MailboxID, &e.From, &e.To, &subject, &body, &kind,
		store.ScanInt64(&e.Timestamp), &sent, &read); err != nil {
		return nil, err
	}
	e.Subject, e.Body, e.ContentType, e.Sent, e.IsRead = subject.String, body.String, kind.String, sent.String, read.Int64 != 0
	for _, field := range []struct {
		key   string
		value sql.NullString
	}{{"Subject", subject}, {"Body", body}, {"ContentType", kind}, {"Sent", sent}} {
		if !field.value.Valid {
			e.nullFields = append(e.nullFields, field.key)
		}
	}
	return &e, nil
}

func queryEmails(ctx context.Context, q emailQuerier, mailboxID string, limit int) ([]Email, error) {
	query := emailSelect + " WHERE mailbox_id = ? ORDER BY timestamp DESC, rowid DESC"
	args := []any{mailboxID}
	if limit != 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	emails := []Email{}
	for rows.Next() {
		e, err := scanEmail(rows)
		if err != nil {
			return nil, err
		}
		e.MailboxID = ""
		emails = append(emails, *e)
	}
	return emails, rows.Err()
}

// prune 在当前事务内扣减实际删除的大小，同秒邮件按插入顺序保留最新。
func prune(ctx context.Context, tx *sql.Tx, mailboxID string, limit int) (int64, error) {
	const stale = `SELECT id FROM emails WHERE mailbox_id = ? ORDER BY timestamp DESC, rowid DESC LIMIT -1 OFFSET ?`
	var size, count int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(size_bytes), 0), COUNT(*) FROM emails WHERE id IN ("+stale+")", mailboxID, limit).
		Scan(store.ScanInt64(&size), &count); err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM emails WHERE id IN ("+stale+")", mailboxID, limit); err != nil {
		return 0, err
	}
	_, err := tx.ExecContext(ctx, "UPDATE mailboxes SET storage_used = MAX(0, COALESCE(storage_used, 0) - ?) WHERE id = ?", size, mailboxID)
	return count, err
}

func affected(result sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
