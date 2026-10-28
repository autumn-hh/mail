package store

import (
	"database/sql"
	"maildrop/internal/config"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestImportJSONPreservesCredentialsAndRollsBack(t *testing.T) {
	s := testStore(t)
	path := filepath.Join(t.TempDir(), "inbox.json")
	raw := `{"old@example.test":{"id":"legacy-id","access_token":"legacy-token","mailbox_key":"Legacy1234","created_at":1700000000,"expires_at":2000000000,"is_active":false,"emails":[{"id":"old-email","From":"sender@remote.test","Subject":"中文","Body":"你好","Timestamp":1700000001,"is_read":true}]},"list@example.test":[{"id":"list-email","Body":"list body"}]}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ImportJSON(path); err != nil || n != 2 {
		t.Fatalf("import: %d %v", n, err)
	}
	m, err := s.GetMailboxByAddress("old@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "legacy-id" || m.AccessToken != "legacy-token" || m.MailboxKey != "Legacy1234" || m.IsActive || m.CreatedAt != 1700000000 || m.ExpiresAt != 2000000000 {
		t.Fatalf("credentials/state changed: id=%s active=%v", m.ID, m.IsActive)
	}
	var body string
	var read bool
	var size int64
	if err := s.DB().QueryRow("SELECT body,is_read,size_bytes FROM emails WHERE id='old-email'").Scan(&body, &read, &size); err != nil {
		t.Fatal(err)
	}
	if body != "你好" || !read || size != m.StorageUsed || size <= int64(len(body)) {
		t.Fatalf("mail/storage mismatch: %q %v %d", body, read, size)
	}
	generated, err := s.GetMailboxByAddress("list@example.test")
	if err != nil || len(generated.AccessToken) != 43 || len(generated.MailboxKey) != 10 {
		t.Fatalf("generated credentials: %v", err)
	}
	if n, err := s.ImportJSON(path); err != nil || n != 0 {
		t.Fatalf("repeat import: %d %v", n, err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != raw {
		t.Fatal("source changed")
	}
	bad := `{"a@example.test":[{"id":"new-email","Body":"one"}],"z@example.test":[{"id":"old-email","Body":"duplicate"}]}`
	if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ImportJSON(path); err == nil || n != 0 {
		t.Fatalf("expected atomic failure: %d %v", n, err)
	}
	var count int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM mailboxes WHERE address IN ('a@example.test','z@example.test')").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial commit: %d %v", count, err)
	}
}

func TestLocalGoSettingsMigrationAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mailbox.db")
	cfg := config.FromEnv(func(string) string { return "" })
	s, err := OpenWithConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, s.DB(), "CREATE TABLE settings(key TEXT PRIMARY KEY,value TEXT NOT NULL,updated_at INTEGER NOT NULL)")
	execSQL(t, s.DB(), `INSERT INTO settings VALUES('domains','["Saved.TEST","second.test"]',0)`)
	m := mustMailbox(t, s, "old@saved.test")
	execSQL(t, s.DB(), "INSERT INTO emails(id,mailbox_id,from_address,to_address,body,timestamp,size_bytes) VALUES('historic',?,'sender@remote.test',?,'你好',?,0)", m.ID, m.Address, time.Now().Unix())
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenWithConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SeedDomainsIfEmpty([]string{"env.test"}); err != nil {
		t.Fatal(err)
	}
	domains, err := s.ListDomains()
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, domain := range domains {
		names = append(names, domain.Domain)
	}
	if !reflect.DeepEqual(names, []string{"saved.test", "second.test"}) {
		t.Fatalf("domains lost: %v", names)
	}
	migrated, err := s.GetMailboxByAddress(m.Address)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.AccessToken != m.AccessToken || migrated.MailboxKey != m.MailboxKey || migrated.StorageUsed <= 0 {
		t.Fatal("credentials/storage migration failed")
	}
	if err := s.DeleteDomain("saved.test"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenWithConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	domains, err = s.ListDomains()
	if err != nil || len(domains) != 1 || domains[0].Domain != "second.test" {
		t.Fatalf("deleted domain resurrected: %v %v", domains, err)
	}
}

func TestInvalidLocalDomainsRollbackMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mailbox.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, "CREATE TABLE settings(key TEXT PRIMARY KEY,value TEXT NOT NULL)")
	execSQL(t, db, `INSERT INTO settings VALUES('domains','["valid.test","https://bad.test"]')`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("invalid domains accepted")
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='domains'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("migration partially committed: %d %v", count, err)
	}
}
