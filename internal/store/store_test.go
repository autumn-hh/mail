package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"maildrop/internal/config"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenWithConfig(filepath.Join(t.TempDir(), "nested", "mailbox.db"), config.FromEnv(func(string) string { return "" }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func mustMailbox(t *testing.T, s *Store, address string) *Mailbox {
	t.Helper()
	m, err := s.CreateMailbox(address, CreateMailboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func execSQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}
func jsonObject(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestMailboxLifecycleAndJSON(t *testing.T) {
	s := testStore(t)
	now := time.Unix(1800000000, 0)
	s.now = func() time.Time { return now }
	days := 2
	m, err := s.CreateMailbox("box@example.test", CreateMailboxOptions{RetentionDays: &days, SenderWhitelist: []string{"@ok.test", "one@other.test"}, CreatedSource: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.AccessToken) != 43 || m.ExpiresAt != now.Unix()+172800 {
		t.Fatal("create contract")
	}
	validateKey(t, m.MailboxKey)
	created := jsonObject(t, m)
	if len(created) != 11 || created["created_source"] != "admin" || created["is_active"] != true {
		t.Fatalf("create JSON: %v", created)
	}
	for _, getter := range []func() (*Mailbox, error){func() (*Mailbox, error) { return s.GetMailboxByID(m.ID) }, func() (*Mailbox, error) { return s.GetMailboxByAddress(m.Address) }, func() (*Mailbox, error) { return s.GetMailboxByToken(m.AccessToken) }} {
		got, err := getter()
		if err != nil {
			t.Fatal(err)
		}
		value := jsonObject(t, got)
		if len(value) != 14 || value["created_by_ip"] != nil || value["whitelist_enabled"] != true || value["storage_limit"] != float64(3145728) {
			t.Fatalf("lookup JSON: %v", value)
		}
		if got.ID != m.ID || len(got.SenderWhitelist) != 2 || got.CreatedSource != "admin" {
			t.Fatalf("lookup: %+v", got)
		}
	}
	if _, err := s.GetMailboxByID("absent"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.CreateMailbox(m.Address, CreateMailboxOptions{}); err == nil {
		t.Fatal("duplicate address")
	}
	// 发件人白名单规则只在 internal/inbox 中实现与测试，这里不再重复。
	now = time.Unix(m.ExpiresAt, 0)
	if s.IsMailboxExpired(m) {
		t.Fatal("equal expiry must remain valid")
	}
	now = now.Add(time.Second)
	if !s.IsMailboxExpired(m) {
		t.Fatal("expired")
	}
	if err := s.UpdateMailboxAccess(m.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetMailboxByID(m.ID)
	if got.LastAccessed == nil || *got.LastAccessed != now.Unix() {
		t.Fatal("access")
	}
	if ok, err := s.UpdateMailboxRetention(m.Address, 3); !ok || err != nil {
		t.Fatal(ok, err)
	}
	got, _ = s.GetMailboxByID(m.ID)
	if got.ExpiresAt != now.Unix()+3*86400 {
		t.Fatal("retention")
	}
	rotated, err := s.RegenerateMailboxKey(m.Address, m.MailboxKey)
	if err != nil {
		t.Fatal(err)
	}
	validateKey(t, rotated)
	if _, err := s.RegenerateMailboxKey(m.Address, m.MailboxKey); !errors.Is(err, ErrNotFound) {
		t.Fatal("old key accepted", err)
	}
	if ok, err := s.AddSenderToWhitelist(m.Address, "new@test.com"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, err := s.AddSenderToWhitelist(m.Address, "new@test.com"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	got, _ = s.GetMailboxByID(m.ID)
	if len(got.SenderWhitelist) != 3 {
		t.Fatal(got.SenderWhitelist)
	}
	if ok, err := s.RemoveSenderFromWhitelist(m.Address, "new@test.com"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, err := s.RemoveSenderFromWhitelist("absent", "x"); ok || err != nil {
		t.Fatal(ok, err)
	}
}
func validateKey(t *testing.T, key string) {
	t.Helper()
	upper, lower, digit := false, false, false
	if len(key) != 10 {
		t.Fatalf("key length %d", len(key))
	}
	for _, ch := range key {
		switch {
		case ch >= 'A' && ch <= 'Z':
			upper = true
		case ch >= 'a' && ch <= 'z':
			lower = true
		case ch >= '0' && ch <= '9':
			digit = true
		default:
			t.Fatal("invalid key alphabet")
		}
	}
	if !upper || !lower || !digit {
		t.Fatal("key lacks character class")
	}
}

func TestEmailLifecycleLimitsAndCleanup(t *testing.T) {
	s := testStore(t)
	now := time.Unix(1800000000, 0)
	s.now = func() time.Time { return now }
	m := mustMailbox(t, s, "mail@test.com")
	e := Email{From: "发@x.test", To: m.Address, Subject: "你好", Body: "🙂", Sent: "today", Timestamp: now.Unix()}
	wantSize := int64(len([]byte(e.From + e.To + e.Subject + e.Body + e.Sent)))
	if CalculateEmailSize(e) != wantSize {
		t.Fatal("UTF-8 size")
	}
	id, err := s.AddEmail(m.ID, e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetEmailByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.SizeBytes != wantSize || got.ContentType != "Text" || got.IsRead || got.MailboxID != m.ID {
		t.Fatalf("email: %+v", got)
	}
	if len(jsonObject(t, got)) != 10 {
		t.Fatal("detail keys")
	}
	for i := 1; i <= 3; i++ {
		e.Timestamp++
		if _, err := s.AddEmail(m.ID, e); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.GetEmailsByMailbox(m.ID, 2)
	if err != nil || len(list) != 2 {
		t.Fatal(len(list), err)
	}
	if list[0].Timestamp < list[1].Timestamp || len(jsonObject(t, list[0])) != 9 {
		t.Fatal("list contract")
	}
	for _, limit := range []int{0, -1} {
		list, err = s.GetEmailsByMailbox(m.ID, limit)
		if err != nil || len(list) != 4 {
			t.Fatal(limit, len(list), err)
		}
	}
	stats, err := s.GetMailboxStats(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalEmails != 4 || stats.UnreadEmails != 4 || stats.StorageUsed != wantSize*4 || stats.LastEmailTime == nil {
		t.Fatalf("stats: %+v", stats)
	}
	if err := s.MarkEmailAsRead(id); err != nil {
		t.Fatal(err)
	}
	if count, err := s.MarkAllEmailsRead(m.ID); err != nil || count != 3 {
		t.Fatal(count, err)
	}
	if err := s.MarkEmailAsUnread(id); err != nil {
		t.Fatal(err)
	}
	if count, err := s.DeleteEmail(id); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if count, err := s.DeleteEmail(id); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	stats, _ = s.GetMailboxStats(m.ID)
	if stats.StorageUsed != wantSize*3 {
		t.Fatal("delete accounting")
	}
	e.ID = "duplicate"
	if _, err := s.AddEmail(m.ID, e); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetMailboxStats(m.ID)
	if _, err := s.AddEmail(m.ID, e); err == nil {
		t.Fatal("duplicate email accepted")
	}
	after, _ := s.GetMailboxStats(m.ID)
	if before.StorageUsed != after.StorageUsed {
		t.Fatal("failed insert changed storage")
	}
	e.ID = ""
	e.Body = strings.Repeat("a", int(s.cfg.MaxEmailSizeBytes))
	if _, err := s.AddEmail(m.ID, e); err == nil {
		t.Fatal("size limit")
	}
	e.Body = ""
	execSQL(t, s.DB(), "UPDATE mailboxes SET storage_limit = storage_used WHERE id = ?", m.ID)
	if _, err := s.AddEmail(m.ID, e); err == nil {
		t.Fatal("mailbox capacity limit")
	}
	if _, err := s.AddEmail("absent", e); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	execSQL(t, s.DB(), "UPDATE mailboxes SET storage_limit = 3145728 WHERE id = ?", m.ID)
	now = now.Add(8 * 24 * time.Hour)
	if count, err := s.CleanOldEmails(); err != nil || count != 4 {
		t.Fatal(count, err)
	}
	stats, _ = s.GetMailboxStats(m.ID)
	if stats.StorageUsed != 0 || stats.TotalEmails != 0 || stats.LastEmailTime != nil {
		t.Fatalf("cleanup: %+v", stats)
	}
	e.Body = "old"
	e.Timestamp = now.Unix()
	if _, err := s.AddEmail(m.ID, e); err != nil {
		t.Fatal(err)
	}
	execSQL(t, s.DB(), "INSERT INTO audit_logs (id,timestamp,action,mailbox_id) VALUES ('audit',?,'CREATE',?)", now.Unix(), m.ID)
	if count, err := s.CleanExpiredMailboxes(); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	var auditMailbox *string
	if err := s.DB().QueryRow("SELECT mailbox_id FROM audit_logs WHERE id='audit'").Scan(&auditMailbox); err != nil || auditMailbox != nil {
		t.Fatal("audit retention", err)
	}
}

func TestWithTxRollback(t *testing.T) {
	s := testStore(t)
	failure := errors.New("rollback")
	err := s.WithTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO domains VALUES ('a.test',1,1,1)")
		if err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	domains, _ := s.ListDomains()
	if len(domains) != 0 {
		t.Fatal("rollback failed")
	}
}

func TestConcurrentWrites(t *testing.T) {
	s := testStore(t)
	m := mustMailbox(t, s, "concurrent@test.com")
	// 两个独立连接池并发，覆盖跨 Store 的 IMMEDIATE/busy_timeout 行为。
	var dbPath string
	if err := s.DB().QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&dbPath); err != nil {
		t.Fatal(err)
	}
	other, err := OpenWithConfig(dbPath, &s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 128)
	for i := range 64 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			current := s
			if i%2 == 1 {
				current = other
			}
			_, err := current.AddEmail(m.ID, Email{From: "a@b.test", To: m.Address, Body: "hello", Timestamp: int64(i)})
			if err != nil {
				errs <- err
				return
			}
			_, err = current.AddSenderToWhitelist(m.Address, fmt.Sprintf("%d@test.com", i))
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	stats, err := s.GetMailboxStats(m.ID)
	if err != nil || stats.TotalEmails != 64 {
		t.Fatal(stats, err)
	}
	mailbox, err := s.GetMailboxByID(m.ID)
	if err != nil || len(mailbox.SenderWhitelist) != 64 {
		t.Fatal("lost whitelist writes", err)
	}
	var used int64
	if err := s.DB().QueryRow("SELECT COALESCE(SUM(size_bytes),0) FROM emails WHERE mailbox_id=?", m.ID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != stats.StorageUsed {
		t.Fatal("lost storage updates")
	}
}

func copyFile(t *testing.T, source, destination string) {
	t.Helper()
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.Create(destination)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		t.Fatal(copyErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}
func fileHash(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

// goOnlyColumns 列出 Go 版在 Python 结构之外追加的字段（表名.字段名）。
var goOnlyColumns = map[string]bool{"mailboxes.admin_disabled": true}

// 新库与旧库迁移后都必须带上 Go 版扩展字段，且位于表末尾、默认值为 0。
func TestGoOnlyColumns(t *testing.T) {
	s := testStore(t)
	var cid int
	var name, kind string
	var notNull int
	var defaultValue sql.NullString
	var pk int
	rows, err := s.DB().Query("PRAGMA table_info(mailboxes)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	last := ""
	found := false
	for rows.Next() {
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		last = name
		if name == "admin_disabled" {
			found = notNull == 1 && defaultValue.String == "0"
		}
	}
	if !found || last != "admin_disabled" {
		t.Fatalf("admin_disabled missing, not last, or wrong definition (last=%s)", last)
	}
}

// 使用 PRAGMA 的语义结构对照，忽略 CREATE SQL 的空白和 IF NOT EXISTS。
func schemaSnapshot(t *testing.T, db *sql.DB) map[string]any {
	t.Helper()
	snapshot := map[string]any{}
	rows, err := db.Query("SELECT type,name,tbl_name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' AND name != 'domains' ORDER BY type,name")
	if err != nil {
		t.Fatal(err)
	}
	type object struct{ kind, name, table string }
	objects := []object{}
	for rows.Next() {
		var o object
		if err := rows.Scan(&o.kind, &o.name, &o.table); err != nil {
			t.Fatal(err)
		}
		objects = append(objects, o)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	readRows := func(query string) [][]any {
		r, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		columns, _ := r.Columns()
		out := [][]any{}
		for r.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := r.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			out = append(out, values)
		}
		if err := r.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, o := range objects {
		if o.kind == "table" {
			columns := readRows("PRAGMA table_info(" + o.name + ")")
			// Go 版扩展字段追加在末尾，不参与与 Python 基准的对比（单独由 TestGoOnlyColumns 覆盖）。
			kept := columns[:0]
			for _, column := range columns {
				if name, _ := column[1].(string); !goOnlyColumns[o.name+"."+name] {
					kept = append(kept, column)
				}
			}
			snapshot[o.name] = []any{kept, readRows("PRAGMA foreign_key_list(" + o.name + ")"), readRows("PRAGMA index_list(" + o.name + ")")}
		}
		if o.kind == "index" {
			snapshot[o.name] = readRows("PRAGMA index_info(" + o.name + ")")
		}
	}
	return snapshot
}

// normalizeSnapshot 经 JSON 往返统一数值与切片类型，便于和基准文件比较。
func normalizeSnapshot(t *testing.T, snapshot map[string]any) any {
	t.Helper()
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		t.Fatal(err)
	}
	return normalized
}

// TestMigrationsAgainstPython 对比 Go 迁移结果与 Python 版 init_tables 生成的表结构。
// 基准 testdata/python_schema_<mode>.json 在移除 Python 实现前由其迁移真实生成，
// 是旧版部署升级兼容性的唯一依据，不要手工修改；表结构有意变更时需同步更新并在提交中说明。
// existing-copy 依赖本机数据文件，只校验迁移可用、完整性与幂等。
func TestMigrationsAgainstPython(t *testing.T) {
	for _, mode := range []string{"fresh", "old-password", "old-no-password", "existing-copy"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			goPath := filepath.Join(dir, "go.db")
			var originalHash [32]byte
			if mode == "existing-copy" {
				source := filepath.Join("..", "..", "data", "mailbox.db")
				if _, err := os.Stat(source); os.IsNotExist(err) {
					t.Skip("no local mailbox.db")
				}
				originalHash = fileHash(t, source)
				copyFile(t, source, goPath)
				defer func() {
					if fileHash(t, source) != originalHash {
						t.Error("source mailbox.db modified")
					}
				}()
			}
			if strings.HasPrefix(mode, "old-") {
				db, err := sql.Open("sqlite", goPath)
				if err != nil {
					t.Fatal(err)
				}
				execSQL(t, db, `CREATE TABLE mailboxes (id TEXT PRIMARY KEY,address TEXT UNIQUE NOT NULL,created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,retention_days INTEGER DEFAULT 7,is_active BOOLEAN DEFAULT 1,sender_whitelist TEXT DEFAULT '[]',created_by_ip TEXT,access_token TEXT,last_accessed INTEGER)`)
				execSQL(t, db, `INSERT INTO mailboxes (id,address,created_at,expires_at) VALUES ('legacy','legacy@test.com',1,2)`)
				execSQL(t, db, baseDDL()[3])
				password := ""
				if mode == "old-password" {
					password = "password_hash TEXT,"
				}
				execSQL(t, db, `CREATE TABLE sub_admins (id TEXT PRIMARY KEY,token TEXT UNIQUE NOT NULL,domains TEXT NOT NULL,`+password+`created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,is_active INTEGER DEFAULT 1,created_by TEXT,notes TEXT)`)
				execSQL(t, db, `INSERT INTO sub_admins (id,token,domains,created_at,updated_at) VALUES ('old','legacy-token','a.test,b.test',1,2)`)
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			golden := filepath.Join("testdata", "python_schema_"+mode+".json")
			s, err := Open(goPath)
			if err != nil {
				t.Fatal(err)
			}
			got := normalizeSnapshot(t, schemaSnapshot(t, s.DB()))
			want := got
			if mode != "existing-copy" {
				encoded, err := os.ReadFile(golden)
				if err != nil {
					t.Fatal(err)
				}
				want = nil
				if err := json.Unmarshal(encoded, &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					a, _ := json.MarshalIndent(got, "", " ")
					t.Errorf("schema mismatch with %s\nGo: %s", golden, a)
				}
			}
			if strings.HasPrefix(mode, "old-") {
				m, err := s.GetMailboxByID("legacy")
				if err != nil {
					t.Fatal(err)
				}
				validateKey(t, m.MailboxKey)
				a, err := s.GetSubAdminByToken("legacy-token")
				if err != nil || a.ID != "old" || a.MaxRetentionDays != 30 {
					t.Fatal(a, err)
				}
			}
			var check string
			if err := s.DB().QueryRow("PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
				t.Fatal(check, err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(goPath)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if !reflect.DeepEqual(normalizeSnapshot(t, schemaSnapshot(t, s.DB())), want) {
				t.Fatal("non-idempotent migration")
			}
		})
	}
}

func TestNullableEmailAndMailboxJSON(t *testing.T) {
	s := testStore(t)
	m := mustMailbox(t, s, "nullable@test.com")
	execSQL(t, s.DB(), "UPDATE mailboxes SET access_token = NULL, last_accessed = NULL, created_by_ip = NULL, retention_days = NULL WHERE id = ?", m.ID)
	got, err := s.GetMailboxByID(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	value := jsonObject(t, got)
	for _, key := range []string{"access_token", "last_accessed", "created_by_ip", "retention_days"} {
		if value[key] != nil {
			t.Errorf("%s: %v", key, value[key])
		}
	}
	execSQL(t, s.DB(), "INSERT INTO emails (id,mailbox_id,from_address,to_address,timestamp,subject,body,content_type,sent_formatted,is_read) VALUES ('nullable',?,'from','to',1,NULL,NULL,NULL,NULL,NULL)", m.ID)
	e, err := s.GetEmailByID("nullable")
	if err != nil {
		t.Fatal(err)
	}
	value = jsonObject(t, e)
	for _, key := range []string{"Subject", "Body", "ContentType", "Sent"} {
		if value[key] != nil {
			t.Errorf("%s: %v", key, value[key])
		}
	}
	if value["is_read"] != false {
		t.Fatal("NULL boolean must be false")
	}
}

func TestAtomicCapacityAndKeyRotation(t *testing.T) {
	s := testStore(t)
	m := mustMailbox(t, s, "capacity@test.com")
	e := Email{From: "a", To: "b", Body: "12345678", Timestamp: 1}
	execSQL(t, s.DB(), "UPDATE mailboxes SET storage_limit = 100 WHERE id = ?", m.ID)
	var wg sync.WaitGroup
	var mutex sync.Mutex
	accepted, rotated := 0, 0
	for range 32 {
		wg.Go(func() {
			_, err := s.AddEmail(m.ID, e)
			if err == nil {
				mutex.Lock()
				accepted++
				mutex.Unlock()
			} else if !strings.Contains(err.Error(), "邮箱容量不足") {
				t.Error(err)
			}
			_, err = s.RegenerateMailboxKey(m.Address, m.MailboxKey)
			if err == nil {
				mutex.Lock()
				rotated++
				mutex.Unlock()
			} else if !errors.Is(err, ErrNotFound) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	stats, err := s.GetMailboxStats(m.ID)
	if err != nil || accepted != 10 || rotated != 1 || stats.StorageUsed != 100 {
		t.Fatal(accepted, rotated, stats, err)
	}
}

func TestPragmasMemoryAndSQLFilename(t *testing.T) {
	for _, path := range []string{":memory:", filepath.Join(t.TempDir(), "空 间%#.db")} {
		s, err := OpenWithConfig(path, config.FromEnv(func(string) string { return "" }))
		if err != nil {
			t.Fatal(path, err)
		}
		var timeout, foreignKeys int
		if err := s.DB().QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil || timeout != 10000 {
			t.Fatal(timeout, err)
		}
		if err := s.DB().QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
			t.Fatal(foreignKeys, err)
		}
		if s.DB().Stats().MaxOpenConnections != 1 {
			t.Fatal("unbounded write pool")
		}
		if path != ":memory:" {
			var mode string
			if err := s.DB().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
				t.Fatal(mode, err)
			}
		}
		mustMailbox(t, s, "filename@test.com")
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrationFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, `CREATE TABLE sub_admins (id TEXT PRIMARY KEY,token TEXT UNIQUE NOT NULL,domains TEXT NOT NULL,password_hash TEXT,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,is_active INTEGER DEFAULT 1,created_by TEXT)`)
	execSQL(t, db, `INSERT INTO sub_admins VALUES ('old','token','a.test','hash',1,2,1,NULL)`)
	db.Close()
	if s, err := OpenWithConfig(path, config.FromEnv(func(string) string { return "" })); err == nil {
		s.Close()
		t.Fatal("broken schema was accepted")
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM sub_admins WHERE id='old'").Scan(&count); err != nil || count != 1 {
		t.Fatal("original admin lost", count, err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='mailboxes'").Scan(&count); err != nil || count != 0 {
		t.Fatal("partial migration committed", count, err)
	}
}

func TestMigrationQuotaAndKeyBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.db")
	cfg := config.FromEnv(func(string) string { return "" })
	s, err := OpenWithConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id, key string
		limit   any
	}{
		{"fifty", " ", 52428800}, {"null", "", nil}, {"custom", "legacy-uuid-token", 9 * 1048576},
	} {
		execSQL(t, s.DB(), "INSERT INTO mailboxes (id,address,created_at,expires_at,mailbox_key,storage_limit) VALUES (?,?,1,2,?,?)", tc.id, tc.id+"@test.com", tc.key, tc.limit)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenWithConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{}
	for _, id := range []string{"fifty", "null", "custom"} {
		m, err := s.GetMailboxByID(id)
		if err != nil {
			t.Fatal(err)
		}
		keys[id] = m.MailboxKey
		if id == "custom" {
			if m.MailboxKey != "legacy-uuid-token" || m.StorageLimit != 9*1048576 {
				t.Fatal(m)
			}
		} else {
			validateKey(t, m.MailboxKey)
			if m.StorageLimit != 3145728 {
				t.Fatal("quota migration", m.StorageLimit)
			}
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenWithConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for id, key := range keys {
		m, err := s.GetMailboxByID(id)
		if err != nil || m.MailboxKey != key {
			t.Fatal("key changed on re-open", err)
		}
	}
}
