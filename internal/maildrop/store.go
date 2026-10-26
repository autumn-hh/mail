package maildrop

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Object = map[string]any
type Store struct {
	DB     *sql.DB
	Config Config
}
type queryer interface {
	Query(string, ...any) (*sql.Rows, error)
	Exec(string, ...any) (sql.Result, error)
}

func query(q queryer, statement string, args ...any) ([]Object, error) {
	rows, err := q.Query(statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	result := []Object{}
	for rows.Next() {
		values := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err = rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		item := Object{}
		for i, key := range columns {
			v := values[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			item[key] = v
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
func one(q queryer, statement string, args ...any) (Object, error) {
	rows, err := query(q, statement, args...)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, sql.ErrNoRows
	}
	return rows[0], nil
}
func str(v any) string { s, _ := v.(string); return s }
func num(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}
func truth(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return num(v) != 0
}
func stringsValue(v any) []string {
	var out []string
	switch a := v.(type) {
	case string:
		if json.Unmarshal([]byte(a), &out) != nil {
			return []string{}
		}
	case []string:
		out = append(out, a...)
	case []any:
		for _, x := range a {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	if out == nil {
		return []string{}
	}
	return out
}
func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func OpenStore(c Config) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(c.DatabasePath), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(c.DatabasePath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", c.DatabasePath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db, Config: c}
	if err = s.initialize(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) initialize() error {
	if _, err := s.DB.Exec("PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL;"); err != nil {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	schema := []string{
		`CREATE TABLE IF NOT EXISTS mailboxes (id TEXT PRIMARY KEY,address TEXT UNIQUE NOT NULL,created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,retention_days INTEGER DEFAULT 7,is_active BOOLEAN DEFAULT 1,sender_whitelist TEXT DEFAULT '[]',whitelist_enabled BOOLEAN DEFAULT 0,created_by_ip TEXT,access_token TEXT,last_accessed INTEGER)`,
		`CREATE TABLE IF NOT EXISTS emails (id TEXT PRIMARY KEY,mailbox_id TEXT NOT NULL,from_address TEXT NOT NULL,to_address TEXT NOT NULL,subject TEXT,body TEXT,content_type TEXT DEFAULT 'Text',timestamp INTEGER NOT NULL,sent_formatted TEXT,is_read BOOLEAN DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS audit_logs (id TEXT PRIMARY KEY,timestamp INTEGER NOT NULL,action TEXT NOT NULL,mailbox_id TEXT,admin_user TEXT,changes TEXT,ip_address TEXT)`,
		`CREATE TABLE IF NOT EXISTS sub_admins (id TEXT PRIMARY KEY,token TEXT UNIQUE NOT NULL,domains TEXT NOT NULL,sender_whitelist TEXT,max_retention_days INTEGER DEFAULT 30,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,is_active INTEGER DEFAULT 1,created_by TEXT,notes TEXT)`,
		`CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY,value TEXT NOT NULL,updated_at INTEGER NOT NULL)`,
	}
	for _, stmt := range schema {
		if _, err = tx.Exec(stmt); err != nil {
			return err
		}
	}
	migrations := map[string]map[string]string{
		"mailboxes": {"mailbox_key": "TEXT", "whitelist_enabled": "BOOLEAN DEFAULT 0", "created_source": "TEXT DEFAULT 'unknown'", "updated_by_admin": "TEXT", "updated_at": "INTEGER", "allowed_domains": "TEXT DEFAULT '[]'", "storage_used": "INTEGER DEFAULT 0", "storage_limit": "INTEGER DEFAULT 3145728"},
		"emails":    {"size_bytes": "INTEGER DEFAULT 0"}, "sub_admins": {"sender_whitelist": "TEXT", "max_retention_days": "INTEGER DEFAULT 30"},
	}
	for table, fields := range migrations {
		cols, e := query(tx, "PRAGMA table_info("+table+")")
		if e != nil {
			return e
		}
		existing := map[string]bool{}
		for _, col := range cols {
			existing[str(col["name"])] = true
		}
		for field, typ := range fields {
			if !existing[field] {
				if _, err = tx.Exec("ALTER TABLE " + table + " ADD COLUMN " + field + " " + typ); err != nil {
					return err
				}
			}
		}
	}
	missing, err := query(tx, "SELECT id FROM mailboxes WHERE mailbox_key IS NULL OR TRIM(mailbox_key) = ''")
	if err != nil {
		return err
	}
	for _, m := range missing {
		if _, err = tx.Exec("UPDATE mailboxes SET mailbox_key=? WHERE id=?", mailboxKey(), m["id"]); err != nil {
			return err
		}
	}
	for _, stmt := range []string{"CREATE INDEX IF NOT EXISTS idx_mailboxes_access_token ON mailboxes(access_token)", "CREATE INDEX IF NOT EXISTS idx_emails_mailbox ON emails(mailbox_id)", "CREATE INDEX IF NOT EXISTS idx_mailboxes_expires ON mailboxes(expires_at)", "CREATE INDEX IF NOT EXISTS idx_emails_timestamp ON emails(timestamp)"} {
		if _, err = tx.Exec(stmt); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("INSERT OR IGNORE INTO settings(key,value,updated_at) VALUES('domains',?,?)", jsonText(s.Config.Domains), time.Now().Unix()); err != nil {
		return err
	}
	// 旧表可能没有记录邮件大小，用 UTF-8 字节数补齐，避免升级后绕过容量限制。
	if _, err = tx.Exec(`UPDATE emails SET size_bytes=length(CAST(COALESCE(from_address,'')||COALESCE(to_address,'')||COALESCE(subject,'')||COALESCE(body,'')||COALESCE(sent_formatted,'') AS BLOB)) WHERE size_bytes IS NULL OR size_bytes=0`); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE mailboxes SET storage_used=COALESCE((SELECT SUM(size_bytes) FROM emails WHERE mailbox_id=mailboxes.id),0)"); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Domains() ([]string, error) {
	m, e := one(s.DB, "SELECT value FROM settings WHERE key='domains'")
	if e != nil {
		return nil, e
	}
	return NormalizeDomains(stringsValue(m["value"]))
}
func (s *Store) SaveDomains(domains []string, ip string) error {
	d, e := NormalizeDomains(domains)
	if e != nil {
		return e
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("UPDATE settings SET value=?,updated_at=? WHERE key='domains'", jsonText(d), time.Now().Unix()); e != nil {
		return e
	}
	if e = audit(tx, "UPDATE_DOMAINS", nil, Object{"domains": d}, ip); e != nil {
		return e
	}
	return tx.Commit()
}
func contains(items []string, item string) bool {
	for _, v := range items {
		if strings.EqualFold(v, item) {
			return true
		}
	}
	return false
}
func audit(q queryer, action string, id any, changes Object, ip string) error {
	_, e := q.Exec("INSERT INTO audit_logs(id,timestamp,action,mailbox_id,admin_user,changes,ip_address) VALUES(?,?,?,?,?,?,?)", token(), time.Now().Unix(), action, id, "admin", jsonText(changes), ip)
	return e
}

const mailboxSelect = `SELECT m.*, (SELECT COUNT(*) FROM emails WHERE mailbox_id=m.id) AS email_count, (SELECT COUNT(*) FROM emails WHERE mailbox_id=m.id AND is_read=0) AS unread_count, (SELECT MAX(timestamp) FROM emails WHERE mailbox_id=m.id) AS last_email_time FROM mailboxes m`

func normalizeMailbox(m Object) Object {
	for _, k := range []string{"is_active", "whitelist_enabled"} {
		m[k] = truth(m[k])
	}
	for _, k := range []string{"sender_whitelist", "allowed_domains"} {
		m[k] = stringsValue(m[k])
	}
	m["is_expired"] = num(m["expires_at"]) <= time.Now().Unix()
	used, limit := float64(num(m["storage_used"])), float64(num(m["storage_limit"]))
	m["storage_used_mb"] = used / 1048576
	m["storage_limit_mb"] = limit / 1048576
	m["storage_percent"] = float64(0)
	if limit > 0 {
		m["storage_percent"] = used / limit * 100
	}
	return m
}
func (s *Store) Mailbox(field, value string) (Object, error) {
	if field != "id" && field != "address" && field != "access_token" {
		return nil, fmt.Errorf("invalid mailbox lookup")
	}
	m, e := one(s.DB, mailboxSelect+" WHERE m."+field+"=?", value)
	if e != nil {
		return nil, e
	}
	return normalizeMailbox(m), nil
}
func publicMailbox(m Object) Object {
	out := Object{}
	for k, v := range m {
		if k != "access_token" && k != "mailbox_key" {
			out[k] = v
		}
	}
	return out
}
func (s *Store) CreateMailbox(address string, days int, whitelist, allowed []string, ip, source string) (Object, error) {
	now := time.Now().Unix()
	id := token()
	key := mailboxKey()
	access := token()
	tx, e := s.DB.Begin()
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	// 域名判断与写入使用同一事务，配置更新后不会再创建已移除域名。
	setting, e := one(tx, "SELECT value FROM settings WHERE key='domains'")
	if e != nil {
		return nil, e
	}
	parts := strings.Split(address, "@")
	if len(parts) != 2 || !contains(stringsValue(setting["value"]), parts[1]) {
		return nil, bad("邮箱域名不在可用列表中")
	}
	_, e = tx.Exec(`INSERT INTO mailboxes(id,address,created_at,expires_at,retention_days,is_active,sender_whitelist,whitelist_enabled,created_by_ip,access_token,mailbox_key,last_accessed,created_source,allowed_domains,storage_used,storage_limit) VALUES(?,?,?,?,?,1,?,?,?,?,?,?,?,?,0,?)`, id, address, now, now+int64(days)*86400, days, jsonText(whitelist), len(whitelist) > 0, ip, access, key, now, source, jsonText(allowed), s.Config.MaxMailboxBytes)
	if e != nil {
		if strings.Contains(e.Error(), "UNIQUE") {
			return nil, &HTTPError{409, "邮箱地址已存在"}
		}
		return nil, e
	}
	if e = audit(tx, "CREATE", id, Object{"address": address, "retention_days": days}, ip); e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return s.Mailbox("id", id)
}
func (s *Store) UpdateMailbox(id string, fields Object, action, ip string) error {
	allowed := map[string]bool{"retention_days": true, "expires_at": true, "sender_whitelist": true, "whitelist_enabled": true, "is_active": true, "allowed_domains": true, "access_token": true, "mailbox_key": true}
	parts := []string{}
	args := []any{}
	changes := Object{}
	for k, v := range fields {
		if !allowed[k] {
			return bad("未知邮箱字段")
		}
		parts = append(parts, k+"=?")
		if k == "sender_whitelist" || k == "allowed_domains" {
			v = jsonText(v)
		}
		args = append(args, v)
		if k != "access_token" && k != "mailbox_key" {
			changes[k] = v
		}
	}
	if len(parts) == 0 {
		return bad("没有可更新的字段")
	}
	parts = append(parts, "updated_at=?", "updated_by_admin=?")
	args = append(args, time.Now().Unix(), "admin", id)
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	res, e := tx.Exec("UPDATE mailboxes SET "+strings.Join(parts, ",")+" WHERE id=?", args...)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil {
		return e
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	if e = audit(tx, action, id, changes, ip); e != nil {
		return e
	}
	return tx.Commit()
}
func deleteMailbox(q queryer, id string) error {
	for _, stmt := range []string{"DELETE FROM emails WHERE mailbox_id=?", "DELETE FROM users_mailboxes WHERE mailbox_id=?", "UPDATE audit_logs SET mailbox_id=NULL WHERE mailbox_id=?", "DELETE FROM mailboxes WHERE id=?"} {
		if _, e := q.Exec(stmt, id); e != nil {
			if strings.Contains(e.Error(), "no such table: users_mailboxes") {
				continue
			}
			return e
		}
	}
	return nil
}
func (s *Store) DeleteMailbox(id string, soft bool, ip string) error {
	if soft {
		return s.UpdateMailbox(id, Object{"is_active": false}, "DELETE", ip)
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = one(tx, "SELECT id FROM mailboxes WHERE id=?", id); e != nil {
		return e
	}
	if e = deleteMailbox(tx, id); e != nil {
		return e
	}
	if e = audit(tx, "DELETE", nil, Object{"deleted_mailbox_id": id}, ip); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) Cleanup(now time.Time) error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	rows, e := query(tx, "SELECT id FROM mailboxes WHERE expires_at<?", now.Unix())
	if e != nil {
		return e
	}
	for _, m := range rows {
		if e = deleteMailbox(tx, str(m["id"])); e != nil {
			return e
		}
	}
	if _, e = tx.Exec("DELETE FROM emails WHERE timestamp<?", now.Unix()-int64(s.Config.EmailRetentionSeconds)); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE mailboxes SET storage_used=COALESCE((SELECT SUM(size_bytes) FROM emails WHERE mailbox_id=mailboxes.id),0)"); e != nil {
		return e
	}
	return tx.Commit()
}

const emailSelect = `SELECT id,mailbox_id,from_address AS "From",to_address AS "To",subject AS "Subject",body AS "Body",content_type AS "ContentType",timestamp AS "Timestamp",sent_formatted AS "Sent",is_read FROM emails`

func (s *Store) Emails(id string) ([]Object, error) {
	rows, e := query(s.DB, emailSelect+" WHERE mailbox_id=? ORDER BY timestamp DESC,id", id)
	for _, m := range rows {
		m["is_read"] = truth(m["is_read"])
		delete(m, "mailbox_id")
	}
	return rows, e
}
func (s *Store) Email(id, mailboxID string) (Object, error) {
	m, e := one(s.DB, emailSelect+" WHERE id=?", id)
	if e != nil {
		return nil, e
	}
	if m["mailbox_id"] != mailboxID {
		return nil, &HTTPError{403, "邮件不属于当前邮箱"}
	}
	m["is_read"] = truth(m["is_read"])
	return m, nil
}
func (s *Store) DeleteEmails(ids []string, mailboxID string) (int, error) {
	tx, e := s.DB.Begin()
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	for _, id := range ids {
		m, e := one(tx, "SELECT mailbox_id FROM emails WHERE id=?", id)
		if e != nil {
			return 0, e
		}
		if m["mailbox_id"] != mailboxID {
			return 0, &HTTPError{403, "邮件不属于当前邮箱"}
		}
	}
	total := 0
	for _, id := range ids {
		r, e := tx.Exec("DELETE FROM emails WHERE id=? AND mailbox_id=?", id, mailboxID)
		if e != nil {
			return 0, e
		}
		n, e := r.RowsAffected()
		if e != nil {
			return 0, e
		}
		total += int(n)
	}
	if _, e = tx.Exec("UPDATE mailboxes SET storage_used=COALESCE((SELECT SUM(size_bytes) FROM emails WHERE mailbox_id=?),0) WHERE id=?", mailboxID, mailboxID); e != nil {
		return 0, e
	}
	return total, tx.Commit()
}
