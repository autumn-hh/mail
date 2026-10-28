package inbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"maildrop/internal/config"
	"maildrop/internal/mailparse"
	"maildrop/internal/store"

	"github.com/google/uuid"
)

type Service struct {
	cfg   config.Config
	store *store.Store
	now   func() time.Time
}

type CreateMailboxOptions = store.CreateMailboxOptions

// New 的配置为快照；注入时钟须支持并发调用。
func New(cfg *config.Config, db *store.Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{cfg: *cfg, store: db, now: now}
}

// CreateOrGetMailbox 原子替换过期邮箱及其关联记录，避免地址唯一键冲突。
func (s *Service) CreateOrGetMailbox(ctx context.Context, address string, options CreateMailboxOptions) (*Mailbox, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	key, err := store.GenerateMailboxKey()
	if err != nil {
		return nil, err
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	days := s.cfg.MailboxRetentionDays
	if options.RetentionDays != nil {
		days = *options.RetentionDays
	}
	whitelist := append([]string{}, options.SenderWhitelist...)
	encoded, err := json.Marshal(whitelist)
	if err != nil {
		return nil, err
	}
	source := options.CreatedSource
	if source == "" {
		source = "unknown"
	}
	var result *Mailbox
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		now := s.now().Unix()
		m, err := s.mailbox(ctx, tx, address)
		if err != nil {
			return err
		}
		if m != nil && now <= m.ExpiresAt {
			_, err = tx.ExecContext(ctx, "UPDATE mailboxes SET last_accessed = ? WHERE id = ?", now, m.ID)
			result = m // Python 返回更新访问时间前的快照。
			return err
		}
		if m != nil {
			if _, err := tx.ExecContext(ctx, "DELETE FROM mailboxes WHERE id = ?", m.ID); err != nil {
				return err
			}
		}
		result = &Mailbox{ID: id.String(), Address: address, CreatedAt: now,
			ExpiresAt: now + int64(days)*86400, RetentionDays: days, SenderWhitelist: whitelist,
			WhitelistEnabled: len(whitelist) > 0, AccessToken: base64.RawURLEncoding.EncodeToString(token),
			MailboxKey: key, IsActive: true, CreatedSource: source, CreatedByIP: options.CreatedByIP,
			LastAccessed: &now, StorageLimit: 3145728, created: true}
		_, err = tx.ExecContext(ctx, `INSERT INTO mailboxes (id, address, created_at, expires_at,
 retention_days, sender_whitelist, whitelist_enabled, created_by_ip, access_token, mailbox_key,
 last_accessed, created_source) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			result.ID, address, now, result.ExpiresAt, days, string(encoded), result.WhitelistEnabled,
			options.CreatedByIP, result.AccessToken, key, now, source)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// senderAllowed 按头部 From 匹配白名单；裸域名与 @域名等价，忽略大小写。
func senderAllowed(m *Mailbox, sender string) bool {
	if !m.WhitelistEnabled || len(m.SenderWhitelist) == 0 {
		return true
	}
	sender = strings.ToLower(sender)
	for _, allowed := range m.SenderWhitelist {
		allowed = strings.ToLower(allowed)
		if !strings.Contains(allowed, "@") {
			allowed = "@" + allowed
		}
		if strings.HasPrefix(allowed, "@") {
			if strings.HasSuffix(sender, allowed) {
				return true
			}
		} else if sender == allowed {
			return true
		}
	}
	return false
}

// senderDomainAllowed 按头部 From 的域名或子域匹配，独立于白名单开关。
func senderDomainAllowed(m *Mailbox, sender string) bool {
	if len(m.AllowedDomains) == 0 {
		return true
	}
	at := strings.LastIndexByte(sender, '@')
	if at < 0 {
		return false
	}
	domain := strings.TrimSuffix(strings.ToLower(sender[at+1:]), ".")
	if domain == "" {
		return false
	}
	for _, allowed := range m.AllowedDomains {
		allowed = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(allowed)), ".")
		if allowed != "" && (domain == allowed || strings.HasSuffix(domain, "."+allowed)) {
			return true
		}
	}
	return false
}

// Deliver 只按信封收件人投递；发件人白名单与 Python 版一致只校验头部 From。
// 信封发件人常是 SES、邮件列表等第三方退信地址，叠加校验会误拒已加白的正常邮件，
// 而两者在没有 SPF/DKIM 时同样可伪造，并不能带来实际安全收益。
// 预期拒收不返回 error；内部错误回滚整批，结果不报告任何已提交投递。
func (s *Service) Deliver(ctx context.Context, envelopeFrom string, recipients []string, msg *mailparse.Message) (DeliveryResult, error) {
	result := DeliveryResult{Recipients: []RecipientOutcome{}}
	if msg == nil {
		return result, errors.New("nil message")
	}
	email := store.Email{From: msg.From, To: msg.To, Subject: msg.Subject, Body: msg.Body,
		ContentType: msg.ContentType, Timestamp: msg.Timestamp, Sent: msg.Sent}
	if email.ContentType == "" {
		email.ContentType = "Text"
	}
	size := store.CalculateEmailSize(email)
	ids := make([]string, len(recipients))
	for i := range ids {
		if i == 0 && msg.ID != "" {
			ids[i] = msg.ID
			continue
		}
		id, err := uuid.NewRandom()
		if err != nil {
			return result, err
		}
		ids[i] = id.String()
	}
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		now := s.now().Unix()
		seen := make(map[string]RecipientOutcome)
		for i, recipient := range recipients {
			outcome, duplicate := seen[recipient]
			if !duplicate {
				outcome = RecipientOutcome{Recipient: recipient}
				m, err := s.deliveryMailbox(ctx, tx, recipient)
				if err != nil {
					return err
				}
				switch {
				case recipient == "":
					outcome.Reason = "No recipient specified"
				case m == nil:
					outcome.Reason = fmt.Sprintf("Mailbox %s does not exist", recipient)
				case now > m.ExpiresAt:
					outcome.Reason = fmt.Sprintf("Mailbox %s has expired", recipient)
				case !m.IsActive:
					outcome.Reason = fmt.Sprintf("Mailbox %s is disabled", recipient)
				case !senderAllowed(m, msg.From) || !senderDomainAllowed(m, msg.From):
					outcome.Reason = fmt.Sprintf("Sender %s not allowed for mailbox %s", msg.From, recipient)
				case size > s.cfg.MaxEmailSizeBytes:
					outcome.Reason = fmt.Sprintf("Failed to save email: 邮件大小 (%.2fMB) 超过限制 (%dMB)", float64(size)/1048576, s.cfg.MaxEmailSizeMB)
				case size > m.StorageLimit-m.StorageUsed:
					outcome.Reason = fmt.Sprintf("Failed to save email: 邮箱容量不足，当前已用 %.2fMB，限制 %.2fMB", float64(m.StorageUsed)/1048576, float64(m.StorageLimit)/1048576)
				default:
					_, err := tx.ExecContext(ctx, `INSERT INTO emails (id, mailbox_id, from_address,
 to_address, subject, body, content_type, timestamp, sent_formatted, size_bytes)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, ids[i], m.ID, email.From, email.To, email.Subject,
						email.Body, email.ContentType, email.Timestamp, email.Sent, size)
					if err != nil {
						return err
					}
					if _, err = tx.ExecContext(ctx, `UPDATE mailboxes SET storage_used = COALESCE(storage_used, 0) + ?, last_accessed = ? WHERE id = ?`, size, now, m.ID); err != nil {
						return err
					}
					if _, err = prune(ctx, tx, m.ID, s.cfg.MaxEmailsPerAddress); err != nil {
						return err
					}
					outcome.Accepted, outcome.Reason, outcome.EmailID = true, "Email accepted", ids[i]
				}
				seen[recipient] = outcome
			}
			result.Recipients = append(result.Recipients, outcome)
			if outcome.Accepted {
				result.Accepted++
			} else {
				result.Rejected++
			}
		}
		return nil
	})
	if err != nil {
		return DeliveryResult{Recipients: []RecipientOutcome{}}, err
	}
	return result, nil
}

