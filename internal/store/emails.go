package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

// CalculateEmailSize 与 Python 一致，仅统计这五个字段的 UTF-8 字节数。
func CalculateEmailSize(e Email) int64 {
	return int64(len(e.From)) + int64(len(e.To)) + int64(len(e.Subject)) + int64(len(e.Body)) + int64(len(e.Sent))
}

func (s *Store) AddEmail(mailboxID string, e Email) (string, error) {
	if e.ID == "" {
		id, err := uuid.NewRandom()
		if err != nil {
			return "", err
		}
		e.ID = id.String()
	}
	if e.ContentType == "" {
		e.ContentType = "Text"
	}
	size := CalculateEmailSize(e)
	if size > s.cfg.MaxEmailSizeBytes {
		return "", fmt.Errorf("邮件大小 (%.2fMB) 超过限制 (%dMB)", float64(size)/1048576, s.cfg.MaxEmailSizeMB)
	}
	err := s.WithTx(context.Background(), func(tx *sql.Tx) error {
		var used, limit int64
		err := tx.QueryRow(`SELECT COALESCE(storage_used, 0), COALESCE(NULLIF(storage_limit, 0), ?) FROM mailboxes WHERE id = ?`, s.cfg.MaxMailboxSizeBytes, mailboxID).Scan(ScanInt64(&used), ScanInt64(&limit))
		if err != nil {
			return notFound(err)
		}
		if size > limit-used {
			return fmt.Errorf("邮箱容量不足，当前已用 %.2fMB，限制 %.2fMB", float64(used)/1048576, float64(limit)/1048576)
		}
		if _, err := tx.Exec(`INSERT INTO emails (id, mailbox_id, from_address, to_address, subject, body, content_type, timestamp, sent_formatted, size_bytes)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, e.ID, mailboxID, e.From, e.To, e.Subject, e.Body, e.ContentType, e.Timestamp, e.Sent, size); err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE mailboxes SET storage_used = COALESCE(storage_used, 0) + ? WHERE id = ?", size, mailboxID)
		return err
	})
	if err != nil {
		return "", err
	}
	return e.ID, nil
}

const emailSelect = `SELECT id, mailbox_id, from_address, to_address, subject, body, content_type, timestamp, sent_formatted, is_read, COALESCE(size_bytes, 0) FROM emails`

func scanEmail(row scanner) (*Email, error) {
	var e Email
	var subject, body, kind, sent sql.NullString
	var read LegacyInt64
	if err := row.Scan(&e.ID, &e.MailboxID, &e.From, &e.To, &subject, &body, &kind, ScanInt64(&e.Timestamp), &sent, &read, ScanInt64(&e.SizeBytes)); err != nil {
		return nil, notFound(err)
	}
	e.Subject, e.Body, e.ContentType, e.Sent, e.IsRead = subject.String, body.String, kind.String, sent.String, read.Int64 != 0
	for _, field := range []struct {
		name  string
		value sql.NullString
	}{{"Subject", subject}, {"Body", body}, {"ContentType", kind}, {"Sent", sent}} {
		if !field.value.Valid {
			e.nullFields = append(e.nullFields, field.name)
		}
	}
	return &e, nil
}

// GetEmailsByMailbox limit 为 0/未传时不限量，负数保持 SQLite LIMIT 的原行为。
func (s *Store) GetEmailsByMailbox(mailboxID string, limit ...int) ([]Email, error) {
	query := emailSelect + " WHERE mailbox_id = ? ORDER BY timestamp DESC"
	args := []any{mailboxID}
	if len(limit) > 0 && limit[0] != 0 {
		query += " LIMIT ?"
		args = append(args, limit[0])
	}
	rows, err := s.db.Query(query, args...)
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
		e.MailboxID = "" // Python 列表不返回 mailbox_id。
		emails = append(emails, *e)
	}
	return emails, rows.Err()
}
func (s *Store) GetEmailByID(id string) (*Email, error) {
	return scanEmail(s.db.QueryRow(emailSelect+" WHERE id = ?", id))
}
func (s *Store) MarkEmailAsRead(id string) error {
	_, err := s.db.Exec("UPDATE emails SET is_read = 1 WHERE id = ?", id)
	return err
}
func (s *Store) MarkEmailAsUnread(id string) error {
	_, err := s.db.Exec("UPDATE emails SET is_read = 0 WHERE id = ?", id)
	return err
}
func (s *Store) MarkAllEmailsRead(mailboxID string) (int64, error) {
	return affected(s.db.Exec("UPDATE emails SET is_read = 1 WHERE mailbox_id = ? AND is_read = 0", mailboxID))
}
func (s *Store) DeleteEmail(id string) (int64, error) {
	var count int64
	err := s.WithTx(context.Background(), func(tx *sql.Tx) error {
		var mailboxID string
		var size int64
		err := tx.QueryRow("SELECT mailbox_id, COALESCE(size_bytes, 0) FROM emails WHERE id = ?", id).Scan(&mailboxID, ScanInt64(&size))
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		count, err = affected(tx.Exec("DELETE FROM emails WHERE id = ?", id))
		if err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE mailboxes SET storage_used = MAX(0, COALESCE(storage_used, 0) - ?) WHERE id = ?", size, mailboxID)
		return err
	})
	return count, err
}
func (s *Store) GetMailboxStats(mailboxID string) (*MailboxStats, error) {
	stats := &MailboxStats{StorageLimit: s.cfg.MaxMailboxSizeBytes}
	var lastEmail LegacyInt64
	// 单条查询提供一致的邮件/容量快照，空邮箱的 MAX 返回 JSON null。
	err := s.db.QueryRow(`SELECT COUNT(*), COUNT(CASE WHEN is_read = 0 THEN 1 END), MAX(timestamp),
 COALESCE((SELECT storage_used FROM mailboxes WHERE id = ?), 0),
 COALESCE((SELECT storage_limit FROM mailboxes WHERE id = ?), ?)
 FROM emails WHERE mailbox_id = ?`, mailboxID, mailboxID, s.cfg.MaxMailboxSizeBytes, mailboxID).
		Scan(&stats.TotalEmails, &stats.UnreadEmails, &lastEmail, ScanInt64(&stats.StorageUsed), ScanInt64(&stats.StorageLimit))
	if err != nil {
		return nil, err
	}
	stats.LastEmailTime = lastEmail.Pointer()
	stats.StorageUsedMB, stats.StorageLimitMB = float64(stats.StorageUsed)/1048576, float64(stats.StorageLimit)/1048576
	if stats.StorageLimit > 0 {
		stats.StoragePercent = float64(stats.StorageUsed) / float64(stats.StorageLimit) * 100
	}
	return stats, nil
}
func (s *Store) CleanExpiredMailboxes() (int64, error) {
	var count int64
	err := s.WithTx(context.Background(), func(tx *sql.Tx) error {
		now := s.now().Unix()
		if _, err := tx.Exec("DELETE FROM emails WHERE mailbox_id IN (SELECT id FROM mailboxes WHERE expires_at < ?)", now); err != nil {
			return err
		}
		var err error
		count, err = affected(tx.Exec("DELETE FROM mailboxes WHERE expires_at < ?", now))
		return err
	})
	return count, err
}

// CleanOldEmails 保持 Python 的按天参数；秒级清理由后续 cleanup 切片决定。
func (s *Store) CleanOldEmails(retentionDays ...int) (int64, error) {
	days := s.cfg.EmailRetentionDays
	if len(retentionDays) > 0 {
		days = retentionDays[0]
	}
	cutoff := s.now().Unix() - int64(days)*86400
	var count int64
	err := s.WithTx(context.Background(), func(tx *sql.Tx) error {
		// 只扣本次删除的字节，不能重算并覆盖旧文件的历史计数。
		_, err := tx.Exec(`UPDATE mailboxes SET storage_used = MAX(0, COALESCE(storage_used, 0) -
 (SELECT COALESCE(SUM(size_bytes), 0) FROM emails WHERE mailbox_id = mailboxes.id AND timestamp < ?))
 WHERE id IN (SELECT mailbox_id FROM emails WHERE timestamp < ?)`, cutoff, cutoff)
		if err != nil {
			return err
		}
		count, err = affected(tx.Exec("DELETE FROM emails WHERE timestamp < ?", cutoff))
		return err
	})
	return count, err
}
