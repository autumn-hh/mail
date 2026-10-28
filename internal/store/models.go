package store

import "encoding/json"

type Mailbox struct {
	ID                string   `json:"id"`
	Address           string   `json:"address"`
	CreatedAt         int64    `json:"created_at"`
	ExpiresAt         int64    `json:"expires_at"`
	RetentionDays     int      `json:"retention_days"`
	SenderWhitelist   []string `json:"sender_whitelist"`
	WhitelistEnabled  bool     `json:"whitelist_enabled"`
	AccessToken       string   `json:"access_token"`
	MailboxKey        string   `json:"mailbox_key"`
	IsActive          bool     `json:"is_active"`
	CreatedSource     string   `json:"created_source,omitempty"`
	CreatedByIP       *string  `json:"created_by_ip"`
	LastAccessed      *int64   `json:"last_accessed"`
	StorageUsed       int64    `json:"storage_used"`
	StorageLimit      int64    `json:"storage_limit"`
	AllowedDomains    []string `json:"allowed_domains"`
	UpdatedByAdmin    *string  `json:"updated_by_admin"`
	UpdatedAt         *int64   `json:"updated_at"`
	created           bool
	nullAccessToken   bool
	nullRetentionDays bool
}

// MarshalJSON 保留 Python 创建和查询返回的不同字段集合。
func (m Mailbox) MarshalJSON() ([]byte, error) {
	type plain Mailbox
	data, err := json.Marshal(plain(m))
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	for _, key := range []string{"allowed_domains", "updated_by_admin", "updated_at"} {
		delete(fields, key)
	}
	if m.created {
		for _, key := range []string{"created_by_ip", "last_accessed", "storage_used", "storage_limit"} {
			delete(fields, key)
		}
	} else {
		delete(fields, "created_source")
	}
	if m.nullRetentionDays {
		fields["retention_days"] = json.RawMessage("null")
	}
	if m.nullAccessToken {
		fields["access_token"] = json.RawMessage("null")
	}
	return json.Marshal(fields)
}

type Email struct {
	ID          string `json:"id"`
	MailboxID   string `json:"mailbox_id,omitempty"`
	From        string `json:"From"`
	To          string `json:"To"`
	Subject     string `json:"Subject"`
	Body        string `json:"Body"`
	ContentType string `json:"ContentType"`
	Timestamp   int64  `json:"Timestamp"`
	Sent        string `json:"Sent"`
	IsRead      bool   `json:"is_read"`
	SizeBytes   int64  `json:"-"`
	nullFields  []string
}

func (e Email) MarshalJSON() ([]byte, error) {
	type plain Email
	data, err := json.Marshal(plain(e))
	if err != nil || len(e.nullFields) == 0 {
		return data, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	for _, key := range e.nullFields {
		fields[key] = json.RawMessage("null")
	}
	return json.Marshal(fields)
}

type SubAdmin struct {
	ID               string   `json:"id"`
	Token            string   `json:"token"`
	Domains          []string `json:"domains"`
	SenderWhitelist  []string `json:"sender_whitelist"`
	MaxRetentionDays int      `json:"max_retention_days"`
	CreatedAt        int64    `json:"created_at"`
	UpdatedAt        int64    `json:"updated_at"`
	IsActive         int      `json:"is_active"`
	CreatedBy        *string  `json:"created_by"`
	Notes            *string  `json:"notes"`
}

type AuditLog struct {
	ID        string         `json:"id"`
	Timestamp int64          `json:"timestamp"`
	Action    string         `json:"action"`
	MailboxID *string        `json:"mailbox_id"`
	AdminUser *string        `json:"admin_user"`
	Changes   map[string]any `json:"changes"`
	IPAddress *string        `json:"ip_address"`
}

type Domain struct {
	Domain    string `json:"domain"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type MailboxStats struct {
	TotalEmails    int64   `json:"total_emails"`
	UnreadEmails   int64   `json:"unread_emails"`
	LastEmailTime  *int64  `json:"last_email_time"`
	StorageUsed    int64   `json:"storage_used"`
	StorageLimit   int64   `json:"storage_limit"`
	StorageUsedMB  float64 `json:"storage_used_mb"`
	StorageLimitMB float64 `json:"storage_limit_mb"`
	StoragePercent float64 `json:"storage_percent"`
}

type CreateMailboxOptions struct {
	// nil 使用 DatabaseManager.create_mailbox 的默认 7 天；显式 0 保持原语义。
	RetentionDays   *int
	SenderWhitelist []string
	CreatedByIP     *string
	CreatedSource   string
}

type CreateSubAdminOptions struct {
	SenderWhitelist  []string
	MaxRetentionDays *int
	CreatedBy        *string
	Notes            *string
}

type SubAdminUpdate struct {
	// nil 不更新，非 nil 空切片清空。
	Domains          []string
	SenderWhitelist  []string
	MaxRetentionDays *int
	IsActive         *int
	Notes            *string
}