// GetInboxEmails 缺失、过期、禁用邮箱均返回 []；有效读取更新访问时间。
func (s *Service) GetInboxEmails(ctx context.Context, address string) ([]Email, error) {
	result := []Email{}
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		m, err := s.mailbox(ctx, tx, address)
		if err != nil || m == nil {
			return err
		}
		if s.now().Unix() > m.ExpiresAt || !m.IsActive {
			return nil
		}
		if _, err := tx.ExecContext(ctx, "UPDATE mailboxes SET last_accessed = ? WHERE id = ?", s.now().Unix(), m.ID); err != nil {
			return err
		}
		result, err = queryEmails(ctx, tx, m.ID, s.cfg.MaxEmailsPerAddress)
		return err
	})
	return result, err
}

// GetEmailByID 用 mailbox_id 验证归属并标为已读；返回标记前的 is_read。
func (s *Service) GetEmailByID(ctx context.Context, address, emailID string) (*Email, error) {
	var result *Email
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		m, err := s.mailbox(ctx, tx, address)
		if err != nil || m == nil {
			return err
		}
		if s.now().Unix() > m.ExpiresAt || !m.IsActive {
			return nil
		}
		if _, err := tx.ExecContext(ctx, "UPDATE mailboxes SET last_accessed = ? WHERE id = ?", s.now().Unix(), m.ID); err != nil {
			return err
		}
		e, err := scanEmail(tx.QueryRowContext(ctx, emailSelect+" WHERE id = ? AND mailbox_id = ?", emailID, m.ID))
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE emails SET is_read = 1 WHERE id = ?", emailID); err != nil {
			return err
		}
		result = e
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Service) GetEmailsByMailbox(ctx context.Context, mailboxID string, limit ...int) ([]Email, error) {
	n := 0
	if len(limit) > 0 {
		n = limit[0]
	}
	return queryEmails(ctx, s.store.DB(), mailboxID, n)
}

