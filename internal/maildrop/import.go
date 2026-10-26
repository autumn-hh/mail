package maildrop

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// ImportJSON 在单个事务中导入旧 JSON；不修改源文件，不覆盖已有邮箱。
func (s *Store) ImportJSON(path string) (int, error) {
	f, e := os.Open(path)
	if e != nil {
		return 0, e
	}
	defer f.Close()
	data := map[string]json.RawMessage{}
	if e = json.NewDecoder(f).Decode(&data); e != nil {
		return 0, e
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	count := 0
	for address, raw := range data {
		_, e := one(tx, "SELECT id FROM mailboxes WHERE address=?", address)
		if e == nil {
			continue
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return 0, e
		}
		var emails []Object
		info := Object{}
		if e = json.Unmarshal(raw, &emails); e != nil {
			if e = json.Unmarshal(raw, &info); e != nil {
				return 0, fmt.Errorf("invalid mailbox JSON: %w", e)
			}
			encoded, e := json.Marshal(info["emails"])
			if e != nil {
				return 0, e
			}
			if string(encoded) != "null" {
				if e = json.Unmarshal(encoded, &emails); e != nil {
					return 0, e
				}
			}
		}
		now := time.Now().Unix()
		created := num(info["created_at"])
		if created == 0 {
			created = now
		}
		expires := num(info["expires_at"])
		if expires == 0 {
			expires = created + int64(s.Config.RetentionDays)*86400
		}
		days := max(1, (expires-created)/86400)
		id := str(info["id"])
		if id == "" {
			id = token()
		}
		access := str(info["access_token"])
		if access == "" {
			access = token()
		}
		key := str(info["mailbox_key"])
		if key == "" {
			key = mailboxKey()
		}
		active := true
		if v, ok := info["is_active"]; ok {
			active = truth(v)
		}
		limit := num(info["storage_limit"])
		if limit <= 0 {
			limit = int64(s.Config.MaxMailboxBytes)
		}
		_, e = tx.Exec("INSERT INTO mailboxes(id,address,created_at,expires_at,retention_days,is_active,sender_whitelist,whitelist_enabled,access_token,mailbox_key,created_source,allowed_domains,storage_used,storage_limit) VALUES(?,?,?,?,?,?,?,?,?,?,?, ?,0,?)", id, address, created, expires, days, active, jsonText(stringsValue(info["sender_whitelist"])), truth(info["whitelist_enabled"]), access, key, "json_import", jsonText(stringsValue(info["allowed_domains"])), limit)
		if e != nil {
			return 0, e
		}
		for _, email := range emails {
			eid := str(email["id"])
			if eid == "" {
				eid = token()
			}
			stamp := num(email["Timestamp"])
			if stamp == 0 {
				stamp = now
			}
			content := str(email["ContentType"])
			if content == "" {
				content = "Text"
			}
			to := str(email["To"])
			if to == "" {
				to = address
			}
			size := len(str(email["From"])) + len(to) + len(str(email["Subject"])) + len(str(email["Body"])) + len(str(email["Sent"]))
			_, e = tx.Exec("INSERT INTO emails(id,mailbox_id,from_address,to_address,subject,body,content_type,timestamp,sent_formatted,is_read,size_bytes) VALUES(?,?,?,?,?,?,?,?,?,?,?)", eid, id, str(email["From"]), to, str(email["Subject"]), str(email["Body"]), content, stamp, str(email["Sent"]), truth(email["is_read"]), size)
			if e != nil {
				return 0, e
			}
		}
		if _, e = tx.Exec("UPDATE mailboxes SET storage_used=COALESCE((SELECT SUM(size_bytes) FROM emails WHERE mailbox_id=?),0) WHERE id=?", id, id); e != nil {
			return 0, e
		}
		count++
	}
	if e = tx.Commit(); e != nil {
		return 0, e
	}
	return count, nil
}
