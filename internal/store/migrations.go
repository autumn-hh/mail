package store

import (
	"context"
	"database/sql"
	"fmt"
)

const subAdminsDDL = `CREATE TABLE IF NOT EXISTS sub_admins (
 id TEXT PRIMARY KEY, token TEXT UNIQUE NOT NULL, domains TEXT NOT NULL,
 sender_whitelist TEXT, max_retention_days INTEGER DEFAULT 30,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 is_active INTEGER DEFAULT 1, created_by TEXT, notes TEXT)`

func baseDDL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS users (
 id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT UNIQUE NOT NULL, email TEXT,
 password_hash TEXT NOT NULL, created_at INTEGER NOT NULL, created_by_ip TEXT,
 is_active BOOLEAN DEFAULT 1, last_login INTEGER)`,
		`CREATE TABLE IF NOT EXISTS users_mailboxes (
 id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL, mailbox_id TEXT NOT NULL,
 created_at INTEGER NOT NULL,
 FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
 FOREIGN KEY (mailbox_id) REFERENCES mailboxes (id) ON DELETE CASCADE,
 UNIQUE(user_id, mailbox_id))`,
		`CREATE TABLE IF NOT EXISTS mailboxes (
 id TEXT PRIMARY KEY, address TEXT UNIQUE NOT NULL, created_at INTEGER NOT NULL,
 expires_at INTEGER NOT NULL, retention_days INTEGER DEFAULT 7,
 is_active BOOLEAN DEFAULT 1, sender_whitelist TEXT DEFAULT '[]',
 whitelist_enabled BOOLEAN DEFAULT 0, created_by_ip TEXT, access_token TEXT, last_accessed INTEGER)`,
		`CREATE TABLE IF NOT EXISTS emails (
 id TEXT PRIMARY KEY, mailbox_id TEXT NOT NULL, from_address TEXT NOT NULL,
 to_address TEXT NOT NULL, subject TEXT, body TEXT, content_type TEXT DEFAULT 'Text',
 timestamp INTEGER NOT NULL, sent_formatted TEXT, is_read BOOLEAN DEFAULT 0,
 FOREIGN KEY (mailbox_id) REFERENCES mailboxes (id) ON DELETE CASCADE)`,
		`CREATE TABLE IF NOT EXISTS invite_codes (
 id TEXT PRIMARY KEY, code TEXT UNIQUE NOT NULL, created_by INTEGER,
 created_at INTEGER NOT NULL, expires_at INTEGER, is_used BOOLEAN DEFAULT 0,
 used_by INTEGER, used_at INTEGER, max_uses INTEGER DEFAULT 1, current_uses INTEGER DEFAULT 0,
 FOREIGN KEY (created_by) REFERENCES users (id) ON DELETE SET NULL,
 FOREIGN KEY (used_by) REFERENCES users (id) ON DELETE SET NULL)`,
		`CREATE TABLE IF NOT EXISTS audit_logs (
 id TEXT PRIMARY KEY, timestamp INTEGER NOT NULL, action TEXT NOT NULL,
 mailbox_id TEXT, admin_user TEXT, changes TEXT, ip_address TEXT,
 FOREIGN KEY (mailbox_id) REFERENCES mailboxes (id) ON DELETE SET NULL)`,
		subAdminsDDL,
		`CREATE TABLE IF NOT EXISTS domains (
 domain TEXT PRIMARY KEY, enabled INTEGER NOT NULL DEFAULT 1,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
	}

}

