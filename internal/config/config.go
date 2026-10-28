// Package config 读取与 Python 版本兼容的环境配置。
package config

import (
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/joho/godotenv"
)

type Config struct {
	FlaskHost             string
	FlaskPort             int
	TrustProxyHops        int
	SMTPHost              string
	SMTPPort              int
	InboxFileName         string
	MaxInboxSize          int
	DatabasePath          string
	UseDatabase           bool
	ProtectedAddresses    string
	Password              string
	DomainsStr            string
	Domains               []string
	Domain                string
	EmailRetentionSeconds int
	EmailRetentionDays    int
	EmailRetentionTime    int64
	MaxEmailsPerAddress   int
	EnableIPWhitelist     bool
	IPWhitelist           string
	MailboxRetentionDays  int
	MaxRetentionDays      int
	EnableSenderWhitelist bool
	MaxMailboxSizeMB      int
	MaxMailboxSizeBytes   int64
	MaxEmailSizeMB        int
	MaxEmailSizeBytes     int64
	IPBlockDuration       int
	IPMaxFailedAttempts   int
	IPAttemptWindow       int
}

// Load 加载当前目录的 .env，保留进程中已设置的变量（包括空值）。
func Load() (*Config, error) {
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("load .env: %w", err)
	}
	return FromLookupEnv(os.LookupEnv), nil
}

// FromEnv 将空值视作未设置；需区分显式空值时使用 FromLookupEnv。
func FromEnv(getenv func(string) string) *Config {
	return FromLookupEnv(func(key string) (string, bool) {
		value := getenv(key)
		return value, value != ""
	})
}

// FromLookupEnv 完整保留 os.getenv 对未设置和显式空值的区分。
func FromLookupEnv(lookup func(string) (string, bool)) *Config {
	text := func(key, fallback string) string {
		if value, ok := lookup(key); ok {
			return value
		}
		return fallback
	}
	integer := func(key string, fallback, minimum, maximum int) int {
		raw := text(key, strconv.Itoa(fallback))
		number, ok := decimalInteger(raw)
		if !ok {
			slog.Warn("Invalid integer, using default", "key", key, "default", fallback)
			return fallback
		}
		if number.Cmp(big.NewInt(int64(minimum))) < 0 {
			slog.Warn("Config is below minimum", "key", key, "value", number.String(), "minimum", minimum)
			return minimum
		}
		if maximum != 0 && number.Cmp(big.NewInt(int64(maximum))) > 0 {
			slog.Warn("Config is above maximum", "key", key, "value", number.String(), "maximum", maximum)
			return maximum
		}
		if !number.IsInt64() || int64(int(number.Int64())) != number.Int64() {
			slog.Warn("Integer overflows machine int, using default", "key", key, "default", fallback)
			return fallback
		}
		return int(number.Int64())
	}
	boolean := func(key string, fallback bool) bool {
		return strings.ToLower(strings.TrimSpace(text(key, strconv.FormatBool(fallback)))) == "true"
	}
	c := &Config{
		FlaskHost: text("FLASK_HOST", "0.0.0.0"), FlaskPort: integer("FLASK_PORT", 5000, 1, 65535),
		TrustProxyHops: integer("TRUST_PROXY_HOPS", 0, 0, 10),
		SMTPHost:       text("SMTP_HOST", "0.0.0.0"), SMTPPort: integer("SMTP_PORT", 25, 1, 65535),
		InboxFileName: text("INBOX_FILE_NAME", "inbox.json"), MaxInboxSize: integer("MAX_INBOX_SIZE", 100000000, 1000, 0),
		DatabasePath: text("DATABASE_PATH", "data/mailbox.db"), UseDatabase: boolean("USE_DATABASE", true),
		ProtectedAddresses: text("PROTECTED_ADDRESSES", "^admin.*"), Password: strings.TrimSpace(text("PASSWORD", "")),
		DomainsStr:            text("DOMAINS", text("DOMAIN", "localhost")),
		EmailRetentionSeconds: integer("EMAIL_RETENTION_SECONDS", 0, 0, 0), EmailRetentionDays: integer("EMAIL_RETENTION_DAYS", 7, 1, 365),
		MaxEmailsPerAddress: integer("MAX_EMAILS_PER_ADDRESS", 50, 1, 1000),
		EnableIPWhitelist:   boolean("ENABLE_IP_WHITELIST", false), IPWhitelist: text("IP_WHITELIST", "127.0.0.1,::1,192.168.0.0/16,10.0.0.0/8"),
		MailboxRetentionDays: integer("MAILBOX_RETENTION_DAYS", 30, 1, 0), MaxRetentionDays: integer("MAX_RETENTION_DAYS", 36500, 1, 0),
		EnableSenderWhitelist: boolean("ENABLE_SENDER_WHITELIST", false),
		MaxMailboxSizeMB:      integer("MAX_MAILBOX_SIZE_MB", 3, 1, 100), MaxEmailSizeMB: integer("MAX_EMAIL_SIZE_MB", 2, 1, 50),
		IPBlockDuration: integer("IP_BLOCK_DURATION", 300, 60, 86400), IPMaxFailedAttempts: integer("IP_MAX_FAILED_ATTEMPTS", 3, 1, 100),
		IPAttemptWindow: integer("IP_ATTEMPT_WINDOW", 60, 10, 3600),
	}
	c.Domains = strings.Split(c.DomainsStr, ",")
	for i := range c.Domains {
		c.Domains[i] = strings.TrimSpace(c.Domains[i])
	}
	c.Domain = c.Domains[0]
	c.EmailRetentionTime = int64(c.EmailRetentionSeconds)
	if c.EmailRetentionTime == 0 {
		c.EmailRetentionTime = int64(c.EmailRetentionDays) * 86400
	}
	c.MaxMailboxSizeBytes = int64(c.MaxMailboxSizeMB) * 1024 * 1024
	c.MaxEmailSizeBytes = int64(c.MaxEmailSizeMB) * 1024 * 1024
	return c
}

// decimalInteger 兼容 Python int 的十进制、分隔符和 Unicode 数字。
func decimalInteger(raw string) (*big.Int, bool) {
	raw = strings.TrimSpace(raw)
	var normalized strings.Builder
	if strings.HasPrefix(raw, "+") || strings.HasPrefix(raw, "-") {
		normalized.WriteByte(raw[0])
		raw = raw[1:]
	}
	previousDigit := false
	for _, ch := range raw {
		if ch == '_' {
			if !previousDigit {
				return nil, false
			}
			previousDigit = false
			continue
		}
		digit := -1
		if ch >= '0' && ch <= '9' {
			digit = int(ch - '0')
		} else {
			for _, span := range unicode.Digit.R16 {
				if uint32(ch) >= uint32(span.Lo) && uint32(ch) <= uint32(span.Hi) && (uint32(ch)-uint32(span.Lo))%uint32(span.Stride) == 0 {
					digit = int((uint32(ch)-uint32(span.Lo))/uint32(span.Stride)) % 10
					break
				}
			}
			if digit < 0 {
				for _, span := range unicode.Digit.R32 {
					if uint32(ch) >= span.Lo && uint32(ch) <= span.Hi && (uint32(ch)-span.Lo)%span.Stride == 0 {
						digit = int((uint32(ch)-span.Lo)/span.Stride) % 10
						break
					}
				}
			}
		}
		if digit < 0 {
			return nil, false
		}
		normalized.WriteByte(byte('0' + digit))
		previousDigit = true
	}
	if !previousDigit {
		return nil, false
	}
	return new(big.Int).SetString(normalized.String(), 10)
}
