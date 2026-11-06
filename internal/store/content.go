package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"maildrop/internal/mailparse"
)

type Content struct {
	Raw     []byte
	Details mailparse.Details
}

func SaveContent(ctx context.Context, tx *sql.Tx, msg *mailparse.Message) error {
	details, err := json.Marshal(msg.Details)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO mail_content(id,raw,details) VALUES (?,?,?)`, msg.ID, msg.Raw, string(details))
	return err
}

// ContentForEmail 同时验证邮箱 ID，避免附件或原文跨邮箱访问。
func (s *Store) ContentForEmail(ctx context.Context, mailboxID, emailID string) (*Content, error) {
	var raw []byte
	var details string
	err := s.db.QueryRowContext(ctx, `SELECT c.raw,c.details FROM emails e JOIN mail_content c ON c.id=e.content_id WHERE e.id=? AND e.mailbox_id=?`, emailID, mailboxID).Scan(&raw, &details)
	if err != nil {
		return nil, notFound(err)
	}
	c := &Content{Raw: raw}
	err = json.Unmarshal([]byte(details), &c.Details)
	return c, err
}
