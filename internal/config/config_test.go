package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func envMap(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) { value, ok := values[key]; return value, ok }
}

func TestDefaults(t *testing.T) {
	got := FromLookupEnv(envMap(nil))
	want := &Config{
		FlaskHost: "0.0.0.0", FlaskPort: 5000, SMTPHost: "0.0.0.0", SMTPPort: 25,
		InboxFileName: "inbox.json", MaxInboxSize: 100000000, DatabasePath: "data/mailbox.db", UseDatabase: true,
		ProtectedAddresses: "^admin.*", DomainsStr: "localhost", Domains: []string{"localhost"}, Domain: "localhost",
		EmailRetentionDays: 7, EmailRetentionTime: 604800, MaxEmailsPerAddress: 50,
		IPWhitelist: "127.0.0.1,::1,192.168.0.0/16,10.0.0.0/8", MailboxRetentionDays: 30, MaxRetentionDays: 36500,
		MaxMailboxSizeMB: 3, MaxMailboxSizeBytes: 3145728, MaxEmailSizeMB: 2, MaxEmailSizeBytes: 2097152,
		IPBlockDuration: 300, IPMaxFailedAttempts: 3, IPAttemptWindow: 60,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults mismatch\ngot: %+v\nwant: %+v", got, want)
	}
}

func TestClamping(t *testing.T) {
	cases := []struct {
		key, field                 string
		fallback, minimum, maximum int
	}{
		{"FLASK_PORT", "FlaskPort", 5000, 1, 65535}, {"SMTP_PORT", "SMTPPort", 25, 1, 65535},
		{"TRUST_PROXY_HOPS", "TrustProxyHops", 0, 0, 10}, {"MAX_INBOX_SIZE", "MaxInboxSize", 100000000, 1000, 0},
		{"EMAIL_RETENTION_SECONDS", "EmailRetentionSeconds", 0, 0, 0}, {"EMAIL_RETENTION_DAYS", "EmailRetentionDays", 7, 1, 365},
		{"MAX_EMAILS_PER_ADDRESS", "MaxEmailsPerAddress", 50, 1, 1000}, {"MAILBOX_RETENTION_DAYS", "MailboxRetentionDays", 30, 1, 0},
		{"MAX_RETENTION_DAYS", "MaxRetentionDays", 36500, 1, 0}, {"MAX_MAILBOX_SIZE_MB", "MaxMailboxSizeMB", 3, 1, 100},
		{"MAX_EMAIL_SIZE_MB", "MaxEmailSizeMB", 2, 1, 50}, {"IP_BLOCK_DURATION", "IPBlockDuration", 300, 60, 86400},
		{"IP_MAX_FAILED_ATTEMPTS", "IPMaxFailedAttempts", 3, 1, 100}, {"IP_ATTEMPT_WINDOW", "IPAttemptWindow", 60, 10, 3600},
	}
	var warnings bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&warnings, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			for _, value := range []struct {
				raw  string
				want int
			}{{"invalid", tc.fallback}, {"", tc.fallback}, {"-999", tc.minimum}} {
				got := FromLookupEnv(envMap(map[string]string{tc.key: value.raw}))
				if actual := int(reflect.ValueOf(got).Elem().FieldByName(tc.field).Int()); actual != value.want {
					t.Errorf("%q = %d, want %d", value.raw, actual, value.want)
				}
			}
			if tc.maximum > 0 {
				got := FromLookupEnv(envMap(map[string]string{tc.key: "999999"}))
				if actual := int(reflect.ValueOf(got).Elem().FieldByName(tc.field).Int()); actual != tc.maximum {
					t.Errorf("maximum = %d", actual)
				}
			}
		})
	}
	if !strings.Contains(warnings.String(), "WARN") || !strings.Contains(warnings.String(), "below minimum") || !strings.Contains(warnings.String(), "above maximum") {
		t.Fatal("missing clamp warnings")
	}
}