func tableColumns(tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var cid, required, pk int
		var name, kind string
		var fallback sql.NullString
		if err := rows.Scan(&cid, &name, &kind, &required, &fallback, &pk); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

func (s *Store) migrate() error {
	return s.WithTx(context.Background(), func(tx *sql.Tx) error {
		for _, ddl := range baseDDL() {
			if _, err := tx.Exec(ddl); err != nil {
				return err
			}
		}
		// 保留历史 ALTER 的顺序、类型和默认值，兼容旧文件。
		for _, migration := range []struct{ table, name, definition string }{
			{"mailboxes", "mailbox_key", "TEXT"},
			{"mailboxes", "whitelist_enabled", "BOOLEAN DEFAULT 0"},
			{"mailboxes", "created_source", `TEXT DEFAULT "unknown"`},
			{"mailboxes", "updated_by_admin", "TEXT"},
			{"mailboxes", "updated_at", "INTEGER"},
			{"mailboxes", "allowed_domains", `TEXT DEFAULT "[]"`},
			{"mailboxes", "storage_used", "INTEGER DEFAULT 0"},
			{"mailboxes", "storage_limit", "INTEGER DEFAULT 3145728"},
			{"emails", "size_bytes", "INTEGER DEFAULT 0"},
			// 以下为 Go 版新增字段，只追加在末尾；Python 旧版按显式列名读写，回滚后可忽略它们。
			// admin_disabled=1 表示管理员禁用，邮箱持有者不能通过公开接口自行恢复。
			{"mailboxes", "admin_disabled", "INTEGER NOT NULL DEFAULT 0"},
		} {
			columns, err := tableColumns(tx, migration.table)
			if err != nil {
				return err
			}
			if !columns[migration.name] {
				if _, err := tx.Exec("ALTER TABLE " + migration.table + " ADD COLUMN " + migration.name + " " + migration.definition); err != nil {
					return err
				}
			}
		}
		// Python 可写入小数；仅修正数据类型，不改表结构或 NULL。
		for _, table := range []struct {
			name    string
			columns []string
		}{
			{"mailboxes", []string{"retention_days", "expires_at", "created_at", "last_accessed", "storage_used", "storage_limit", "updated_at", "is_active", "whitelist_enabled"}},
			{"emails", []string{"timestamp", "size_bytes", "is_read"}},
		} {
			for _, column := range table.columns {
				query := fmt.Sprintf("UPDATE %s SET %s = CAST(%s AS INTEGER) WHERE typeof(%s) = 'real'", table.name, column, column, column)
				if _, err := tx.Exec(query); err != nil {
					return err
				}
			}
		}
		rows, err := tx.Query(`SELECT id FROM mailboxes WHERE mailbox_key IS NULL OR TRIM(mailbox_key) = ''`)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			key, err := GenerateMailboxKey()
			if err != nil {
				return err
			}
			if _, err := tx.Exec("UPDATE mailboxes SET mailbox_key = ? WHERE id = ?", key, id); err != nil {
				return err
			}
		}
		// 与 Python 启动迁移保持一致，保留历史 50 MiB -> 3 MiB 策略。
		if _, err := tx.Exec(`UPDATE mailboxes SET storage_limit = 3145728 WHERE storage_limit = 52428800 OR storage_limit IS NULL`); err != nil {
			return err
		}
		for _, index := range []struct{ name, table, column string }{
			{"idx_mailboxes_address", "mailboxes", "address"}, {"idx_mailboxes_expires", "mailboxes", "expires_at"},
			{"idx_mailboxes_access_token", "mailboxes", "access_token"}, {"idx_emails_mailbox", "emails", "mailbox_id"},
			{"idx_emails_timestamp", "emails", "timestamp"}, {"idx_invite_codes_code", "invite_codes", "code"},
			{"idx_invite_codes_used", "invite_codes", "is_used"}, {"idx_audit_logs_mailbox", "audit_logs", "mailbox_id"},
			{"idx_audit_logs_timestamp", "audit_logs", "timestamp"},
		} {
			if _, err := tx.Exec(fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (%s)", index.name, index.table, index.column)); err != nil {
				return err
			}
		}
		columns, err := tableColumns(tx, "sub_admins")
		if err != nil {
			return err
		}
		if columns["password_hash"] {
			// 同事务内备份、重建、恢复；失败时回滚，不能吞掉迁移错误。
			for _, query := range []string{
				`CREATE TEMP TABLE sub_admins_backup AS SELECT id, token, domains, created_at, updated_at, is_active, created_by, notes FROM sub_admins`,
				`DROP TABLE sub_admins`, subAdminsDDL,
				`INSERT INTO sub_admins (id, token, domains, sender_whitelist, max_retention_days, created_at, updated_at, is_active, created_by, notes) SELECT id, token, domains, NULL, 30, created_at, updated_at, is_active, created_by, notes FROM sub_admins_backup`,
				`DROP TABLE sub_admins_backup`,
			} {
				if _, err := tx.Exec(query); err != nil {
					return err
				}
			}
		} else {
			if !columns["sender_whitelist"] {
				if _, err := tx.Exec(`ALTER TABLE sub_admins ADD COLUMN sender_whitelist TEXT`); err != nil {
					return err
				}
			}
			if !columns["max_retention_days"] {
				if _, err := tx.Exec(`ALTER TABLE sub_admins ADD COLUMN max_retention_days INTEGER DEFAULT 30`); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