// GetMailboxByToken 读取认证快照，后续写入必须使用返回的不可变 ID。
func (s *Service) GetMailboxByToken(ctx context.Context, token string) (*Mailbox, error) {
	return scanMailbox(s.store.DB().QueryRowContext(ctx, mailboxSelect+" WHERE access_token = ?", s.cfg.MaxMailboxSizeBytes, token))
}

// GetMailboxInfo 包含过期、禁用邮箱，不更新访问时间。
func (s *Service) GetMailboxInfo(ctx context.Context, address string) (*MailboxInfo, error) {
	var result *MailboxInfo
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		m, err := s.mailbox(ctx, tx, address)
		if err != nil || m == nil {
			return err
		}
		info := &MailboxInfo{ID: m.ID, Address: m.Address, CreatedAt: m.CreatedAt, ExpiresAt: m.ExpiresAt,
			RetentionDays: m.RetentionDays, SenderWhitelist: m.SenderWhitelist, WhitelistEnabled: m.WhitelistEnabled,
			AccessToken: m.AccessToken, MailboxKey: m.MailboxKey, IsActive: m.IsActive, AdminDisabled: m.AdminDisabled,
			IsExpired: s.now().Unix() > m.ExpiresAt, nullToken: m.nullToken, nullRetention: m.nullRetention}
		var lastEmail store.LegacyInt64
		err = tx.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(CASE WHEN is_read = 0 THEN 1 END), MAX(timestamp)
 FROM emails WHERE mailbox_id = ?`, m.ID).Scan(&info.EmailCount, &info.UnreadCount, &lastEmail)
		info.LastEmailTime = lastEmail.Pointer()
		result = info
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
