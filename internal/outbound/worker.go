package outbound

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"
)

type job struct {
	ID, MailboxID, Address, From string
	Raw                          []byte
	Attempts                     int
	CreatedAt                    int64
}

func (s *Service) claim(ctx context.Context) (*job, error) {
	var selected *job
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		now := s.now().Unix()
		if _, err := tx.ExecContext(ctx, `UPDATE outbound_recipients SET status='retry',uncertain=1,last_error='上次连接未完成确认，将自动重试' WHERE status='sending' AND lease_until<=?`, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE outbound_recipients SET status='failed',last_error='邮箱已停用、过期或发件权限已撤销'
    WHERE status IN ('queued','retry') AND (cancelled=1 OR message_id IN (
      SELECT o.id FROM outbound_messages o JOIN mailboxes m ON m.id=o.mailbox_id LEFT JOIN sending_policies p ON p.mailbox_id=m.id
      WHERE m.is_active=0 OR m.expires_at<? OR COALESCE(p.enabled,0)=0))`, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE outbound_recipients SET status='failed',last_error='24小时内未能完成投递'
    WHERE status IN ('queued','retry') AND message_id IN (SELECT id FROM outbound_messages WHERE created_at<=?)`, now-86400); err != nil {
			return err
		}
		j := &job{}
		err := tx.QueryRowContext(ctx, `SELECT r.message_id,o.mailbox_id,r.address,o.bounce_address,o.raw,r.attempts,o.created_at
    FROM outbound_recipients r JOIN outbound_messages o ON o.id=r.message_id
    WHERE r.status IN ('queued','retry') AND r.next_attempt<=? ORDER BY r.next_attempt,o.created_at LIMIT 1`, now).Scan(&j.ID, &j.MailboxID, &j.Address, &j.From, &j.Raw, &j.Attempts, &j.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE outbound_recipients SET status='sending',attempts=attempts+1,lease_until=? WHERE message_id=? AND address=?`, now+1800, j.ID, j.Address)
		selected = j
		return err
	})
	return selected, err
}
func (s *Service) process(ctx context.Context) (bool, error) {
	j, err := s.claim(ctx)
	if err != nil || j == nil {
		return false, err
	}
	// claim 是开始投递的持久边界；网络建立前再次读取撤销标记。
	var cancelled bool
	if err = s.db.DB().QueryRowContext(ctx, `SELECT cancelled FROM outbound_recipients WHERE message_id=? AND address=?`, j.ID, j.Address).Scan(&cancelled); err != nil {
		return true, err
	}
	if cancelled {
		err = &DeliveryError{Permanent: true, Reason: "发件权限已撤销"}
	} else {
		deliveryCtx, cancel := context.WithTimeout(ctx, min(20*time.Minute, time.Duration(j.CreatedAt+86400-s.now().Unix())*time.Second))
		err = s.transport.Deliver(deliveryCtx, j.From, j.Address, j.Raw)
		cancel()
	}
	now := s.now().Unix()
	status, reason, uncertain := "accepted", "", false
	next := now
	if err != nil {
		status, reason = "retry", err.Error()
		var delivery *DeliveryError
		if errors.As(err, &delivery) {
			uncertain = delivery.Uncertain
			if delivery.Permanent {
				status = "failed"
			}
		}
		if now >= j.CreatedAt+86400 {
			status = "failed"
		}
		delays := []int64{60, 300, 900, 1800, 3600}
		next = min(now+delays[min(j.Attempts, len(delays)-1)], j.CreatedAt+86400)
	}
	// 已取得投递结果后，仅重试落库，避免重复发送或将明确永久失败恢复为重试。
	// 若此时进程崩溃，重启只能看到未完成租约，按不确定结果恢复并明确标记重复风险。
	for {
		saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, saveErr := s.db.DB().ExecContext(saveCtx, `UPDATE outbound_recipients SET status=CASE WHEN cancelled=1 AND ?='retry' THEN 'failed' ELSE ? END,last_error=?,next_attempt=?,lease_until=0,uncertain=MAX(uncertain,?) WHERE message_id=? AND address=? AND status='sending'`, status, status, reason, next, uncertain, j.ID, j.Address)
		cancel()
		if saveErr == nil {
			return true, nil
		}
		if ctx.Err() != nil {
			return true, saveErr
		}
		slog.Error("等待保存邮件投递结果", "message_id", j.ID, "status", status, "error", saveErr)
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	cleanup := time.NewTicker(time.Hour)
	defer cleanup.Stop()
	if err := s.Clean(ctx); err != nil {
		slog.Error("清理发件存档失败", "error", err)
	}
	// 单进程启动时恢复未完成的租约；已接受和永久失败记录不进入恢复范围。
	if _, err := s.db.DB().ExecContext(ctx, `UPDATE outbound_recipients SET lease_until=0 WHERE status='sending'`); err != nil {
		slog.Error("恢复发件队列失败", "error", err)
		return
	}
	lastCleanup := s.now()
	for ctx.Err() == nil {
		if s.now().Sub(lastCleanup) >= time.Hour {
			if err := s.Clean(ctx); err != nil {
				slog.Error("清理发件存档失败", "error", err)
			}
			lastCleanup = s.now()
		}
		processed := false
		var err error
		if s.cfg.OutboundEnabled {
			processed, err = s.process(ctx)
		}
		if err != nil && ctx.Err() == nil {
			slog.Error("后台发件失败", "error", err)
		}
		if processed && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-ticker.C:
		case <-cleanup.C:
			if err := s.Clean(ctx); err != nil {
				slog.Error("清理发件存档失败", "error", err)
			}
		}
	}
}
