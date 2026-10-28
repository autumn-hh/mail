// Package ipguard 提供并发安全的 IP 封禁、白名单和代理地址解析。
package ipguard

import (
	"errors"
	"sync"
	"time"
)

// Config 使用管理 API 的字段名，时间单位为秒。
type Config struct {
	BlockDuration int `json:"block_duration"`
	MaxAttempts   int `json:"max_attempts"`
	AttemptWindow int `json:"attempt_window"`
}

// ConfigUpdate 的 nil 字段保持原值，兼容 JSON 的缺省值和 null。
type ConfigUpdate struct {
	BlockDuration *int `json:"block_duration"`
	MaxAttempts   *int `json:"max_attempts"`
	AttemptWindow *int `json:"attempt_window"`
}

type Blocker struct {
	mu       sync.Mutex
	config   Config
	now      func() time.Time
	blocked  map[string]time.Time
	attempts map[string][]time.Time
}

func NewBlocker(blockDuration, maxAttempts, attemptWindow int) *Blocker {
	return NewBlockerWithClock(blockDuration, maxAttempts, attemptWindow, time.Now)
}

// NewBlockerWithClock 允许初始环境配置使用比管理 API 更宽的范围。
// 非正值回退默认值；调用方提供的时钟须可并发调用。
func NewBlockerWithClock(blockDuration, maxAttempts, attemptWindow int, now func() time.Time) *Blocker {
	if blockDuration <= 0 {
		blockDuration = 300
	}
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	if attemptWindow <= 0 {
		attemptWindow = 60
	}
	if now == nil {
		now = time.Now
	}
	return &Blocker{
		config: Config{blockDuration, maxAttempts, attemptWindow},
		now:    now, blocked: make(map[string]time.Time), attempts: make(map[string][]time.Time),
	}
}

func (b *Blocker) IsBlocked(ip string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.expireBlock(ip, now)
	_, ok := b.blocked[ip]
	return ok
}

// RecordFailedAttempt 在达到阈值时返回 true；重复调用延长封禁，与 Python 一致。
func (b *Blocker) RecordFailedAttempt(ip string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.expireBlock(ip, now)
	attempts := b.pruneAttempts(ip, now)
	attempts = append(attempts, now)
	b.attempts[ip] = attempts
	if len(attempts) >= b.config.MaxAttempts {
		b.blocked[ip] = now.Add(time.Duration(b.config.BlockDuration) * time.Second)
		return true
	}
	return false
}

// GetRemainingBlockTime 的 bool 表示记录存在；过期但尚未清理时返回 (0, true)。
func (b *Blocker) GetRemainingBlockTime(ip string) (int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	until, ok := b.blocked[ip]
	if !ok {
		return 0, false
	}
	return max(0, int(until.Sub(b.now())/time.Second)), true
}

func (b *Blocker) UnblockIP(ip string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.blocked, ip)
	delete(b.attempts, ip)
}

func (b *Blocker) GetBlockedIPs() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	result := make(map[string]int)
	for ip, until := range b.blocked {
		if now.Before(until) {
			result[ip] = int(until.Sub(now) / time.Second)
		}
	}
	return result
}

// CleanupExpired 同时清理未达到封禁阈值的过期尝试，避免长期积累 IP。
func (b *Blocker) CleanupExpired() {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for ip := range b.blocked {
		b.expireBlock(ip, now)
	}
	for ip := range b.attempts {
		b.pruneAttempts(ip, now)
	}
}

func (b *Blocker) GetConfig() Config {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.config
}

// UpdateConfig 先校验全部字段，再一次性更新；现有封禁截止时间不变。
func (b *Blocker) UpdateConfig(update ConfigUpdate) error {
	if v := update.BlockDuration; v != nil && (*v < 60 || *v > 86400) {
		return errors.New("封禁时长必须在60-86400秒之间")
	}
	if v := update.MaxAttempts; v != nil && (*v < 1 || *v > 10) {
		return errors.New("最大失败次数必须在1-10次之间")
	}
	if v := update.AttemptWindow; v != nil && (*v < 30 || *v > 600) {
		return errors.New("尝试窗口时间必须在30-600秒之间")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if update.BlockDuration != nil {
		b.config.BlockDuration = *update.BlockDuration
	}
	if update.MaxAttempts != nil {
		b.config.MaxAttempts = *update.MaxAttempts
	}
	if update.AttemptWindow != nil {
		b.config.AttemptWindow = *update.AttemptWindow
	}
	return nil
}

// 以下辅助方法仅在持锁时调用。
func (b *Blocker) expireBlock(ip string, now time.Time) {
	if until, ok := b.blocked[ip]; ok && !now.Before(until) {
		delete(b.blocked, ip)
		delete(b.attempts, ip)
	}
}

func (b *Blocker) pruneAttempts(ip string, now time.Time) []time.Time {
	attempts := b.attempts[ip]
	kept := attempts[:0]
	cutoff := now.Add(-time.Duration(b.config.AttemptWindow) * time.Second)
	for _, timestamp := range attempts {
		if timestamp.After(cutoff) {
			kept = append(kept, timestamp)
		}
	}
	if len(kept) == 0 {
		delete(b.attempts, ip)
		return nil
	}
	b.attempts[ip] = kept
	return kept
}
