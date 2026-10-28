package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"math/big"

	"github.com/google/uuid"
)

// GenerateMailboxKey 使用与 Python 相同的 10 位字母数字及三类字符保证。
func GenerateMailboxKey() (string, error) {
	const upper = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	const lower = "abcdefghijklmnopqrstuvwxyz"
	const digits = "0123456789"
	pick := func(alphabet string) (byte, error) {
		i, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return 0, err
		}
		return alphabet[i.Int64()], nil
	}
	key := make([]byte, 10)
	for i := range key {
		alphabet := lower + upper + digits
		switch i {
		case 0:
			alphabet = upper
		case 1:
			alphabet = lower
		case 2:
			alphabet = digits
		}
		value, err := pick(alphabet)
		if err != nil {
			return "", err
		}
		key[i] = value
	}
	for i := len(key) - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return "", err
		}
		key[i], key[j.Int64()] = key[j.Int64()], key[i]
	}
	return string(key), nil
}

func (s *Store) CreateMailbox(address string, options CreateMailboxOptions) (*Mailbox, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	key, err := GenerateMailboxKey()
	if err != nil {
		return nil, err
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	days := 7
	if options.RetentionDays != nil {
		days = *options.RetentionDays
	}
	whitelist := append([]string{}, options.SenderWhitelist...)
	encoded, err := json.Marshal(whitelist)
	if err != nil {
		return nil, err
	}
	source := options.CreatedSource
	if source == "" {
		source = "unknown"
	}
	now := s.now().Unix()
	m := &Mailbox{ID: id.String(), Address: address, CreatedAt: now, ExpiresAt: now + int64(days)*86400,
		RetentionDays: days, SenderWhitelist: whitelist, WhitelistEnabled: len(whitelist) > 0,
		AccessToken: base64.RawURLEncoding.EncodeToString(token), MailboxKey: key, IsActive: true,
		CreatedSource: source, CreatedByIP: options.CreatedByIP, LastAccessed: &now,
		StorageLimit: 3145728, AllowedDomains: []string{}, created: true}
	_, err = s.db.Exec(`INSERT INTO mailboxes (id, address, created_at, expires_at, retention_days,
 sender_whitelist, whitelist_enabled, created_by_ip, access_token, mailbox_key, last_accessed, created_source)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, m.ID, address, now, m.ExpiresAt, days,
		string(encoded), m.WhitelistEnabled, options.CreatedByIP, m.AccessToken, key, now, source)
	if err != nil {
		return nil, err
	}
	return m, nil
}

const mailboxSelect = `SELECT id, address, created_at, expires_at, retention_days, is_active,
 sender_whitelist, whitelist_enabled, created_by_ip, access_token, mailbox_key, last_accessed,
 storage_used, storage_limit, created_source, allowed_domains, updated_by_admin, updated_at FROM mailboxes`

type scanner interface{ Scan(...any) error }

func (s *Store) scanMailbox(row scanner) (*Mailbox, error) {
	var m Mailbox
	var active, enabled, used, limit, retention, accessed, updated LegacyInt64
	var whitelist, token, key, source, domains sql.NullString
	if err := row.Scan(&m.ID, &m.Address, ScanInt64(&m.CreatedAt), ScanInt64(&m.ExpiresAt), &retention, &active,
		&whitelist, &enabled, &m.CreatedByIP, &token, &key, &accessed,
		&used, &limit, &source, &domains, &m.UpdatedByAdmin, &updated); err != nil {
		return nil, notFound(err)
	}
	m.IsActive = active.Int64 != 0
	m.WhitelistEnabled = enabled.Int64 != 0
	m.AccessToken, m.MailboxKey, m.CreatedSource = token.String, key.String, source.String
	m.nullAccessToken = !token.Valid
	m.RetentionDays = int(retention.Int64)
	m.nullRetentionDays = !retention.Valid
	m.LastAccessed, m.UpdatedAt = accessed.Pointer(), updated.Pointer()
	m.StorageUsed, m.StorageLimit = used.Int64, limit.Int64
	if m.StorageLimit == 0 {
		m.StorageLimit = s.cfg.MaxMailboxSizeBytes
	}
	m.SenderWhitelist = DecodeMailboxList(whitelist, m.ID, "sender_whitelist")
	m.AllowedDomains = DecodeMailboxList(domains, m.ID, "allowed_domains")
	return &m, nil
}

func (s *Store) GetMailboxByAddress(address string) (*Mailbox, error) {
	return s.scanMailbox(s.db.QueryRow(mailboxSelect+" WHERE address = ?", address))
}
func (s *Store) GetMailboxByToken(token string) (*Mailbox, error) {
	return s.scanMailbox(s.db.QueryRow(mailboxSelect+" WHERE access_token = ?", token))
}
func (s *Store) GetMailboxByID(id string) (*Mailbox, error) {
	return s.scanMailbox(s.db.QueryRow(mailboxSelect+" WHERE id = ?", id))
}
func (s *Store) UpdateMailboxAccess(id string) error {
	_, err := s.db.Exec("UPDATE mailboxes SET last_accessed = ? WHERE id = ?", s.now().Unix(), id)
	return err
}
func (s *Store) IsMailboxExpired(m *Mailbox) bool { return s.now().Unix() > m.ExpiresAt }

func (s *Store) changeWhitelist(address, sender string, add bool) (bool, error) {
	found := false
	err := s.WithTx(context.Background(), func(tx *sql.Tx) error {
		var id string
		var encoded sql.NullString
		err := tx.QueryRow("SELECT id, sender_whitelist FROM mailboxes WHERE address = ?", address).Scan(&id, &encoded)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		whitelist := DecodeMailboxList(encoded, id, "sender_whitelist")
		index := -1
		for i, value := range whitelist {
			if value == sender {
				index = i
				break
			}
		}
		if add && index < 0 {
			whitelist = append(whitelist, sender)
		}
		if !add && index >= 0 {
			whitelist = append(whitelist[:index], whitelist[index+1:]...)
		}
		data, err := json.Marshal(whitelist)
		if err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE mailboxes SET sender_whitelist = ? WHERE address = ?", string(data), address)
		return err
	})
	return found && err == nil, err
}
func (s *Store) AddSenderToWhitelist(address, sender string) (bool, error) {
	return s.changeWhitelist(address, sender, true)
}
func (s *Store) RemoveSenderFromWhitelist(address, sender string) (bool, error) {
	return s.changeWhitelist(address, sender, false)
}
func (s *Store) UpdateMailboxRetention(address string, days int) (bool, error) {
	count, err := affected(s.db.Exec("UPDATE mailboxes SET retention_days = ?, expires_at = ? WHERE address = ?", days, s.now().Unix()+int64(days)*86400, address))
	return count > 0, err
}
func (s *Store) RegenerateMailboxKey(address, currentKey string) (string, error) {
	key, err := GenerateMailboxKey()
	if err != nil {
		return "", err
	}
	// 比较并交换，旧凭据不能并发覆盖新密钥。
	count, err := affected(s.db.Exec("UPDATE mailboxes SET mailbox_key = ? WHERE address = ? AND mailbox_key = ?", key, address, currentKey))
	if err != nil {
		return "", err
	}
	if count == 0 {
		return "", ErrNotFound
	}
	return key, nil
}
