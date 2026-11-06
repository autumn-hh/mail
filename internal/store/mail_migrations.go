package store

import "database/sql"

// 新能力只增加表和可空字段；原 Python 列定义继续保持兼容。
func migrateMailFeatures(tx *sql.Tx) error {
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY)`,
		`CREATE TABLE IF NOT EXISTS mail_content (id TEXT PRIMARY KEY, raw BLOB NOT NULL, details TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS sending_policies (
          mailbox_id TEXT PRIMARY KEY REFERENCES mailboxes(id) ON DELETE CASCADE,
          enabled INTEGER NOT NULL DEFAULT 0, daily_limit INTEGER NOT NULL DEFAULT 100,
          minute_limit INTEGER NOT NULL DEFAULT 10)`,
		`CREATE TABLE IF NOT EXISTS outbound_messages (
          id TEXT PRIMARY KEY, mailbox_id TEXT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
          created_at INTEGER NOT NULL, details TEXT NOT NULL, raw BLOB NOT NULL, size_bytes INTEGER NOT NULL,
          message_id TEXT NOT NULL, bounce_address TEXT UNIQUE NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS outbound_recipients (
          message_id TEXT NOT NULL REFERENCES outbound_messages(id) ON DELETE CASCADE,
          address TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'queued', attempts INTEGER NOT NULL DEFAULT 0,
          next_attempt INTEGER NOT NULL, lease_until INTEGER NOT NULL DEFAULT 0,
          last_error TEXT NOT NULL DEFAULT '', uncertain INTEGER NOT NULL DEFAULT 0, cancelled INTEGER NOT NULL DEFAULT 0,
          PRIMARY KEY(message_id,address))`,
		`CREATE INDEX IF NOT EXISTS idx_outbound_due ON outbound_recipients(status,next_attempt,lease_until)`,
		`CREATE TABLE IF NOT EXISTS outbound_requests (
          mailbox_id TEXT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE, request_key TEXT NOT NULL,
          request_hash TEXT NOT NULL, message_id TEXT NOT NULL, created_at INTEGER NOT NULL,
          recipient_count INTEGER NOT NULL, PRIMARY KEY(mailbox_id,request_key))`,
		`CREATE INDEX IF NOT EXISTS idx_outbound_quota ON outbound_requests(mailbox_id,created_at)`,
		`CREATE TABLE IF NOT EXISTS outbound_bounces (
          id TEXT PRIMARY KEY, message_id TEXT NOT NULL REFERENCES outbound_messages(id) ON DELETE CASCADE,
          received_at INTEGER NOT NULL, raw BLOB NOT NULL, summary TEXT NOT NULL)`,
	} {
		if _, err := tx.Exec(ddl); err != nil {
			return err
		}
	}
	outboundColumns, err := tableColumns(tx, "outbound_recipients")
	if err != nil {
		return err
	}
	if !outboundColumns["cancelled"] {
		if _, err = tx.Exec(`ALTER TABLE outbound_recipients ADD COLUMN cancelled INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	columns, err := tableColumns(tx, "emails")
	if err != nil {
		return err
	}
	if !columns["content_id"] {
		if _, err := tx.Exec(`ALTER TABLE emails ADD COLUMN content_id TEXT REFERENCES mail_content(id)`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`CREATE TRIGGER IF NOT EXISTS delete_mail_content AFTER DELETE ON emails
      WHEN OLD.content_id IS NOT NULL BEGIN
        DELETE FROM mail_content WHERE id=OLD.content_id AND NOT EXISTS (SELECT 1 FROM emails WHERE content_id=OLD.content_id);
      END`); err != nil {
		return err
	}
	var applied int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name='mail-features-v1'`).Scan(&applied); err != nil {
		return err
	}
	if applied == 0 {
		// 一次性升级历史默认容量；50 MiB 等自定义值不再被旧启动迁移覆盖。
		if _, err := tx.Exec(`UPDATE mailboxes SET storage_limit=104857600 WHERE storage_limit=3145728 OR storage_limit IS NULL OR storage_limit=0`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(name) VALUES ('mail-features-v1')`); err != nil {
			return err
		}
	}
	return nil
}
