// Package inbox 提供按邮箱隔离的收信和管理服务。
package inbox

import "encoding/json"

// Mailbox 保留 Python 创建和查询时各自的字段集合。
type Mailbox struct {
	ID               string   `json:"id"`
	Address          string   `json:"address"`
	CreatedAt        int64    `json:"created_at"`
	ExpiresAt        int64    `json:"expires_at"`
	RetentionDays    int      `json:"retention_days"`
	SenderWhitelist  []string `json:"sender_whitelist"`
	AllowedDomains   []string `json:"-"`
	WhitelistEnabled bool     `json:"whitelist_enabled"`
	AccessToken      string   `json:"access_token"`
	MailboxKey       string   `json:"mailbox_key"`
	IsActive         bool     `json:"is_active"`
	AdminDisabled    bool     `json:"admin_disabled"`
	CreatedSource    string   `json:"created_source,omitempty"`
	CreatedByIP      *string  `json:"created_by_ip"`
	LastAccessed     *int64   `json:"last_accessed"`
	StorageUsed      int64    `json:"storage_used"`
	StorageLimit     int64    `json:"storage_limit"`
	created          bool
	nullToken        bool
	nullRetention    bool
}

func (m Mailbox) MarshalJSON() ([]byte, error) {
	type plain Mailbox
	return marshalFields(plain(m), func(fields map[string]json.RawMessage) {
		if m.created {
			for _, key := range []string{"created_by_ip", "last_accessed", "storage_used", "storage_limit"} {
				delete(fields, key)
			}
		} else {
			delete(fields, "created_source")
		}
		if m.nullToken {
			fields["access_token"] = json.RawMessage("null")
		}
		if m.nullRetention {
			fields["retention_days"] = json.RawMessage("null")
		}
	})
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
	nullFields  []string
}

func (e Email) MarshalJSON() ([]byte, error) {
	type plain Email
	return marshalFields(plain(e), func(fields map[string]json.RawMessage) {
		for _, key := range e.nullFields {
			fields[key] = json.RawMessage("null")
		}
	})
}

type MailboxInfo struct {
	ID               string   `json:"id"`
	Address          string   `json:"address"`
	CreatedAt        int64    `json:"created_at"`
	ExpiresAt        int64    `json:"expires_at"`
	RetentionDays    int      `json:"retention_days"`
	SenderWhitelist  []string `json:"sender_whitelist"`
	WhitelistEnabled bool     `json:"whitelist_enabled"`
	AccessToken      string   `json:"access_token"`
	MailboxKey       string   `json:"mailbox_key"`
	IsActive         bool     `json:"is_active"`
	AdminDisabled    bool     `json:"admin_disabled"`
	IsExpired        bool     `json:"is_expired"`
	EmailCount       int64    `json:"email_count"`
	UnreadCount      int64    `json:"unread_count"`
	LastEmailTime    *int64   `json:"last_email_time"`
	nullToken        bool
	nullRetention    bool
}

func (m MailboxInfo) MarshalJSON() ([]byte, error) {
	type plain MailboxInfo
	return marshalFields(plain(m), func(fields map[string]json.RawMessage) {
		if m.nullToken {
			fields["access_token"] = json.RawMessage("null")
		}
		if m.nullRetention {
			fields["retention_days"] = json.RawMessage("null")
		}
	})
}

type RecipientOutcome struct {
	Recipient string `json:"recipient"`
	Accepted  bool   `json:"accepted"`
	Reason    string `json:"reason"`
	EmailID   string `json:"email_id,omitempty"`
}

type DeliveryResult struct {
	Recipients []RecipientOutcome `json:"recipients"`
	Accepted   int                `json:"accepted"`
	Rejected   int                `json:"rejected"`
}

type CleanupResult struct {
	Mailboxes int64 `json:"mailboxes"`
	Emails    int64 `json:"emails"`
	Bytes     int64 `json:"bytes"`
}

func marshalFields(value any, change func(map[string]json.RawMessage)) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	change(fields)
	return json.Marshal(fields)
}
