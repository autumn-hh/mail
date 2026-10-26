package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"reflect"
	"testing"

	"maildrop/internal/config"
)

func writeLegacyNumbers(t *testing.T, s *Store, m *Mailbox) {
	t.Helper()
	execSQL(t, s.DB(), `UPDATE mailboxes SET created_at = 1800000000.75,
 expires_at = 1800129600.75, retention_days = 1.5, last_accessed = 1800000001.75,
 storage_used = 100.75, storage_limit = 100000.75, updated_at = 1800000002.75,
 is_active = 1.75, whitelist_enabled = 0.75 WHERE id = ?`, m.ID)
	execSQL(t, s.DB(), `INSERT INTO emails (id, mailbox_id, from_address, to_address,
 timestamp, size_bytes, is_read) VALUES ('legacy-real', ?, 'sender@example.com', ?,
 1800000003.75, 17.75, 1.75)`, m.ID, m.Address)
}

func assertLegacyMailbox(t *testing.T, m *Mailbox) {
	t.Helper()
	if m.CreatedAt != 1800000000 || m.ExpiresAt != 1800129600 || m.RetentionDays != 1 ||
		m.LastAccessed == nil || *m.LastAccessed != 1800000001 ||
		m.StorageUsed != 100 || m.StorageLimit != 100000 ||
		m.UpdatedAt == nil || *m.UpdatedAt != 1800000002 || !m.IsActive || m.WhitelistEnabled {
		t.Fatalf("legacy mailbox = %+v", m)
	}
}

func TestLegacyREALMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	cfg := config.FromEnv(func(string) string { return "" })
	s, err := OpenWithConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := mustMailbox(t, s, "legacy@example.com")
	writeLegacyNumbers(t, s, m)
	nullable := mustMailbox(t, s, "nullable@example.com")
	execSQL(t, s.DB(), `UPDATE mailboxes SET retention_days = NULL, last_accessed = NULL,
 updated_at = NULL, storage_used = NULL WHERE id = ?`, nullable.ID)
	execSQL(t, s.DB(), `INSERT INTO emails (id, mailbox_id, from_address, to_address,
 timestamp, size_bytes) VALUES ('legacy-null', ?, 'sender@example.com', ?, 1, NULL)`, nullable.ID, nullable.Address)
	schema := normalizeSnapshot(t, schemaSnapshot(t, s.DB()))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for attempt := range 2 {
		s, err = OpenWithConfig(path, cfg)
		if err != nil {
			t.Fatal(err)
		}
		current := s
		t.Cleanup(func() { _ = current.Close() })
		if got := normalizeSnapshot(t, schemaSnapshot(t, s.DB())); !reflect.DeepEqual(got, schema) {
			t.Fatalf("migration %d changed the schema", attempt)
		}
		for _, tc := range []struct {
			table, id string
			columns   []string
		}{
			{"mailboxes", m.ID, []string{"created_at", "expires_at", "retention_days", "last_accessed", "storage_used", "storage_limit", "updated_at", "is_active", "whitelist_enabled"}},
			{"emails", "legacy-real", []string{"timestamp", "size_bytes", "is_read"}},
		} {
			for _, column := range tc.columns {
				var kind string
				if err := s.DB().QueryRow(fmt.Sprintf("SELECT typeof(%s) FROM %s WHERE id = ?", column, tc.table), tc.id).Scan(&kind); err != nil {
					t.Fatal(err)
				}
				if kind != "integer" {
					t.Errorf("migration %d: %s.%s type = %s", attempt, tc.table, column, kind)
				}
			}
		}
		got, err := s.GetMailboxByID(m.ID)
		if err != nil {
			t.Fatal(err)
		}
		assertLegacyMailbox(t, got)
		e, err := s.GetEmailByID("legacy-real")
		if err != nil || e.Timestamp != 1800000003 || e.SizeBytes != 17 || !e.IsRead {
			t.Fatalf("legacy email = %+v, err = %v", e, err)
		}
		got, err = s.GetMailboxByID(nullable.ID)
		if err != nil || got.LastAccessed != nil || got.UpdatedAt != nil || !got.nullRetentionDays {
			t.Fatalf("nullable mailbox = %+v, err = %v", got, err)
		}
		var used, size sql.NullInt64
		if err := s.DB().QueryRow(`SELECT storage_used, (SELECT size_bytes FROM emails WHERE id = 'legacy-null')
 FROM mailboxes WHERE id = ?`, nullable.ID).Scan(&used, &size); err != nil || used.Valid || size.Valid {
			t.Fatalf("NULL numbers changed: used = %+v, size = %+v, err = %v", used, size, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLegacyREALScanners(t *testing.T) {
	s := testStore(t)
	m := mustMailbox(t, s, "legacy@example.com")
	writeLegacyNumbers(t, s, m)
	for _, get := range []func() (*Mailbox, error){
		func() (*Mailbox, error) { return s.GetMailboxByID(m.ID) },
		func() (*Mailbox, error) { return s.GetMailboxByAddress(m.Address) },
		func() (*Mailbox, error) { return s.GetMailboxByToken(m.AccessToken) },
	} {
		got, err := get()
		if err != nil {
			t.Fatal(err)
		}
		assertLegacyMailbox(t, got)
	}
	emails, err := s.GetEmailsByMailbox(m.ID)
	if err != nil || len(emails) != 1 || emails[0].Timestamp != 1800000003 || emails[0].SizeBytes != 17 || !emails[0].IsRead {
		t.Fatalf("legacy emails = %+v, err = %v", emails, err)
	}
	stats, err := s.GetMailboxStats(m.ID)
	if err != nil || stats.LastEmailTime == nil || *stats.LastEmailTime != 1800000003 || stats.StorageUsed != 100 || stats.StorageLimit != 100000 {
		t.Fatalf("legacy stats = %+v, err = %v", stats, err)
	}
	if _, err := s.AddEmail(m.ID, Email{Body: "new", Timestamp: 1800000004}); err != nil {
		t.Fatal(err)
	}
	if count, err := s.DeleteEmail("legacy-real"); err != nil || count != 1 {
		t.Fatalf("delete legacy email = %d, err = %v", count, err)
	}
}

func TestMailboxLegacyLists(t *testing.T) {
	s := testStore(t)
	m := mustMailbox(t, s, "lists@example.com")
	for _, tc := range []struct {
		name, encoded string
		want          []string
	}{
		{"invalid", "[", []string{}},
		{"object", `{"domain":"example.com"}`, []string{}},
		{"string", `"example.com"`, []string{}},
		{"null", "null", []string{}},
		{"empty text", "", []string{}},
		{"empty array", "[]", []string{}},
		{"mixed", `["example.com",1,true,null,{},[],"sender@example.com",1e400]`, []string{"example.com", "sender@example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			execSQL(t, s.DB(), "UPDATE mailboxes SET sender_whitelist = ?, allowed_domains = ? WHERE id = ?", tc.encoded, tc.encoded, m.ID)
			got, err := s.GetMailboxByID(m.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.SenderWhitelist, tc.want) || !reflect.DeepEqual(got.AllowedDomains, tc.want) {
				t.Fatalf("lists = %v, %v, want %v", got.SenderWhitelist, got.AllowedDomains, tc.want)
			}
			var whitelist, domains string
			if err := s.DB().QueryRow("SELECT sender_whitelist, allowed_domains FROM mailboxes WHERE id = ?", m.ID).Scan(&whitelist, &domains); err != nil || whitelist != tc.encoded || domains != tc.encoded {
				t.Fatalf("read changed stored lists: %q, %q, err = %v", whitelist, domains, err)
			}
			if ok, err := s.AddSenderToWhitelist(m.Address, "new@example.com"); err != nil || !ok {
				t.Fatalf("add = %v, err = %v", ok, err)
			}
			got, err = s.GetMailboxByID(m.ID)
			want := append(append([]string{}, tc.want...), "new@example.com")
			if err != nil || !reflect.DeepEqual(got.SenderWhitelist, want) {
				t.Fatalf("updated whitelist = %+v, err = %v", got, err)
			}
		})
	}
}

func TestLegacyIntegerScannerTypes(t *testing.T) {
	s := testStore(t)
	for _, tc := range []struct {
		name  string
		value any
		want  int64
		valid bool
	}{
		{"integer", int64(42), 42, true},
		{"real", 42.75, 42, true},
		{"negative real", -42.75, -42, true},
		{"integer text", " 42 ", 42, true},
		{"real text", "42.75", 42, true},
		{"exponent text", "4.275e1", 42, true},
		{"integer bytes", []byte("42"), 42, true},
		{"real bytes", []byte("-42.75"), -42, true},
		{"max integer text", "9223372036854775807", math.MaxInt64, true},
		{"min integer text", "-9223372036854775808", math.MinInt64, true},
		{"null", nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			number := LegacyInt64{Int64: 99, Valid: true}
			if err := s.DB().QueryRow("SELECT ?", tc.value).Scan(&number); err != nil {
				t.Fatal(err)
			}
			if number.Int64 != tc.want || number.Valid != tc.valid || (number.Pointer() != nil) != tc.valid {
				t.Fatalf("scan = %+v, want %d valid=%v", number, tc.want, tc.valid)
			}
			var required int64
			err := s.DB().QueryRow("SELECT ?", tc.value).Scan(ScanInt64(&required))
			if tc.valid && (err != nil || required != tc.want) || !tc.valid && err == nil {
				t.Fatalf("required scan = %d, err = %v", required, err)
			}
		})
	}
	for _, value := range []any{"not a number", "9223372036854775808", "-9223372036854775809", "NaN", "Inf", math.Inf(1)} {
		var number LegacyInt64
		if err := s.DB().QueryRow("SELECT ?", value).Scan(&number); err == nil || number.Valid {
			t.Errorf("invalid %v scanned as %+v, err = %v", value, number, err)
		}
	}
}

func TestDecodeMailboxListWarnings(t *testing.T) {
	original := slog.Default()
	t.Cleanup(func() { slog.SetDefault(original) })
	for _, tc := range []struct {
		name string
		raw  sql.NullString
		want []string
		warn bool
	}{
		{"sql null", sql.NullString{}, []string{}, false},
		{"empty array", sql.NullString{String: "[]", Valid: true}, []string{}, false},
		{"valid", sql.NullString{String: `["example.com"]`, Valid: true}, []string{"example.com"}, false},
		{"malformed", sql.NullString{String: "[", Valid: true}, []string{}, true},
		{"non-array", sql.NullString{String: "null", Valid: true}, []string{}, true},
		{"non-string items", sql.NullString{String: `[null,1,true,{},[],"example.com"]`, Valid: true}, []string{"example.com"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
			if got := DecodeMailboxList(tc.raw, "legacy-mailbox-id", "allowed_domains"); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("list = %v, want %v", got, tc.want)
			}
			if !tc.warn {
				if output.Len() > 0 {
					t.Fatalf("unexpected warning: %s", output.String())
				}
				return
			}
			var warning map[string]any
			if err := json.Unmarshal(output.Bytes(), &warning); err != nil {
				t.Fatal(err)
			}
			if warning["level"] != "WARN" || warning["mailbox_id"] != "legacy-mailbox-id" || warning["column"] != "allowed_domains" {
				t.Fatalf("warning = %v", warning)
			}
		})
	}
}
