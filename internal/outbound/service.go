// Package outbound 提供主管理员授权的发件、持久队列及发送记录。
package outbound

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"maildrop/internal/config"
	"maildrop/internal/mailparse"
	"maildrop/internal/store"
)

const ArchiveLimit int64 = 100 << 20
const AttachmentLimit int64 = 10 << 20
const Retention = 30 * 24 * time.Hour

type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string                 { return e.Message }
func problem(status int, message string) error { return &Error{status, message} }

type Upload struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Content     []byte `json:"content,omitempty"`
	ID          string `json:"id,omitempty"`
	Size        int64  `json:"size,omitempty"`
}
type Request struct {
	To           []string `json:"to"`
	Cc           []string `json:"cc,omitempty"`
	Bcc          []string `json:"bcc,omitempty"`
	Subject      string   `json:"subject"`
	Text         string   `json:"text,omitempty"`
	HTML         string   `json:"html,omitempty"`
	ReplyEmailID string   `json:"reply_email_id,omitempty"`
	Attachments  []Upload `json:"attachments,omitempty"`
}
type Policy struct {
	Enabled      bool   `json:"enabled"`
	DailyLimit   int    `json:"daily_limit"`
	MinuteLimit  int    `json:"minute_limit"`
	DailyUsed    int    `json:"daily_used"`
	MinuteUsed   int    `json:"minute_used"`
	StorageUsed  int64  `json:"storage_used"`
	StorageLimit int64  `json:"storage_limit"`
	Available    bool   `json:"available"`
	Reason       string `json:"reason,omitempty"`
}
type Recipient struct {
	Address   string `json:"address"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	Uncertain bool   `json:"uncertain,omitempty"`
	Attempts  int    `json:"attempts"`
}
type Sent struct {
	ID          string      `json:"id"`
	CreatedAt   int64       `json:"created_at"`
	Status      string      `json:"status"`
	MessageID   string      `json:"message_id"`
	Details     Request     `json:"details"`
	Recipients  []Recipient `json:"recipients"`
	BounceCount int         `json:"bounce_count"`
	Reports     []Report    `json:"reports,omitempty"`
}
type Report struct {
	ID         string `json:"id"`
	ReceivedAt int64  `json:"received_at"`
	Summary    string `json:"summary"`
}

type Transport interface {
	Deliver(context.Context, string, string, []byte) error
}
type DeliveryError struct {
	Permanent bool
	Uncertain bool
	Reason    string
}

func (e *DeliveryError) Error() string { return e.Reason }

type Service struct {
	cfg       config.Config
	db        *store.Store
	now       func() time.Time
	transport Transport
	sign      func(string, []byte) ([]byte, error)
	wake      chan struct{}
}

func New(cfg *config.Config, db *store.Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	s := &Service{cfg: *cfg, db: db, now: now, wake: make(chan struct{}, 1)}
	s.transport = &DirectTransport{Hostname: cfg.SMTPHostname, IPv4Only: cfg.OutboundIPv4Only}
	s.sign = s.signMessage
	return s
}

func dayStart(now time.Time) int64 {
	local := now.In(time.FixedZone("Asia/Shanghai", 8*3600))
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location()).Unix()
}
func policyTx(ctx context.Context, tx *sql.Tx, mailboxID string) (Policy, error) {
	p := Policy{DailyLimit: 100, MinuteLimit: 10, StorageLimit: ArchiveLimit}
	err := tx.QueryRowContext(ctx, `SELECT enabled,daily_limit,minute_limit FROM sending_policies WHERE mailbox_id=?`, mailboxID).Scan(&p.Enabled, &p.DailyLimit, &p.MinuteLimit)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return p, err
}
func usageTx(ctx context.Context, tx *sql.Tx, mailboxID string, now time.Time, p *Policy) error {
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN created_at>=? THEN recipient_count ELSE 0 END),0),COALESCE(SUM(CASE WHEN created_at>? THEN recipient_count ELSE 0 END),0)
 FROM outbound_requests WHERE mailbox_id=? AND created_at>=?`, dayStart(now), now.Unix()-60, mailboxID, min(dayStart(now), now.Unix()-60)).Scan(&p.DailyUsed, &p.MinuteUsed); err != nil {
		return err
	}
	return tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes),0)+(SELECT COALESCE(SUM(length(b.raw)),0) FROM outbound_bounces b JOIN outbound_messages m ON m.id=b.message_id WHERE m.mailbox_id=?) FROM outbound_messages WHERE mailbox_id=?`, mailboxID, mailboxID).Scan(&p.StorageUsed)
}
func (s *Service) Policy(ctx context.Context, mailboxID string) (Policy, error) {
	var p Policy
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		p, err = policyTx(ctx, tx, mailboxID)
		if err != nil {
			return err
		}
		return usageTx(ctx, tx, mailboxID, s.now(), &p)
	})
	p.Available = s.cfg.OutboundEnabled && validDomain(s.cfg.SMTPHostname)
	if !p.Available {
		p.Reason = "服务器尚未配置发件，请联系主管理员"
	}
	return p, err
}
func (s *Service) SetPolicy(ctx context.Context, mailboxID string, p Policy, ip string) error {
	if p.DailyLimit < 1 || p.DailyLimit > 100000 || p.MinuteLimit < 1 || p.MinuteLimit > 1000 {
		return problem(400, "额度必须为正整数，每日最多 100000、每分钟最多 1000")
	}
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailboxes WHERE id=?`, mailboxID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return store.ErrNotFound
		}
		previous, err := policyTx(ctx, tx, mailboxID)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO sending_policies(mailbox_id,enabled,daily_limit,minute_limit) VALUES (?,?,?,?) ON CONFLICT(mailbox_id) DO UPDATE SET enabled=excluded.enabled,daily_limit=excluded.daily_limit,minute_limit=excluded.minute_limit`, mailboxID, p.Enabled, p.DailyLimit, p.MinuteLimit); err != nil {
			return err
		}
		if !p.Enabled {
			if _, err = tx.ExecContext(ctx, `UPDATE outbound_recipients SET cancelled=1 WHERE status='sending' AND message_id IN (SELECT id FROM outbound_messages WHERE mailbox_id=?)`, mailboxID); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE outbound_recipients SET status='failed',last_error='发件权限已撤销' WHERE status IN ('queued','retry') AND message_id IN (SELECT id FROM outbound_messages WHERE mailbox_id=?)`, mailboxID); err != nil {
				return err
			}
		}
		changes, _ := json.Marshal(map[string]any{"old": previous, "new": p})
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_logs(id,timestamp,action,mailbox_id,admin_user,changes,ip_address) VALUES (?,?,?,?,?,?,?)`, uuid.NewString(), s.now().Unix(), "SENDING_POLICY", mailboxID, "admin", string(changes), ip)
		return err
	})
}

func (s *Service) Submit(ctx context.Context, mailboxID, key string, req Request) (*Sent, error) {
	if len(key) == 0 || len(key) > 128 || strings.ContainsAny(key, "\r\n") {
		return nil, problem(400, "需要有效的 Idempotency-Key 请求头（最多128字符）")
	}
	req.Attachments = append([]Upload(nil), req.Attachments...)
	recipients, err := normalize(&req)
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(req)
	digest := sha256.Sum256(encoded)
	hash := hex.EncodeToString(digest[:])
	var previousID, previousHash string
	err = s.db.DB().QueryRowContext(ctx, `SELECT message_id,request_hash FROM outbound_requests WHERE mailbox_id=? AND request_key=?`, mailboxID, key).Scan(&previousID, &previousHash)
	if err == nil {
		if hash != previousHash {
			return nil, problem(409, "此请求标识已用于不同邮件")
		}
		sent, err := s.Get(ctx, mailboxID, previousID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, problem(410, "该请求已处理，发送记录已删除")
		}
		return sent, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	policy, err := s.Policy(ctx, mailboxID)
	if err != nil {
		return nil, err
	}
	if !policy.Enabled {
		return nil, problem(403, "此邮箱尚未获得主管理员发件授权")
	}
	if !s.cfg.OutboundEnabled || !validDomain(s.cfg.SMTPHostname) {
		return nil, problem(503, "服务器尚未配置发件")
	}
	var from string
	if err = s.db.DB().QueryRowContext(ctx, `SELECT address FROM mailboxes WHERE id=?`, mailboxID).Scan(&from); err != nil {
		return nil, store.ErrNotFound
	}
	var reply *mailparse.Details
	if req.ReplyEmailID != "" {
		content, err := s.db.ContentForEmail(ctx, mailboxID, req.ReplyEmailID)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
			// 老邮件没有保存原文，仍允许回复，但只使用能验证归属的发件地址。
			old, oldErr := s.db.GetEmailByID(req.ReplyEmailID)
			if oldErr != nil || old.MailboxID != mailboxID {
				return nil, problem(404, "原邮件不存在")
			}
			reply = &mailparse.Details{}
		} else {
			reply = &content.Details
		}
	}
	id := uuid.NewString()
	messageID := "<" + id + "@" + domainOf(from) + ">"
	bounce := "md-bounce-" + strings.ReplaceAll(uuid.NewString(), "-", "") + "@" + domainOf(from)
	now := s.now()
	raw, err := compose(from, messageID, req, reply, now)
	if err != nil {
		return nil, err
	}
	raw, err = s.sign(from, raw)
	if err != nil {
		return nil, problem(503, "发件域名的 DKIM 配置不可用")
	}
	if len(raw) > 20<<20 {
		return nil, problem(413, "编码后的邮件不能超过20 MiB")
	}
	archived := req
	archived.Attachments = make([]Upload, len(req.Attachments))
	for i, a := range req.Attachments {
		archived.Attachments[i] = Upload{Filename: a.Filename, ContentType: a.ContentType, ID: strconv.Itoa(i + 1), Size: int64(len(a.Content))}
	}
	details, _ := json.Marshal(archived)
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		now = s.now()
		var currentHash string
		lookupErr := tx.QueryRowContext(ctx, `SELECT message_id,request_hash FROM outbound_requests WHERE mailbox_id=? AND request_key=?`, mailboxID, key).Scan(&previousID, &currentHash)
		if lookupErr == nil {
			if currentHash != hash {
				return problem(409, "此请求标识已用于不同邮件")
			}
			id = previousID
			return nil
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return lookupErr
		}
		var active bool
		var expires int64
		if err := tx.QueryRowContext(ctx, `SELECT is_active,expires_at FROM mailboxes WHERE id=?`, mailboxID).Scan(&active, &expires); err != nil {
			return store.ErrNotFound
		}
		if !active || expires < now.Unix() {
			return problem(423, "邮箱已停用或过期")
		}
		p, err := policyTx(ctx, tx, mailboxID)
		if err != nil {
			return err
		}
		if !p.Enabled {
			return problem(403, "此邮箱尚未获得主管理员发件授权")
		}
		if err = usageTx(ctx, tx, mailboxID, now, &p); err != nil {
			return err
		}
		if p.DailyUsed+len(recipients) > p.DailyLimit || p.MinuteUsed+len(recipients) > p.MinuteLimit {
			return problem(429, "已达到收件人数额度，请稍后重试或联系主管理员")
		}
		if p.StorageUsed+int64(len(raw)) > ArchiveLimit {
			return problem(409, "发件存档容量不足，请先清理发送记录")
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO outbound_messages(id,mailbox_id,created_at,details,raw,size_bytes,message_id,bounce_address) VALUES (?,?,?,?,?,?,?,?)`, id, mailboxID, now.Unix(), string(details), raw, len(raw), messageID, bounce); err != nil {
			return err
		}
		for _, recipient := range recipients {
			if _, err = tx.ExecContext(ctx, `INSERT INTO outbound_recipients(message_id,address,next_attempt) VALUES (?,?,?)`, id, recipient, now.Unix()); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO outbound_requests(mailbox_id,request_key,request_hash,message_id,created_at,recipient_count) VALUES (?,?,?,?,?,?)`, mailboxID, key, hash, id, now.Unix(), len(recipients))
		return err
	})
	if err != nil {
		return nil, err
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return s.Get(ctx, mailboxID, id)
}

func (s *Service) Get(ctx context.Context, mailboxID, id string) (*Sent, error) {
	var sent Sent
	var details string
	err := s.db.DB().QueryRowContext(ctx, `SELECT id,created_at,message_id,details FROM outbound_messages WHERE id=? AND mailbox_id=?`, id, mailboxID).Scan(&sent.ID, &sent.CreatedAt, &sent.MessageID, &details)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(details), &sent.Details); err != nil {
		return nil, err
	}
	sent.Recipients = []Recipient{}
	rows, err := s.db.DB().QueryContext(ctx, `SELECT address,status,last_error,uncertain,attempts FROM outbound_recipients WHERE message_id=? ORDER BY address`, id)
	if err != nil {
		return nil, err
	}
	accepted, failed, pending, bounced := 0, 0, 0, 0
	for rows.Next() {
		var r Recipient
		if err = rows.Scan(&r.Address, &r.Status, &r.Error, &r.Uncertain, &r.Attempts); err != nil {
			rows.Close()
			return nil, err
		}
		sent.Recipients = append(sent.Recipients, r)
		switch r.Status {
		case "accepted":
			accepted++
		case "failed":
			failed++
		case "bounced":
			bounced++
		default:
			pending++
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	sent.Status = "accepted"
	switch {
	case pending > 0:
		sent.Status = "queued"
	case bounced > 0:
		sent.Status = "bounced"
	case failed > 0 && accepted > 0:
		sent.Status = "partial_failed"
	case failed > 0:
		sent.Status = "failed"
	}
	rows, err = s.db.DB().QueryContext(ctx, `SELECT id,received_at,summary FROM outbound_bounces WHERE message_id=? ORDER BY received_at DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r Report
		if err = rows.Scan(&r.ID, &r.ReceivedAt, &r.Summary); err != nil {
			return nil, err
		}
		sent.Reports = append(sent.Reports, r)
	}
	sent.BounceCount = len(sent.Reports)
	return &sent, rows.Err()
}
func (s *Service) List(ctx context.Context, mailboxID string, offset int) ([]Sent, error) {
	rows, err := s.db.DB().QueryContext(ctx, `SELECT id FROM outbound_messages WHERE mailbox_id=? ORDER BY created_at DESC,rowid DESC LIMIT 50 OFFSET ?`, mailboxID, offset)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []Sent{}
	for _, id := range ids {
		item, err := s.Get(ctx, mailboxID, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		item.Details.Text = ""
		item.Details.HTML = ""
		out = append(out, *item)
	}
	return out, nil
}
func (s *Service) Raw(ctx context.Context, mailboxID, id string) ([]byte, error) {
	var raw []byte
	err := s.db.DB().QueryRowContext(ctx, `SELECT raw FROM outbound_messages WHERE id=? AND mailbox_id=?`, id, mailboxID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		err = store.ErrNotFound
	}
	return raw, err
}
func (s *Service) Delete(ctx context.Context, mailboxID, id string) error {
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var pending int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_recipients r JOIN outbound_messages m ON m.id=r.message_id WHERE m.id=? AND m.mailbox_id=? AND r.status IN ('queued','retry','sending')`, id, mailboxID).Scan(&pending); err != nil {
			return err
		}
		if pending > 0 {
			return problem(409, "邮件正在投递，暂时不能删除")
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM outbound_messages WHERE id=? AND mailbox_id=?`, id, mailboxID)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			return store.ErrNotFound
		}
		return nil
	})
}
func (s *Service) Clean(ctx context.Context) error {
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		cutoff := s.now().Add(-Retention).Unix()
		if _, err := tx.ExecContext(ctx, `DELETE FROM outbound_messages WHERE created_at<?`, cutoff); err != nil {
			return err
		}
		// 幂等墓碑保留至邮箱删除；即使存档到期，旧请求也不能重新入队。
		return nil
	})
}
func domainOf(address string) string {
	_, domain, _ := strings.Cut(address, "@")
	return strings.ToLower(domain)
}