func TestFallbacksAndBooleans(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		domains []string
	}{
		{"default", nil, []string{"localhost"}},
		{"legacy", map[string]string{"DOMAIN": " old.test , next.test "}, []string{"old.test", "next.test"}},
		{"plural", map[string]string{"DOMAIN": "old.test", "DOMAINS": " A.test, ,B.test, "}, []string{"A.test", "", "B.test", ""}},
		{"empty present", map[string]string{"DOMAIN": "old.test", "DOMAINS": ""}, []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := FromLookupEnv(envMap(tc.env))
			if !reflect.DeepEqual(c.Domains, tc.domains) || c.Domain != tc.domains[0] {
				t.Fatalf("%+v", c.Domains)
			}
		})
	}
	for _, raw := range []string{"true", " True \n", "TRUE", "false", "1", "yes", "on", ""} {
		c := FromLookupEnv(envMap(map[string]string{"USE_DATABASE": raw, "ENABLE_IP_WHITELIST": raw, "ENABLE_SENDER_WHITELIST": raw}))
		want := strings.EqualFold(strings.TrimSpace(raw), "true")
		if c.UseDatabase != want || c.EnableIPWhitelist != want || c.EnableSenderWhitelist != want {
			t.Errorf("boolean %q", raw)
		}
	}
	c := FromLookupEnv(envMap(map[string]string{"EMAIL_RETENTION_SECONDS": "12", "EMAIL_RETENTION_DAYS": "30", "MAX_MAILBOX_SIZE_MB": "9", "MAX_EMAIL_SIZE_MB": "4", "PASSWORD": "  secret \n"}))
	if c.EmailRetentionTime != 12 || c.MaxMailboxSizeBytes != 9*1048576 || c.MaxEmailSizeBytes != 4*1048576 || c.Password != "secret" {
		t.Fatalf("conversions: %+v", c)
	}
	c = FromLookupEnv(envMap(map[string]string{"EMAIL_RETENTION_SECONDS": "-2", "EMAIL_RETENTION_DAYS": "2"}))
	if c.EmailRetentionTime != 172800 {
		t.Fatal(c.EmailRetentionTime)
	}
	if !FromEnv(func(string) string { return "" }).UseDatabase {
		t.Fatal("FromEnv empty fallback")
	}
}

func TestLoadDotEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	keys := []string{"FLASK_HOST", "SMTP_HOST", "DOMAINS"}
	for _, key := range keys {
		old, present := os.LookupEnv(key)
		t.Cleanup(func() {
			if present {
				os.Setenv(key, old)
			} else {
				os.Unsetenv(key)
			}
		})
		os.Unsetenv(key)
	}
	t.Setenv("FLASK_HOST", "real-env")
	t.Setenv("DOMAINS", "")
	if err := os.WriteFile(filepath.Join(".env"), []byte("FLASK_HOST=dotenv\nSMTP_HOST=from-file\nDOMAINS=example.test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.FlaskHost != "real-env" || c.SMTPHost != "from-file" || c.Domain != "" {
		t.Fatalf("dotenv precedence: %+v", c)
	}
	if err := os.Remove(".env"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err != nil {
		t.Fatal("missing .env:", err)
	}
	if err := os.WriteFile(".env", []byte("INVALID KEY!=oops\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("malformed dotenv accepted")
	}
}

func TestPythonIntegerSyntax(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"  +1_234 ", 1234}, {"１２３４", 1234}, {"١٢٣٤", 1234},
		{"999999999999999999999999999999999", 65535}, {"-999999999999999999999999999999999", 1},
		{"1__2", 5000}, {"_12", 5000}, {"12_", 5000}, {"0x10", 5000}, {"1.0", 5000},
	} {
		if got := FromLookupEnv(envMap(map[string]string{"FLASK_PORT": tc.raw})).FlaskPort; got != tc.want {
			t.Errorf("%q: %d != %d", tc.raw, got, tc.want)
		}
	}
}
