package admin

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

type Stats struct {
	TotalMailboxes    int64 `json:"total_mailboxes"`
	ActiveMailboxes   int64 `json:"active_mailboxes"`
	ExpiredMailboxes  int64 `json:"expired_mailboxes"`
	DisabledMailboxes int64 `json:"disabled_mailboxes"`
	TotalEmails       int64 `json:"total_emails"`
	UnreadEmails      int64 `json:"unread_emails"`
}

func (s *Service) Stats(ctx context.Context) (*Stats, error) {
	var v Stats
	err := s.store.DB().QueryRowContext(ctx, `SELECT COUNT(*),
 COUNT(CASE WHEN is_active=1 AND expires_at>? THEN 1 END),
 COUNT(CASE WHEN expires_at<=? THEN 1 END),COUNT(CASE WHEN is_active=0 THEN 1 END),
 (SELECT COUNT(*) FROM emails),(SELECT COUNT(*) FROM emails WHERE is_read=0) FROM mailboxes`, s.now().Unix(), s.now().Unix()).Scan(
		&v.TotalMailboxes, &v.ActiveMailboxes, &v.ExpiredMailboxes, &v.DisabledMailboxes, &v.TotalEmails, &v.UnreadEmails)
	return &v, err
}

func (s *Service) SourceStats(ctx context.Context) (map[string]int64, error) {
	rows, err := s.store.DB().QueryContext(ctx, "SELECT created_source,COUNT(*) FROM mailboxes GROUP BY created_source")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]int64{}
	for rows.Next() {
		var source sql.NullString
		var count int64
		if err := rows.Scan(&source, &count); err != nil {
			return nil, err
		}
		key := source.String
		if key == "" {
			key = "unknown"
		}
		result[key] = count
	}
	return result, rows.Err()
}

func existingAuditID(ctx context.Context, tx *sql.Tx, id string, changes map[string]any) (*string, error) {
	var found string
	err := tx.QueryRowContext(ctx, "SELECT id FROM mailboxes WHERE id=?", id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		changes["mailbox_id"] = id
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &found, nil
}

func (s *Service) ResetToken(ctx context.Context, id string, actor Actor) (string, error) {
	token, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		changes := map[string]any{"new_token": token.String()}
		auditID, err := existingAuditID(ctx, tx, id, changes)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE mailboxes SET access_token=? WHERE id=?", token.String(), id); err != nil {
			return err
		}
		return s.logAuditTx(ctx, tx, "RESET_TOKEN", auditID, changes, actor)
	})
	return token.String(), err
}

func (s *Service) EnableMailbox(ctx context.Context, id string, actor Actor) error {
	return s.store.WithTx(ctx, func(tx *sql.Tx) error {
		changes := map[string]any{"is_active": true, "admin_disabled": false}
		auditID, err := existingAuditID(ctx, tx, id, changes)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE mailboxes SET is_active=1,admin_disabled=0 WHERE id=?", id); err != nil {
			return err
		}
		return s.logAuditTx(ctx, tx, "RESTORE", auditID, changes, actor)
	})
}
