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
			LastAccessed: &now, StorageLimit: s.cfg.MaxMailboxSizeBytes, created: true}
		_, err = tx.ExecContext(ctx, `INSERT INTO mailboxes (id, address, created_at, expires_at,
 retention_days, sender_whitelist, whitelist_enabled, created_by_ip, access_token, mailbox_key,
 last_accessed, created_source, storage_limit) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			result.ID, address, now, result.ExpiresAt, days, string(encoded), result.WhitelistEnabled,
			options.CreatedByIP, result.AccessToken, key, now, source, result.StorageLimit)
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

// RecipientStatus 在 RCPT 阶段检查可提前确定的条件；DATA 提交时再次校验。
func (s *Service) RecipientStatus(ctx context.Context, address string) (int, string, error) {
	var code int
	var reason string
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		m, err := s.deliveryMailbox(ctx, tx, address)
		if err != nil {
			return err
		}
		switch {
		case m == nil:
			code, reason = 550, "Mailbox does not exist"
		case s.now().Unix() > m.ExpiresAt:
			code, reason = 550, "Mailbox has expired"
		case !m.IsActive:
			code, reason = 550, "Mailbox is disabled"
		case m.StorageUsed >= m.StorageLimit:
			code, reason = 452, "Mailbox storage is full"
		}
		return nil
	})
	return code, reason, err
}

// Deliver 在同一事务中先校验全部收件人，再统一写入；任何失败都不部分投递。
func (s *Service) Deliver(ctx context.Context, envelopeFrom string, recipients []string, msg *mailparse.Message) (DeliveryResult, error) {
	result := DeliveryResult{Recipients: []RecipientOutcome{}}
	if msg == nil {
		return result, errors.New("nil message")
	}
	email := store.Email{From: msg.From, To: msg.To, Subject: msg.Subject, Body: msg.Body, ContentType: msg.ContentType, Timestamp: msg.Timestamp, Sent: msg.Sent}
	if email.ContentType == "" {
		email.ContentType = "Text"
	}
	size := store.CalculateEmailSize(email)
	if msg.Raw != nil {
		size = int64(len(msg.Raw))
	}
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		now := s.now().Unix()
		mailboxes := []*Mailbox{}
		seen := map[string]bool{}
		for _, recipient := range recipients {
			m, err := s.deliveryMailbox(ctx, tx, recipient)
			if err != nil {
				return err
			}
			key := recipient
			if m != nil {
				key = m.ID
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			o := RecipientOutcome{Recipient: recipient, Code: 550}
			switch {
			case recipient == "":
				o.Reason = "No recipient specified"
			case m == nil:
				o.Reason = fmt.Sprintf("Mailbox %s does not exist", recipient)
			case now > m.ExpiresAt:
				o.Reason = fmt.Sprintf("Mailbox %s has expired", recipient)
			case !m.IsActive:
				o.Reason = fmt.Sprintf("Mailbox %s is disabled", recipient)
			case !senderAllowed(m, msg.From) || !senderDomainAllowed(m, msg.From):
				o.Reason = fmt.Sprintf("Sender %s not allowed for mailbox %s", msg.From, recipient)
			case size > s.cfg.MaxEmailSizeBytes:
				o.Code, o.Reason = 552, "Message size exceeds limit"
			case size > m.StorageLimit-m.StorageUsed:
				o.Code, o.Reason = 452, "Mailbox storage is full"
			default:
				o.Code, o.Accepted, o.Reason = 250, true, "Email accepted"
			}
			if !o.Accepted {
				result.Rejected++
			}
			result.Recipients = append(result.Recipients, o)
			mailboxes = append(mailboxes, m)
		}
		if result.Rejected > 0 {
			// SMTP DATA 只有一个最终响应，不能在返回失败前提交部分邮箱。
			for i := range result.Recipients {
				if result.Recipients[i].Accepted {
					result.Recipients[i].Accepted = false
					result.Recipients[i].Code = 0
					result.Recipients[i].Reason = "Batch not committed"
				}
			}
			result.Rejected = len(result.Recipients)
			return nil
		}
		if len(mailboxes) == 0 {
			return nil
		}
		contentID := any(nil)
		if msg.Raw != nil {
			if msg.ID == "" {
				id, err := uuid.NewRandom()
				if err != nil {
					return err
				}
				msg.ID = id.String()
			}
			if err := store.SaveContent(ctx, tx, msg); err != nil {
				return err
			}
			contentID = msg.ID
		}
		for i, m := range mailboxes {
			id := msg.ID
			if i > 0 || id == "" {
				generated, err := uuid.NewRandom()
				if err != nil {
					return err
				}
				id = generated.String()
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO emails (id,mailbox_id,from_address,to_address,subject,body,content_type,timestamp,sent_formatted,size_bytes,content_id)
     VALUES (?,?,?,?,?,?,?,?,?,?,?)`, id, m.ID, email.From, email.To, email.Subject, email.Body, email.ContentType, email.Timestamp, email.Sent, size, contentID)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE mailboxes SET storage_used=COALESCE(storage_used,0)+?,last_accessed=? WHERE id=?`, size, now, m.ID); err != nil {
				return err
			}
			result.Recipients[i].EmailID = id
			result.Accepted++
		}
		for _, m := range mailboxes {
			if _, err := prune(ctx, tx, m.ID, s.cfg.MaxEmailsPerAddress); err != nil {
				return err
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
