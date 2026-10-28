package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
)

// migrateLocalSettings 兼容本地单包 Go 版本的域名设置，保留已有模块化域名配置。
func (s *Store) migrateLocalSettings(tx *sql.Tx) error {
	var exists int
	if err := tx.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='settings'").Scan(&exists); err != nil || exists == 0 {
		return err
	}
	var raw string
	err := tx.QueryRow("SELECT value FROM settings WHERE key='domains'").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow("SELECT COUNT(*) FROM domains").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		var values []string
		if err := json.Unmarshal([]byte(raw), &values); err != nil {
			return fmt.Errorf("legacy domain settings: %w", err)
		}
		if len(values) == 0 {
			return ErrInvalidDomain
		}
		for _, value := range values {
			domain, err := NormalizeDomain(value)
			if err != nil {
				return fmt.Errorf("legacy domain settings: %w", err)
			}
			if _, err := tx.Exec("INSERT OR IGNORE INTO domains(domain,created_at,updated_at) VALUES(?,?,?)", domain, s.now().Unix(), s.now().Unix()); err != nil {
				return err
			}
		}
	}
	// 本地版本保留过没有大小记账的历史邮件；按 UTF-8 字节补齐。
	if _, err := tx.Exec(`UPDATE emails SET size_bytes=length(CAST(COALESCE(from_address,'')||COALESCE(to_address,'')||COALESCE(subject,'')||COALESCE(body,'')||COALESCE(sent_formatted,'') AS BLOB)) WHERE size_bytes IS NULL OR size_bytes=0`); err != nil {
		return err
	}
	_, err = tx.Exec("UPDATE mailboxes SET storage_used=COALESCE((SELECT SUM(size_bytes) FROM emails WHERE mailbox_id=mailboxes.id),0)")
	return err
}

// ImportJSON 单事务导入旧 JSON，不改源文件，不覆盖已有地址或凭据。
func (s *Store) ImportJSON(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	data := map[string]json.RawMessage{}
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&data); err != nil {
		return 0, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return 0, errors.New("invalid trailing JSON data")
	}
	addresses := make([]string, 0, len(data))
	for address := range data {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	count := 0
	err = s.WithTx(context.Background(), func(tx *sql.Tx) error {
		for _, address := range addresses {
			var exists int
			if err := tx.QueryRow("SELECT COUNT(*) FROM mailboxes WHERE address=?", address).Scan(&exists); err != nil {
				return err
			}
			if exists > 0 {
				continue
			}
			var emails []Email
			info := struct {
				ID               string   `json:"id"`
				CreatedAt        int64    `json:"created_at"`
				ExpiresAt        int64    `json:"expires_at"`
				AccessToken      string   `json:"access_token"`
				MailboxKey       string   `json:"mailbox_key"`
				IsActive         *bool    `json:"is_active"`
				SenderWhitelist  []string `json:"sender_whitelist"`
				WhitelistEnabled bool     `json:"whitelist_enabled"`
				AllowedDomains   []string `json:"allowed_domains"`
				StorageLimit     int64    `json:"storage_limit"`
				Emails           []Email  `json:"emails"`
			}{}
			if err := json.Unmarshal(data[address], &emails); err != nil {
				if err := json.Unmarshal(data[address], &info); err != nil {
					return fmt.Errorf("invalid mailbox JSON: %w", err)
				}
				emails = info.Emails
			}
			now := s.now().Unix()
			if info.CreatedAt == 0 {
				info.CreatedAt = now
			}
			if info.ExpiresAt == 0 {
				info.ExpiresAt = info.CreatedAt + int64(s.cfg.MailboxRetentionDays)*86400
			}
			if info.ID == "" {
				info.ID, err = importToken()
				if err != nil {
					return err
				}
			}
			if info.AccessToken == "" {
				info.AccessToken, err = importToken()
				if err != nil {
					return err
				}
			}
			if info.MailboxKey == "" {
				info.MailboxKey, err = GenerateMailboxKey()
				if err != nil {
					return err
				}
			}
			if info.StorageLimit <= 0 {
				info.StorageLimit = s.cfg.MaxMailboxSizeBytes
			}
			if info.SenderWhitelist == nil {
				info.SenderWhitelist = []string{}
			}
			if info.AllowedDomains == nil {
				info.AllowedDomains = []string{}
			}
			whitelist, err := json.Marshal(info.SenderWhitelist)
			if err != nil {
				return err
			}
			domains, err := json.Marshal(info.AllowedDomains)
			if err != nil {
				return err
			}
			active := info.IsActive == nil || *info.IsActive
			_, err = tx.Exec(`INSERT INTO mailboxes(id,address,created_at,expires_at,retention_days,is_active,sender_whitelist,whitelist_enabled,access_token,mailbox_key,created_source,allowed_domains,storage_used,storage_limit) VALUES(?,?,?,?,?,?,?,?,?,?,?, ?,0,?)`, info.ID, address, info.CreatedAt, info.ExpiresAt, max(1, (info.ExpiresAt-info.CreatedAt)/86400), active, string(whitelist), info.WhitelistEnabled, info.AccessToken, info.MailboxKey, "json_import", string(domains), info.StorageLimit)
			if err != nil {
				return err
			}
			var used int64
			for _, email := range emails {
				if email.ID == "" {
					email.ID, err = importToken()
					if err != nil {
						return err
					}
				}
				if email.Timestamp == 0 {
					email.Timestamp = now
				}
				if email.ContentType == "" {
					email.ContentType = "Text"
				}
				if email.To == "" {
					email.To = address
				}
				size := int64(len(email.From) + len(email.To) + len(email.Subject) + len(email.Body) + len(email.Sent))
				_, err := tx.Exec(`INSERT INTO emails(id,mailbox_id,from_address,to_address,subject,body,content_type,timestamp,sent_formatted,is_read,size_bytes) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, email.ID, info.ID, email.From, email.To, email.Subject, email.Body, email.ContentType, email.Timestamp, email.Sent, email.IsRead, size)
				if err != nil {
					return err
				}
				used += size
			}
			if _, err := tx.Exec("UPDATE mailboxes SET storage_used=? WHERE id=?", used, info.ID); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

func importToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}
