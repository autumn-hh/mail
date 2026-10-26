package maildrop

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config 保留原有环境变量，域名环境变量仅用于数据库首次初始化。
type Config struct {
	HTTPAddr, SMTPAddr, DatabasePath, Password, InboxFile  string
	Domains                                                []string
	Protected                                              *regexp.Regexp
	RetentionDays, MaxRetentionDays, EmailRetentionSeconds int
	MaxEmailBytes, MaxMailboxBytes, MaxEmails, ProxyHops   int
	IPWhitelistEnabled                                     bool
	IPWhitelist                                            []string
	BlockDuration, MaxAttempts, AttemptWindow              int
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
func envInt(key string, fallback, min, max int) int {
	v, err := strconv.Atoi(env(key, strconv.Itoa(fallback)))
	if err != nil {
		slog.Warn("Invalid integer configuration", "key", key)
		return fallback
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
func LoadConfig() (Config, error) {
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("load .env: %w", err)
	}
	domains, err := NormalizeDomains(strings.Split(env("DOMAINS", env("DOMAIN", "localhost")), ","))
	if err != nil {
		return Config{}, err
	}
	protected, err := regexp.Compile(env("PROTECTED_ADDRESSES", "^admin.*"))
	if err != nil {
		return Config{}, fmt.Errorf("PROTECTED_ADDRESSES: %w", err)
	}
	c := Config{
		HTTPAddr:     net.JoinHostPort(env("FLASK_HOST", "0.0.0.0"), strconv.Itoa(envInt("FLASK_PORT", 5000, 1, 65535))),
		SMTPAddr:     net.JoinHostPort(env("SMTP_HOST", "0.0.0.0"), strconv.Itoa(envInt("SMTP_PORT", 25, 1, 65535))),
		DatabasePath: env("DATABASE_PATH", "data/mailbox.db"), Password: strings.TrimSpace(env("PASSWORD", "")),
		InboxFile: env("INBOX_FILE_NAME", "inbox.json"), Domains: domains, Protected: protected,
		RetentionDays: envInt("MAILBOX_RETENTION_DAYS", 30, 1, 36500), MaxRetentionDays: envInt("MAX_RETENTION_DAYS", 36500, 1, 36500),
		EmailRetentionSeconds: envInt("EMAIL_RETENTION_SECONDS", 0, 0, 31536000),
		MaxEmailBytes:         envInt("MAX_EMAIL_SIZE_MB", 2, 1, 50) * 1024 * 1024, MaxMailboxBytes: envInt("MAX_MAILBOX_SIZE_MB", 3, 1, 100) * 1024 * 1024,
		MaxEmails: envInt("MAX_EMAILS_PER_ADDRESS", 50, 1, 1000), ProxyHops: envInt("TRUST_PROXY_HOPS", 0, 0, 10),
		IPWhitelistEnabled: strings.EqualFold(env("ENABLE_IP_WHITELIST", "false"), "true"),
		IPWhitelist:        strings.Split(env("IP_WHITELIST", "127.0.0.1,::1,192.168.0.0/16,10.0.0.0/8"), ","),
		BlockDuration:      envInt("IP_BLOCK_DURATION", 300, 60, 86400), MaxAttempts: envInt("IP_MAX_FAILED_ATTEMPTS", 3, 1, 100), AttemptWindow: envInt("IP_ATTEMPT_WINDOW", 60, 10, 3600),
	}
	if c.EmailRetentionSeconds == 0 {
		c.EmailRetentionSeconds = envInt("EMAIL_RETENTION_DAYS", 7, 1, 365) * 86400
	}
	if strings.EqualFold(env("USE_DATABASE", "true"), "false") {
		return c, fmt.Errorf("Go runtime requires SQLite; import legacy JSON with -import-json before starting")
	}
	return c, nil
}

var domainLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func NormalizeDomains(input []string) ([]string, error) {
	if len(input) == 0 || len(input) > 100 {
		return nil, bad("请配置 1–100 个域名")
	}
	result := []string{}
	seen := map[string]bool{}
	for _, raw := range input {
		d := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
		if len(d) == 0 || len(d) > 253 {
			return nil, bad("域名不能为空或超过 253 个字符")
		}
		for _, label := range strings.Split(d, ".") {
			if !domainLabel.MatchString(label) {
				return nil, bad("域名格式不正确，请仅填写域名，不含协议、端口或路径")
			}
		}
		if !seen[d] {
			result = append(result, d)
			seen[d] = true
		}
	}
	return result, nil
}
func token() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func mailboxKey() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	for {
		b := make([]byte, 10)
		upper, lower, digit := false, false, false
		for i := range b {
			var n [1]byte
			for {
				if _, err := rand.Read(n[:]); err != nil {
					panic(err)
				}
				if n[0] < 248 {
					break
				}
			}
			b[i] = alphabet[int(n[0])%len(alphabet)]
			upper = upper || (b[i] >= 'A' && b[i] <= 'Z')
			lower = lower || (b[i] >= 'a' && b[i] <= 'z')
			digit = digit || (b[i] >= '0' && b[i] <= '9')
		}
		if upper && lower && digit {
			return string(b)
		}
	}
}
func (c Config) IPAllowed(ip string) bool {
	if !c.IPWhitelistEnabled {
		return true
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, entry := range c.IPWhitelist {
		entry = strings.TrimSpace(entry)
		if p := net.ParseIP(entry); p != nil && p.Equal(parsed) {
			return true
		}
		if _, network, e := net.ParseCIDR(entry); e == nil && network.Contains(parsed) {
			return true
		}
	}
	return false
}
