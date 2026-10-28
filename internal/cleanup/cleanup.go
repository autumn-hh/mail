// Package cleanup 定时清理数据库过期数据和 IP 封禁历史。
package cleanup

import (
	"context"
	"log/slog"
	"time"

	"maildrop/internal/config"
	"maildrop/internal/inbox"
)

// ExpiredBlockCleaner 由 *ipguard.Blocker 实现，测试可注入观察器。
type ExpiredBlockCleaner interface{ CleanupExpired() }

// Interval 与 Python 一致：秒模式半周期且至少 5 秒，否则每小时。
func Interval(cfg *config.Config) time.Duration {
	if cfg.EmailRetentionSeconds <= 0 {
		return time.Hour
	}
	seconds := max(5, cfg.EmailRetentionSeconds/2)
	// 防止极端环境整数转纳秒时溢出。
	const maxSeconds = int64(1<<63-1) / int64(time.Second)
	return time.Duration(min(int64(seconds), maxSeconds)) * time.Second
}

// RunOnce 即使数据库清理失败，也清理过期 IP 记录。
func RunOnce(ctx context.Context, inboxSvc *inbox.Service, blocker ExpiredBlockCleaner) (inbox.CleanupResult, error) {
	if blocker != nil {
		defer blocker.CleanupExpired()
	}
	return inboxSvc.CleanExpiredData(ctx)
}

// Run 先清理一次，随后按周期执行；时钟从 inbox.Service 注入。
func Run(ctx context.Context, cfg *config.Config, inboxSvc *inbox.Service, blocker ExpiredBlockCleaner) {
	if ctx.Err() != nil {
		return
	}
	ticker := time.NewTicker(Interval(cfg))
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		result, err := RunOnce(ctx, inboxSvc, blocker)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("定时清理失败", "error", err)
		} else if result.Mailboxes != 0 || result.Emails != 0 {
			slog.Info("定时清理完成", "mailboxes", result.Mailboxes, "emails", result.Emails, "bytes", result.Bytes)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
