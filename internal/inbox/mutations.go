package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"maildrop/internal/store"
)

// ErrAdminDisabled 表示持有者不能恢复管理员禁用的邮箱。
var ErrAdminDisabled = errors.New("mailbox disabled by administrator")

func (s *Service) exec(ctx context.Context, query string, args ...any) (int64, error) {
	var count int64
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		count, err = affected(tx.ExecContext(ctx, query, args...))
		return err
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// MarkEmailAsRead 与 Python 一致，未找到邮件仍返回 true。
func (s *Service) MarkEmailAsRead(ctx context.Context, emailID string) (bool, error) {
	_, err := s.exec(ctx, "UPDATE emails SET is_read = 1 WHERE id = ?", emailID)
	return err == nil, err
}

func (s *Service) MarkEmailAsUnread(ctx context.Context, emailID string) (bool, error) {
	_, err := s.exec(ctx, "UPDATE emails SET is_read = 0 WHERE id = ?", emailID)
	return err == nil, err
}

func (s *Service) MarkAllEmailsRead(ctx context.Context, mailboxID string) (int64, error) {
	return s.exec(ctx, "UPDATE emails SET is_read = 1 WHERE mailbox_id = ? AND is_read = 0", mailboxID)
}

func (s *Service) DeleteEmail(ctx context.Context, emailID string) (int64, error) {
	var count int64
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var mailboxID string
		var size int64
		err := tx.QueryRowContext(ctx, "SELECT mailbox_id, COALESCE(size_bytes, 0) FROM emails WHERE id = ?", emailID).Scan(&mailboxID, store.ScanInt64(&size))
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		count, err = affected(tx.ExecContext(ctx, "DELETE FROM emails WHERE id = ?", emailID))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE mailboxes SET storage_used = MAX(0, COALESCE(storage_used, 0) - ?) WHERE id = ?", size, mailboxID)
		return err
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Service) changeWhitelist(ctx context.Context, mailboxID, sender string, add bool) (bool, error) {
	found := false
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		m, err := s.mailboxByID(ctx, tx, mailboxID)
		if err != nil || m == nil {
			return err
		}
		list := m.SenderWhitelist
		index := -1
		for i, value := range list {
			if value == sender {
				index = i
				break
			}
		}
		if add && index < 0 {
			list = append(list, sender)
		}
		if !add && index >= 0 {
			list = append(list[:index], list[index+1:]...)
		}
		data, err := json.Marshal(list)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE mailboxes SET sender_whitelist = ? WHERE id = ?", string(data), m.ID)
		found = err == nil
		return err
	})
	return found && err == nil, err
}

func (s *Service) AddSenderToWhitelist(ctx context.Context, mailboxID, sender string) (bool, error) {
	return s.changeWhitelist(ctx, mailboxID, sender, true)
}

func (s *Service) RemoveSenderFromWhitelist(ctx context.Context, mailboxID, sender string) (bool, error) {
	return s.changeWhitelist(ctx, mailboxID, sender, false)
}

// UpdateMailboxRetention 从当前时间重新计算有效期，不按创建时间计算。
func (s *Service) UpdateMailboxRetention(ctx context.Context, mailboxID string, days int) (bool, error) {
	count, err := s.exec(ctx, "UPDATE mailboxes SET retention_days = ?, expires_at = ? WHERE id = ?", days, s.now().Unix()+int64(days)*86400, mailboxID)
	return count > 0, err
}

// RegenerateMailboxKey 空字符串表示邮箱不存在或当前密钥不匹配。
func (s *Service) RegenerateMailboxKey(ctx context.Context, mailboxID, currentKey string) (string, error) {
	key, err := store.GenerateMailboxKey()
	if err != nil {
		return "", err
	}
	count, err := s.exec(ctx, "UPDATE mailboxes SET mailbox_key = ? WHERE id = ? AND mailbox_key = ?", key, mailboxID, currentKey)
	if err != nil || count == 0 {
		return "", err
	}
	return key, nil
}

// UpdateMailboxStatus 在写入事务内检查管理员禁用标记，暂停不受此限制。
func (s *Service) UpdateMailboxStatus(ctx context.Context, mailboxID string, active bool) (bool, error) {
	var count int64
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		count, err = affected(tx.ExecContext(ctx, "UPDATE mailboxes SET is_active = ? WHERE id = ? AND (? = 0 OR admin_disabled = 0)", active, mailboxID, active))
		if err != nil || count > 0 {
			return err
		}
		var disabled store.LegacyInt64
		err = tx.QueryRowContext(ctx, "SELECT admin_disabled FROM mailboxes WHERE id = ?", mailboxID).Scan(&disabled)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if active && disabled.Int64 != 0 {
			return ErrAdminDisabled
		}
		return nil
	})
	return count > 0 && err == nil, err
}

func (s *Service) UpdateWhitelistStatus(ctx context.Context, mailboxID string, enabled bool) (bool, error) {
	count, err := s.exec(ctx, "UPDATE mailboxes SET whitelist_enabled = ? WHERE id = ?", enabled, mailboxID)
	return count > 0, err
}

func (s *Service) UpdateSenderWhitelist(ctx context.Context, mailboxID string, whitelist []string) (bool, error) {
	data, err := json.Marshal(append([]string{}, whitelist...))
	if err != nil {
		return false, err
	}
	count, err := s.exec(ctx, "UPDATE mailboxes SET sender_whitelist = ? WHERE id = ?", string(data), mailboxID)
	return count > 0, err
}

// DeleteMailbox 依靠已启用的外键级联删除邮件和绑定，审计记录保留。
func (s *Service) DeleteMailbox(ctx context.Context, mailboxID string) (bool, error) {
	count, err := s.exec(ctx, "DELETE FROM mailboxes WHERE id = ?", mailboxID)
	return count > 0, err
}

func (s *Service) LimitEmailsPerMailbox(ctx context.Context, mailboxID string) (int64, error) {
	var count int64
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		count, err = prune(ctx, tx, mailboxID, s.cfg.MaxEmailsPerAddress)
		return err
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// CleanExpiredData 使用配置中已计算好的秒级有效保留时间，收信不调用此方法。
func (s *Service) CleanExpiredData(ctx context.Context) (CleanupResult, error) {
	var result CleanupResult
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		now := s.now().Unix()
		cutoff := now - s.cfg.EmailRetentionTime
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM emails
 WHERE timestamp < ? OR mailbox_id IN (SELECT id FROM mailboxes WHERE expires_at < ?)`, cutoff, now).
			Scan(&result.Emails, store.ScanInt64(&result.Bytes)); err != nil {
			return err
		}
		var err error
		result.Mailboxes, err = affected(tx.ExecContext(ctx, "DELETE FROM mailboxes WHERE expires_at < ?", now))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE mailboxes SET storage_used = MAX(0, COALESCE(storage_used, 0) -
 (SELECT COALESCE(SUM(size_bytes), 0) FROM emails WHERE mailbox_id = mailboxes.id AND timestamp < ?))
 WHERE id IN (SELECT mailbox_id FROM emails WHERE timestamp < ?)`, cutoff, cutoff)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM emails WHERE timestamp < ?", cutoff)
		return err
	})
	if err != nil {
		return CleanupResult{}, err
	}
	return result, nil
}
