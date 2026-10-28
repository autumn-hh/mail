// Package admin 实现管理端业务、校验和审计。
package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"maildrop/internal/addrpolicy"
	"maildrop/internal/config"
	"maildrop/internal/store"
)

type Actor struct {
	User string
	IP   string
}

type Service struct {
	cfg            *config.Config
	store          *store.Store
	now            func() time.Time
	addressPattern *regexp.Regexp
	domainPattern  *regexp.Regexp
	localPattern   *regexp.Regexp
	protected      *addrpolicy.Protected
}

func New(cfg *config.Config, db *store.Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{cfg: cfg, store: db, now: now,
		addressPattern: regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`),
		domainPattern:  regexp.MustCompile(`^[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`),
		localPattern:   regexp.MustCompile(`^[a-zA-Z0-9]{3,20}$`),
		protected:      addrpolicy.NewProtected(cfg.ProtectedAddresses)}
}

func (s *Service) ValidateLocalPart(address string) error {
	local, _, ok := strings.Cut(address, "@")
	if address == "" || !ok {
		return errors.New("邮箱地址格式不正确")
	}
	if !s.localPattern.MatchString(local) {
		return errors.New("用户名仅允许英文/数字，长度 3–20")
	}
	return nil
}

func (s *Service) ValidateAddress(address string) error {
	if address == "" {
		return errors.New("邮箱地址不能为空")
	}
	if !s.addressPattern.MatchString(address) {
		return errors.New("邮箱地址格式不正确")
	}
	// 保护规则与公开注册入口共用，见 internal/addrpolicy。
	return s.protected.Check(address)
}

func (s *Service) ValidateRetentionDays(days float64) error {
	if days < 1 {
		return errors.New("保留天数必须大于0")
	}
	if days > float64(s.cfg.MaxRetentionDays) {
		return fmt.Errorf("保留天数不能超过%d天", s.cfg.MaxRetentionDays)
	}
	return nil
}

func stringList(value any, listMessage, itemMessage string) ([]string, error) {
	switch list := value.(type) {
	case []string:
		return list, nil
	case []any:
		values := make([]string, len(list))
		for i, item := range list {
			text, ok := item.(string)
			if !ok {
				return nil, errors.New(itemMessage)
			}
			values[i] = text
		}
		return values, nil
	default:
		return nil, errors.New(listMessage)
	}
}

func normalizeWhitelist(value any) ([]string, error) {
	values, err := stringList(value, "白名单必须是列表格式", "白名单条目必须是字符串")
	if err != nil {
		return nil, err
	}
	result := []string{}
	seen := map[string]bool{}
	for _, sender := range values {
		sender = strings.TrimSpace(sender)
		key := strings.ToLower(sender)
		if sender != "" && !seen[key] {
			result = append(result, sender)
			seen[key] = true
		}
	}
	return result, nil
}

func (s *Service) normalizeSenderWhitelist(value any) ([]string, error) {
	values, err := normalizeWhitelist(value)
	if err != nil {
		return nil, err
	}
	for _, sender := range values {
		if strings.HasPrefix(sender, "@") {
			if !s.domainPattern.MatchString(sender[1:]) {
				return nil, fmt.Errorf("域名格式不正确: %s", sender)
			}
		} else if s.ValidateAddress(sender) != nil {
			return nil, fmt.Errorf("邮箱地址格式不正确: %s", sender)
		}
	}
	return values, nil
}

func (s *Service) ValidateSenderWhitelist(value any) error {
	_, err := s.normalizeSenderWhitelist(value)
	return err
}

func normalizeAllowedDomains(value any) ([]string, error) {
	values, err := stringList(value, "允许的域名必须是字符串列表", "允许的域名必须是字符串列表")
	if err != nil {
		return nil, err
	}
	result := []string{}
	seen := map[string]bool{}
	for _, item := range values {
		item = strings.TrimSpace(item)
		domain, err := store.NormalizeDomain(item)
		if err != nil {
			return nil, fmt.Errorf("允许的域名格式不正确: %s", item)
		}
		if !seen[domain] {
			result = append(result, domain)
			seen[domain] = true
		}
	}
	return result, nil
}

// Truthy 保留 Python 对状态开关及可选 JSON 值的布尔转换。
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case []any:
		return len(x) != 0
	case []string:
		return len(x) != 0
	case map[string]any:
		return len(x) != 0
	default:
		return true
	}
}

// Integer 对应创建接口的 Python int()，允许数字字符串、浮点截断和 bool。
func Integer(v any) (int, error) {
	switch x := v.(type) {
	case int:
		return x, nil
	case int64:
		return int(x), nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) || x >= float64(math.MaxInt) || x < float64(math.MinInt) {
			return 0, errors.New("invalid integer")
		}
		return int(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		return decimalInteger(x)
	default:
		return 0, errors.New("invalid integer")
	}
}

func user(actor Actor) string {
	if actor.User == "" {
		return "system"
	}
	return actor.User
}
func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
func encoded(value any) (string, error) { b, err := json.Marshal(value); return string(b), err }

func (s *Service) logAuditTx(ctx context.Context, tx *sql.Tx, action string, mailboxID *string, changes map[string]any, actor Actor) error {
	id, err := uuid.NewRandom()
	if err != nil {
		return err
	}
	if changes == nil {
		changes = map[string]any{}
	}
	data, err := encoded(changes)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_logs (id,timestamp,action,mailbox_id,admin_user,changes,ip_address) VALUES (?,?,?,?,?,?,?)`,
		id.String(), s.now().Unix(), action, mailboxID, user(actor), data, optionalString(actor.IP))
	return err
}

func (s *Service) LogAudit(ctx context.Context, action string, mailboxID *string, changes map[string]any, actor Actor) error {
	return s.store.WithTx(ctx, func(tx *sql.Tx) error { return s.logAuditTx(ctx, tx, action, mailboxID, changes, actor) })
}

// ClampAuditLogLimit 为 HTTP 与服务调用统一限制审计条数。
func ClampAuditLogLimit(mailboxID string, limit int) int {
	if limit < 1 {
		if mailboxID != "" {
			return 50
		}
		return 100
	}
	return min(limit, 1000)
}

func (s *Service) GetAuditLogs(ctx context.Context, mailboxID string, limit int) ([]store.AuditLog, error) {
	limit = ClampAuditLogLimit(mailboxID, limit)
	query := `SELECT id,timestamp,action,mailbox_id,admin_user,changes,ip_address FROM audit_logs`
	args := []any{}
	if mailboxID != "" {
		query += " WHERE mailbox_id = ?"
		args = append(args, mailboxID)
	}
	query += " ORDER BY timestamp DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logs := []store.AuditLog{}
	for rows.Next() {
		var item store.AuditLog
		var changes sql.NullString
		if err := rows.Scan(&item.ID, &item.Timestamp, &item.Action, &item.MailboxID, &item.AdminUser, &changes, &item.IPAddress); err != nil {
			return nil, err
		}
		item.Changes = map[string]any{}
		if changes.Valid && changes.String != "" {
			if err := json.Unmarshal([]byte(changes.String), &item.Changes); err != nil {
				return nil, err
			}
		}
		logs = append(logs, item)
	}
	return logs, rows.Err()
}

// 十进制字符串遵循 Python int 的下划线和 Unicode 数字规则。
func decimalInteger(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	var normalized strings.Builder
	if strings.HasPrefix(raw, "+") || strings.HasPrefix(raw, "-") {
		normalized.WriteByte(raw[0])
		raw = raw[1:]
	}
	previousDigit := false
	for _, ch := range raw {
		if ch == '_' {
			if !previousDigit {
				return 0, errors.New("invalid integer")
			}
			previousDigit = false
			continue
		}
		digit := -1
		if ch >= '0' && ch <= '9' {
			digit = int(ch - '0')
		} else {
			for _, span := range unicode.Digit.R16 {
				if uint32(ch) >= uint32(span.Lo) && uint32(ch) <= uint32(span.Hi) && (uint32(ch)-uint32(span.Lo))%uint32(span.Stride) == 0 {
					digit = int((uint32(ch)-uint32(span.Lo))/uint32(span.Stride)) % 10
					break
				}
			}
			if digit < 0 {
				for _, span := range unicode.Digit.R32 {
					if uint32(ch) >= span.Lo && uint32(ch) <= span.Hi && (uint32(ch)-span.Lo)%span.Stride == 0 {
						digit = int((uint32(ch)-span.Lo)/span.Stride) % 10
						break
					}
				}
			}
		}
		if digit < 0 {
			return 0, errors.New("invalid integer")
		}
		normalized.WriteByte(byte('0' + digit))
		previousDigit = true
	}
	if !previousDigit {
		return 0, errors.New("invalid integer")
	}
	return strconv.Atoi(normalized.String())
}
