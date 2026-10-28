package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
)

// LegacyInt64 兼容旧库的可空整数，REAL 向零截断。
type LegacyInt64 struct {
	Int64 int64
	Valid bool
}

func (n *LegacyInt64) Scan(value any) error {
	*n = LegacyInt64{}
	if value == nil {
		return nil
	}
	number, err := legacyInteger(value)
	if err != nil {
		return err
	}
	n.Int64, n.Valid = number, true
	return nil
}

func (n LegacyInt64) Pointer() *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}

type integerScanner struct{ dest *int64 }

// ScanInt64 保留必填列的 NULL 错误，兼容历史数值类型。
func ScanInt64(dest *int64) sql.Scanner { return integerScanner{dest} }

func (s integerScanner) Scan(value any) error {
	number, err := legacyInteger(value)
	if err != nil {
		return err
	}
	*s.dest = number
	return nil
}

func legacyInteger(value any) (int64, error) {
	switch v := value.(type) {
	case int64:
		return v, nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v < math.MinInt64 || v >= -float64(math.MinInt64) {
			return 0, fmt.Errorf("legacy integer out of range")
		}
		return int64(v), nil
	case []byte:
		return legacyInteger(string(v))
	case string:
		v = strings.TrimSpace(v)
		if number, err := strconv.ParseInt(v, 10, 64); err == nil {
			return number, nil
		} else if errors.Is(err, strconv.ErrRange) {
			return 0, fmt.Errorf("legacy integer out of range")
		}
		number, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid legacy integer")
		}
		return legacyInteger(number)
	default:
		return 0, fmt.Errorf("cannot scan %T as an integer", value)
	}
}

// DecodeMailboxList 仅读取字符串项；坏数据告警，不中断邮箱读取。
func DecodeMailboxList(raw sql.NullString, mailboxID, column string) []string {
	values := []string{}
	if !raw.Valid {
		return values
	}
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(raw.String), &items); err != nil || items == nil {
		slog.Warn("邮箱列表数据无效", "mailbox_id", mailboxID, "column", column)
		return values
	}
	dropped := 0
	for _, item := range items {
		var value string
		if len(item) == 0 || item[0] != '"' || json.Unmarshal(item, &value) != nil {
			dropped++
			continue
		}
		values = append(values, value)
	}
	if dropped > 0 {
		slog.Warn("邮箱列表忽略非字符串项", "mailbox_id", mailboxID, "column", column, "dropped", dropped)
	}
	return values
}
