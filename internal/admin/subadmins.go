package admin

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/google/uuid"
	"maildrop/internal/store"
)

type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }
func invalid(message string) error       { return &ValidationError{message} }

func normalizeDomains(value any) ([]string, error) {
	var submitted []string
	switch list := value.(type) {
	case []string:
		submitted = list
	case []any:
		for _, v := range list {
			text, ok := v.(string)
			if !ok {
				return nil, invalid("域名格式不正确")
			}
			submitted = append(submitted, text)
		}
	// Python 支持逗号分隔字符串；逐项规范化，禁止隐式接受无效域名。
	case string:
		submitted = strings.Split(list, ",")
	default:
		return nil, invalid("域名格式不正确")
	}
	domains := []string{}
	for _, value := range submitted {
		domain, err := store.NormalizeDomain(value)
		if err != nil {
			return nil, invalid("域名格式不正确")
		}
		domains = append(domains, domain)
	}
	return domains, nil
}

func commaWhitelist(value any) (any, error) {
	values, err := normalizeWhitelist(value)
	if err != nil {
		return nil, invalid(err.Error())
	}
	if len(values) == 0 {
		return nil, nil
	}
	return strings.Join(values, ","), nil
}

func subRetention(value any) (int, error) {
	days, err := Integer(value)
	if err != nil {
		return 0, invalid("最长保留天数必须是有效的数字")
	}
	if days < 1 || days > 36500 {
		return 0, invalid("最长保留天数必须在1-36500之间")
	}
	return days, nil
}

func (s *Service) ListSubAdmins() ([]store.SubAdmin, error) { return s.store.GetAllSubAdmins() }

func (s *Service) CreateSubAdmin(ctx context.Context, data map[string]any) (map[string]any, error) {
	token, _ := data["token"].(string)
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, invalid("Token不能为空")
	}
	if !Truthy(data["domains"]) {
		return nil, invalid("至少需要分配一个域名")
	}
	domains, err := normalizeDomains(data["domains"])
	if err != nil {
		return nil, err
	}
	rawDays, hasDays := data["max_retention_days"]
	if !hasDays {
		rawDays = 30
	}
	days, err := subRetention(rawDays)
	if err != nil {
		return nil, err
	}
	var whitelist any
	if value, exists := data["sender_whitelist"]; exists {
		whitelist, err = commaWhitelist(value)
		if err != nil {
			return nil, err
		}
	}
	notes, _ := data["notes"].(string)
	notes = strings.TrimSpace(notes)
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var existing string
		err := tx.QueryRowContext(ctx, "SELECT id FROM sub_admins WHERE token=? AND is_active=1", token).Scan(&existing)
		if err == nil {
			return invalid("Token已存在")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sub_admins (id,token,domains,sender_whitelist,max_retention_days,created_at,updated_at,created_by,notes) VALUES (?,?,?,?,?,?,?,?,?)`,
			id.String(), token, strings.Join(domains, ","), whitelist, days, s.now().Unix(), s.now().Unix(), "admin", notes)
		return err
	})
	return map[string]any{"id": id.String(), "token": token}, err
}

func (s *Service) UpdateSubAdmin(ctx context.Context, id string, data map[string]any) error {
	fields := []string{}
	args := []any{}
	for _, key := range []string{"domains", "sender_whitelist", "max_retention_days", "is_active", "notes"} {
		v, ok := data[key]
		if !ok || (v == nil && key != "sender_whitelist") {
			continue
		}
		switch key {
		case "domains":
			domains, err := normalizeDomains(v)
			if err != nil {
				return err
			}
			v = strings.Join(domains, ",")
		case "sender_whitelist":
			value, err := commaWhitelist(v)
			if err != nil {
				return err
			}
			v = value
		case "max_retention_days":
			days, err := subRetention(v)
			if err != nil {
				return err
			}
			v = days
		}
		fields = append(fields, key+"=?")
		args = append(args, v)
	}
	fields = append(fields, "updated_at=?")
	args = append(args, s.now().Unix(), id)
	return s.store.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE sub_admins SET "+strings.Join(fields, ",")+" WHERE id=?", args...)
		return err
	})
}

func (s *Service) DeleteSubAdmin(ctx context.Context, id string) error {
	return s.store.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM sub_admins WHERE id=?", id)
		return err
	})
}
