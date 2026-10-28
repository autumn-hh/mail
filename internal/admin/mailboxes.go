package admin

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"maildrop/internal/store"
)

type ListOptions struct {
	Page, PageSize         int
	Search, Status, Source string
}
type MailboxList struct {
	Mailboxes  []map[string]any `json:"mailboxes"`
	Total      int              `json:"total"`
	Page       int              `json:"page"`
	PageSize   int              `json:"page_size"`
	TotalPages int              `json:"total_pages"`
}
type CreateOptions struct {
	Address         string
	RetentionDays   *int
	SenderWhitelist any
	AllowedDomains  any
}

func expiredAt(value any, now int64) bool {
	switch expiry := value.(type) {
	case int64:
		return expiry <= now
	case float64:
		return expiry <= float64(now)
	default:
		return false
	}
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// scanMaps 保留旧数据库中的 null；服务响应不复用 Store 的定制 MarshalJSON。
func scanMaps(ctx context.Context, q queryer, query string, args ...any) ([]map[string]any, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	result := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		item := make(map[string]any, len(columns))
		for i, key := range columns {
			v := values[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			switch key {
			case "is_active", "admin_disabled", "whitelist_enabled":
				v = Truthy(v)
			case "sender_whitelist", "allowed_domains":
				parsed := any([]any{})
				if text, ok := v.(string); ok && text != "" {
					if err := json.Unmarshal([]byte(text), &parsed); err != nil {
						return nil, err
					}
				}
				v = parsed
			}
			item[key] = v
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

const mailboxFields = `m.id,m.address,m.created_at,m.expires_at,m.retention_days,m.is_active,m.admin_disabled,
 m.sender_whitelist,m.whitelist_enabled,m.created_by_ip,m.last_accessed,m.updated_by_admin,m.updated_at,m.created_source`

func (s *Service) ListMailboxes(ctx context.Context, o ListOptions) (*MailboxList, error) {
	// HTTP 层已归一分页参数；这里兜底防止其他调用方传入非正数。
	if o.Page < 1 {
		o.Page = 1
	}
	if o.PageSize < 1 {
		o.PageSize = 20
	}
	where := []string{"1=1"}
	args := []any{}
	now := s.now().Unix()
	if o.Search != "" {
		where = append(where, "(m.address LIKE ? OR m.id LIKE ?)")
		args = append(args, "%"+o.Search+"%", "%"+o.Search+"%")
	}
	switch o.Status {
	case "active":
		where = append(where, "m.is_active = 1 AND m.expires_at > ?")
		args = append(args, now)
	case "expired":
		where = append(where, "m.expires_at <= ?")
		args = append(args, now)
	case "disabled":
		where = append(where, "m.is_active = 0")
	}
	if o.Source != "" && o.Source != "all" {
		where = append(where, "m.created_source = ?")
		args = append(args, o.Source)
	}
	filter := strings.Join(where, " AND ")
	result := &MailboxList{Mailboxes: []map[string]any{}, Page: o.Page, PageSize: o.PageSize}
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailboxes m WHERE "+filter, args...).Scan(&result.Total); err != nil {
			return err
		}
		listArgs := append(append([]any{}, args...), o.PageSize, (o.Page-1)*o.PageSize)
		// 先分页再批量聚合，仅统计当前页的邮件，保持 Python 的降序及 LIKE 语义。
		query := `WITH page AS (SELECT ` + mailboxFields + ` FROM mailboxes m WHERE ` + filter + ` ORDER BY m.created_at DESC LIMIT ? OFFSET ?),
 stats AS (SELECT e.mailbox_id, COUNT(*) AS email_count, COUNT(CASE WHEN e.is_read=0 THEN 1 END) AS unread_count
 FROM emails e JOIN page p ON p.id=e.mailbox_id GROUP BY e.mailbox_id)
 SELECT p.*,COALESCE(stats.email_count,0) AS email_count,COALESCE(stats.unread_count,0) AS unread_count
 FROM page p LEFT JOIN stats ON p.id=stats.mailbox_id ORDER BY p.created_at DESC`
		items, err := scanMaps(ctx, tx, query, listArgs...)
		if err != nil {
			return err
		}
		for _, item := range items {
			item["is_expired"] = expiredAt(item["expires_at"], now)
		}
		result.Mailboxes = items
		result.TotalPages = (result.Total + o.PageSize - 1) / o.PageSize
		return nil
	})
	return result, err
}

func (s *Service) mailboxDetail(ctx context.Context, q queryer, id string) (map[string]any, error) {
	items, err := scanMaps(ctx, q, `SELECT `+mailboxFields+`,m.access_token,m.mailbox_key,m.storage_used,m.storage_limit,m.allowed_domains,
 (SELECT COUNT(*) FROM emails e WHERE e.mailbox_id=m.id) AS email_count,
 (SELECT COUNT(*) FROM emails e WHERE e.mailbox_id=m.id AND e.is_read=0) AS unread_count,
 (SELECT MAX(timestamp) FROM emails e WHERE e.mailbox_id=m.id) AS last_email_time
 FROM mailboxes m WHERE m.id=?`, id)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	item := items[0]
	delete(item, "created_source")
	item["is_expired"] = expiredAt(item["expires_at"], s.now().Unix())
	number := func(key string) float64 {
		if v, ok := item[key].(int64); ok {
			return float64(v)
		}
		return 0
	}
	used, limit := number("storage_used"), number("storage_limit")
	item["storage_used_mb"], item["storage_limit_mb"] = used/1048576, limit/1048576
	percent := float64(0)
	if limit > 0 {
		percent = used / limit * 100
	}
	item["storage_percent"] = percent
	return item, nil
}

func (s *Service) GetMailboxDetail(ctx context.Context, id string) (map[string]any, error) {
	return s.mailboxDetail(ctx, s.store.DB(), id)
}

func (s *Service) CreateMailbox(ctx context.Context, o CreateOptions, actor Actor) (map[string]any, error) {
	if err := s.ValidateAddress(o.Address); err != nil {
		return nil, err
	}
	var result map[string]any
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var expiry float64
		err := tx.QueryRowContext(ctx, "SELECT expires_at FROM mailboxes WHERE address=?", o.Address).Scan(&expiry)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("创建失败: %w", err)
		}
		if err == nil && float64(s.now().Unix()) <= expiry {
			return errors.New("邮箱地址已存在")
		}
		days := s.cfg.MailboxRetentionDays
		if o.RetentionDays != nil {
			days = *o.RetentionDays
		}
		if err := s.ValidateRetentionDays(float64(days)); err != nil {
			return err
		}
		whitelist := []string{}
		if o.SenderWhitelist != nil {
			whitelist, err = s.normalizeSenderWhitelist(o.SenderWhitelist)
			if err != nil {
				return err
			}
		}
		whitelistJSON, err := encoded(whitelist)
		if err != nil {
			return err
		}
		domains := []string{}
		if o.AllowedDomains != nil {
			domains, err = normalizeAllowedDomains(o.AllowedDomains)
			if err != nil {
				return err
			}
		}
		domainsJSON, err := encoded(domains)
		if err != nil {
			return err
		}
		id, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		key, err := store.GenerateMailboxKey()
		if err != nil {
			return err
		}
		rawToken := make([]byte, 32)
		if _, err := rand.Read(rawToken); err != nil {
			return err
		}
		token := base64.RawURLEncoding.EncodeToString(rawToken)
		now := s.now().Unix()
		expires := now + int64(days)*86400
		ip := actor.IP
		if ip == "" {
			ip = "unknown"
		}
		// allowed_domains 与邮箱、审计一并提交，失败全部回滚。
		_, err = tx.ExecContext(ctx, `INSERT INTO mailboxes
 (id,address,created_at,expires_at,retention_days,sender_whitelist,whitelist_enabled,created_by_ip,access_token,mailbox_key,last_accessed,created_source,allowed_domains)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, id.String(), o.Address, now, expires, days, whitelistJSON, Truthy(whitelist), ip, token, key, now, "admin", domainsJSON)
		if err != nil {
			return fmt.Errorf("创建失败: %w", err)
		}
		mailboxID := id.String()
		if err := s.logAuditTx(ctx, tx, "CREATE", &mailboxID, map[string]any{"address": o.Address, "retention_days": days}, actor); err != nil {
			return fmt.Errorf("创建失败: %w", err)
		}
		result = map[string]any{"id": mailboxID, "address": o.Address, "created_at": now, "expires_at": expires, "retention_days": days,
			"sender_whitelist": whitelist, "whitelist_enabled": Truthy(whitelist), "access_token": token, "mailbox_key": key, "is_active": true, "created_source": "admin"}
		if o.AllowedDomains != nil {
			result["allowed_domains"] = domains
		}
		return nil
	})
	return result, err
}

func (s *Service) UpdateMailbox(ctx context.Context, id string, updates map[string]any, actor Actor) error {
	return s.store.WithTx(ctx, func(tx *sql.Tx) error {
		current, err := s.mailboxDetail(ctx, tx, id)
		if err != nil {
			return fmt.Errorf("更新失败: %w", err)
		}
		if current == nil {
			return errors.New("邮箱不存在")
		}
		fields := []string{}
		args := []any{}
		changes := map[string]any{}
		for _, key := range []string{"retention_days", "sender_whitelist", "whitelist_enabled", "is_active", "allowed_domains"} {
			value, ok := updates[key]
			if !ok {
				continue
			}
			switch key {
			case "retention_days":
				var days float64
				switch v := value.(type) {
				case float64:
					days = v
				case int:
					days = float64(v)
				case bool:
					if v {
						days = 1
					}
				default:
					return errors.New("保留天数必须是有效的数字")
				}
				if err := s.ValidateRetentionDays(days); err != nil {
					return err
				}
				fields = append(fields, "expires_at = ?")
				args = append(args, float64(s.now().Unix())+days*86400)
			case "sender_whitelist":
				value, err = s.normalizeSenderWhitelist(value)
				if err != nil {
					return err
				}
			case "allowed_domains":
				value, err = normalizeAllowedDomains(value)
				if err != nil {
					return err
				}
			case "whitelist_enabled":
				value = Truthy(value)
			case "is_active":
				active := Truthy(value)
				value = active
				fields = append(fields, "admin_disabled = ?")
				args = append(args, !active)
				changes["admin_disabled"] = map[string]any{"old": current["admin_disabled"], "new": !active}
			}
			fields = append(fields, key+" = ?")
			stored := value
			if key == "sender_whitelist" || key == "allowed_domains" {
				data, err := encoded(value)
				if err != nil {
					return err
				}
				stored = data
			}
			args = append(args, stored)
			changes[key] = map[string]any{"old": current[key], "new": value}
		}
		if len(fields) == 0 {
			return errors.New("没有需要更新的字段")
		}
		fields = append(fields, "updated_by_admin = ?", "updated_at = ?")
		args = append(args, user(actor), s.now().Unix(), id)
		if _, err := tx.ExecContext(ctx, "UPDATE mailboxes SET "+strings.Join(fields, ",")+" WHERE id=?", args...); err != nil {
			return fmt.Errorf("更新失败: %w", err)
		}
		return s.logAuditTx(ctx, tx, "UPDATE", &id, changes, actor)
	})
}

func (s *Service) deleteMailboxTx(ctx context.Context, tx *sql.Tx, id string, soft bool, actor Actor) error {
	var address string
	if err := tx.QueryRowContext(ctx, "SELECT address FROM mailboxes WHERE id=?", id).Scan(&address); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("邮箱不存在")
		}
		return err
	}
	action := "SOFT_DELETE"
	auditID := &id
	changes := map[string]any{"address": address}
	if soft {
		changes["admin_disabled"] = true
		if _, err := tx.ExecContext(ctx, "UPDATE mailboxes SET is_active=0,admin_disabled=1,updated_by_admin=?,updated_at=? WHERE id=?", user(actor), s.now().Unix(), id); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, "DELETE FROM emails WHERE mailbox_id=?", id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM mailboxes WHERE id=?", id); err != nil {
			return err
		}
		// 外键开启：删除后审计不再引用已删除行，原 ID 写入 changes。
		action, auditID = "HARD_DELETE", nil
		changes["mailbox_id"] = id
	}
	return s.logAuditTx(ctx, tx, action, auditID, changes, actor)
}

func (s *Service) DeleteMailbox(ctx context.Context, id string, soft bool, actor Actor) error {
	return s.store.WithTx(ctx, func(tx *sql.Tx) error { return s.deleteMailboxTx(ctx, tx, id, soft, actor) })
}

type BatchError struct {
	MailboxID string `json:"mailbox_id"`
	Error     string `json:"error"`
}
type BatchResult struct {
	Success      bool         `json:"success"`
	Message      string       `json:"message"`
	SuccessCount int          `json:"success_count"`
	FailedCount  int          `json:"failed_count"`
	Errors       []BatchError `json:"errors"`
}

func (s *Service) BatchDelete(ctx context.Context, ids []string, soft bool, actor Actor) BatchResult {
	result := BatchResult{Success: true, Errors: []BatchError{}}
	// 每个邮箱一个原子事务；与 Python 一致保留部分成功结果。
	for _, id := range ids {
		if err := s.DeleteMailbox(ctx, id, soft, actor); err != nil {
			result.FailedCount++
			result.Errors = append(result.Errors, BatchError{id, err.Error()})
		} else {
			result.SuccessCount++
		}
	}
	result.Message = fmt.Sprintf("成功删除 %d 个邮箱，失败 %d 个", result.SuccessCount, result.FailedCount)
	return result
}
