package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"maildrop/internal/config"
)

func TestMailSchemaIsIdempotentAndCascades(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.db")
	cfg := config.FromEnv(func(string) string { return "" })
	s, err := OpenWithConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	m := mustMailbox(t, s, "one@test.example")
	readSchema := func() []string {
		rows, err := s.DB().Query(`SELECT type||':'||name||':'||COALESCE(sql,'') FROM sqlite_master ORDER BY type,name`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		result := []string{}
		for rows.Next() {
			var definition string
			if err := rows.Scan(&definition); err != nil {
				t.Fatal(err)
			}
			result = append(result, definition)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := readSchema()
	execSQL(t, s.DB(), `UPDATE mailboxes SET storage_limit=3145728 WHERE id=?`, m.ID)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenWithConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, readSchema()) {
		t.Fatal("full new schema changed on reopen")
	}
	got, err := s.GetMailboxByID(m.ID)
	if err != nil || got.StorageLimit != 3145728 {
		t.Fatal("one-time migration ran again", got, err)
	}
	if err = s.WithTx(context.Background(), func(tx *sql.Tx) error {
		statements := []string{
			`INSERT INTO sending_policies(mailbox_id) VALUES ('` + m.ID + `')`,
			`INSERT INTO outbound_messages VALUES ('message','` + m.ID + `',1,'{}',X'01',1,'<message@test.example>','bounce@test.example')`,
			`INSERT INTO outbound_recipients(message_id,address,next_attempt) VALUES ('message','recipient@test.example',1)`,
			`INSERT INTO outbound_bounces VALUES ('report','message',1,X'01','report')`,
			`INSERT INTO outbound_requests VALUES ('` + m.ID + `','key','hash','message',1,1)`,
			`DELETE FROM mailboxes WHERE id='` + m.ID + `'`,
		}
		for _, query := range statements {
			if _, err := tx.Exec(query); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"sending_policies", "outbound_messages", "outbound_recipients", "outbound_bounces", "outbound_requests"} {
		var count int
		if err := s.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("cascade", table, count, err)
		}
	}
	rows, err := s.DB().Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign key corruption")
	}
}
