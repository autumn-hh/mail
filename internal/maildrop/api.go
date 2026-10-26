package maildrop

import (
	"database/sql"
	"errors"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"time"
)

func validateWhitelist(items []string) error {
	for _, item := range items {
		if strings.Contains(strings.TrimPrefix(item, "@"), "@") {
			p, e := mail.ParseAddress(item)
			if e != nil || p.Address != item {
				return bad("发件人白名单格式错误")
			}
		} else {
			if _, e := NormalizeDomains([]string{strings.TrimPrefix(item, "@")}); e != nil {
				return e
			}
		}
	}
	return nil
}
func (a *App) validateAddress(address string, admin bool) error {
	p := strings.Split(address, "@")
	pattern := "^[a-zA-Z0-9_]{3,20}$"
	if admin {
		pattern = "^[a-zA-Z0-9]{3,20}$"
	}
	if len(p) != 2 || !regexp.MustCompile(pattern).MatchString(p[0]) {
		return bad("邮箱用户名格式不正确（3–20 位英文或数字）")
	}
	if a.Config.Protected.MatchString(p[0]) {
		return bad("该邮箱地址受保护")
	}
	d, e := a.Store.Domains()
	if e != nil {
		return e
	}
	if !contains(d, p[1]) {
		return bad("邮箱域名不在可用列表中")
	}
	return nil
}
func (a *App) routes() {
	a.handle("GET /api/get_domain", false, func(r *http.Request) (any, error) {
		d, e := a.Store.Domains()
		if e != nil {
			return nil, e
		}
		return Object{"domain": d[0], "available_domains": d}, nil
	})
	a.handle("GET /api/get_random_address", false, func(r *http.Request) (any, error) {
		d, e := a.Store.Domains()
		if e != nil {
			return nil, e
		}
		return Object{"address": strings.ToLower(mailboxKey()+mailboxKey()[:6]) + "@" + d[int(time.Now().UnixNano()%int64(len(d)))], "available_domains": d}, nil
	})
	a.handle("POST /api/admin_login", false, func(r *http.Request) (any, error) {
		if a.Config.Password == "" {
			return nil, &HTTPError{503, "管理员密码未配置"}
		}
		v, e := body(r)
		if e != nil {
			return nil, e
		}
		p, e := textField(v, "password", true)
		if e != nil {
			return nil, e
		}
		if e = a.adminAuth(r, p); e != nil {
			return nil, e
		}
		return ok(), nil
	})
	a.handle("POST /api/get_mailbox_token", false, func(r *http.Request) (any, error) {
		if a.limiter.blocked(a.ip(r)) {
			return nil, &HTTPError{403, "IP已被临时封禁"}
		}
		v, e := body(r)
		if e != nil {
			return nil, e
		}
		addr, e := textField(v, "address", true)
		if e != nil {
			return nil, e
		}
		key, e := textField(v, "mailbox_key", true)
		if e != nil {
			return nil, e
		}
		m, e := a.Store.Mailbox("address", addr)
		if e != nil {
			return nil, e
		}
		if !equalSecret(key, str(m["mailbox_key"])) {
			a.limiter.fail(a.ip(r))
			return nil, &HTTPError{401, "Invalid mailbox key"}
		}
		if truth(m["is_expired"]) {
			return nil, &HTTPError{410, "Mailbox expired"}
		}
		if !truth(m["is_active"]) {
			return nil, &HTTPError{423, "Mailbox disabled"}
		}
		return Object{"success": true, "address": addr, "access_token": m["access_token"], "mailbox_id": m["id"], "expires_at": m["expires_at"]}, nil
	})
	a.handle("GET /api/mailbox_info_v2", false, func(r *http.Request) (any, error) {
		m, e := a.authMailbox(r, "", false)
		if e != nil {
			return nil, e
		}
		return Object{"success": true, "mailbox": publicMailbox(m), "storage_type": "database"}, nil
	})
	a.handle("GET /api/get_inbox", false, func(r *http.Request) (any, error) {
		address := r.URL.Query().Get("address")
		if address == "" {
			return nil, bad("Address is required")
		}
		m, e := a.authMailbox(r, address, false)
		if e != nil {
			return nil, e
		}
		if _, e = a.Store.DB.Exec("UPDATE mailboxes SET last_accessed=? WHERE id=?", time.Now().Unix(), m["id"]); e != nil {
			return nil, e
		}
		return a.Store.Emails(str(m["id"]))
	})
	a.handle("GET /api/get_email", false, func(r *http.Request) (any, error) {
		address, id := r.URL.Query().Get("address"), r.URL.Query().Get("id")
		if address == "" || id == "" {
			return nil, bad("Missing address or email ID")
		}
		m, e := a.authMailbox(r, address, false)
		if e != nil {
			return nil, e
		}
		email, e := a.Store.Email(id, str(m["id"]))
		if e != nil {
			return nil, e
		}
		if _, e = a.Store.DB.Exec("UPDATE emails SET is_read=1 WHERE id=? AND mailbox_id=?", id, m["id"]); e != nil {
			return nil, e
		}
		email["is_read"] = true
		return email, nil
	})
	for _, path := range []string{"mark_email_read", "delete_email", "delete_emails_batch", "mark_all_read", "add_sender_whitelist", "remove_sender_whitelist", "update_retention", "regenerate_mailbox_key", "toggle_mailbox_status", "toggle_whitelist"} {
		p := path
		a.handle("POST /api/"+p, false, func(r *http.Request) (any, error) { return a.mailboxWrite(r, p) })
	}
	a.handle("POST /api/register", false, func(r *http.Request) (any, error) { return a.register(r, false) })
	a.handle("POST /api/register_with_token", false, func(r *http.Request) (any, error) { return a.register(r, true) })
}
func (a *App) register(r *http.Request, sub bool) (any, error) {
	domains, e := a.Store.Domains()
	if e != nil {
		return nil, e
	}
	whitelist := []string{}
	maxDays := 365
	source := "register"
	if sub {
		source = "sub_admin_token"
		if a.limiter.blocked(a.ip(r)) {
			return nil, &HTTPError{403, "IP已被临时封禁"}
		}
		v, err := one(a.Store.DB, "SELECT * FROM sub_admins WHERE token=? AND is_active=1", r.Header.Get("X-Sub-Admin-Token"))
		if errors.Is(err, sql.ErrNoRows) {
			a.limiter.fail(a.ip(r))
			return nil, &HTTPError{401, "Invalid sub-admin token"}
		}
		if err != nil {
			return nil, err
		}
		permitted := []string{}
		for _, d := range stringsValue(v["domains"]) {
			if contains(domains, d) {
				permitted = append(permitted, d)
			}
		}
		domains = permitted
		whitelist = stringsValue(v["sender_whitelist"])
		maxDays = min(365, int(num(v["max_retention_days"])))
	} else {
		provided := bearer(r)
		if provided == "" {
			provided = r.Header.Get("Authorization")
		}
		if e = a.adminAuth(r, provided); e != nil {
			return nil, e
		}
	}
	if len(domains) == 0 {
		return nil, &HTTPError{403, "没有可用的域名权限"}
	}
	v, e := body(r)
	if e != nil {
		return nil, e
	}
	address, e := textField(v, "email", true)
	if e != nil {
		return nil, e
	}
	if !strings.Contains(address, "@") {
		address += "@" + domains[0]
	}
	if e = a.validateAddress(address, false); e != nil {
		return nil, e
	}
	if !contains(domains, strings.Split(address, "@")[1]) {
		return nil, &HTTPError{403, "Domain not allowed"}
	}
	days, e := intField(v, "retention_days", a.Config.RetentionDays, 1, maxDays)
	if e != nil {
		return nil, e
	}
	if days > maxDays {
		return nil, bad("保留天数超过权限限制")
	}
	m, e := a.Store.CreateMailbox(address, days, whitelist, []string{}, a.ip(r), source)
	if e != nil {
		return nil, e
	}
	return Object{"success": true, "mailbox_created": true, "mailbox_address": address, "access_token": m["access_token"], "mailbox_key": m["mailbox_key"], "created_at": m["created_at"], "expires_at": m["expires_at"], "retention_days": days}, nil
}
func (a *App) mailboxWrite(r *http.Request, action string) (any, error) {
	v, e := body(r)
	if e != nil {
		return nil, e
	}
	address, e := textField(v, "address", false)
	if e != nil {
		return nil, e
	}
	m, e := a.authMailbox(r, address, action == "toggle_mailbox_status")
	if e != nil {
		return nil, e
	}
	id := str(m["id"])
	switch action {
	case "mark_email_read", "delete_email":
		eid, e := textField(v, "email_id", true)
		if e != nil {
			return nil, e
		}
		if _, e = a.Store.Email(eid, id); e != nil {
			return nil, e
		}
		if action == "delete_email" {
			_, e = a.Store.DeleteEmails([]string{eid}, id)
			return ok(), e
		}
		read, e := boolField(v, "is_read", true)
		if e != nil {
			return nil, e
		}
		_, e = a.Store.DB.Exec("UPDATE emails SET is_read=? WHERE id=? AND mailbox_id=?", read, eid, id)
		return ok(), e
	case "delete_emails_batch":
		ids, e := listField(v, "email_ids")
		if e != nil {
			return nil, e
		}
		if len(ids) == 0 {
			return nil, bad("Email IDs are required")
		}
		n, e := a.Store.DeleteEmails(ids, id)
		return Object{"success": true, "deleted_count": n}, e
	case "mark_all_read":
		result, e := a.Store.DB.Exec("UPDATE emails SET is_read=1 WHERE mailbox_id=? AND is_read=0", id)
		if e != nil {
			return nil, e
		}
		n, e := result.RowsAffected()
		return Object{"success": true, "updated_count": n}, e
	}
	fields := Object{}
	response := ok()
	switch action {
	case "toggle_mailbox_status":
		fields["is_active"] = !truth(m["is_active"])
		response["is_active"] = fields["is_active"]
	case "toggle_whitelist":
		b, e := boolField(v, "enabled", false)
		if e != nil {
			return nil, e
		}
		fields["whitelist_enabled"] = b
		response["whitelist_enabled"] = b
		response["whitelist"] = m["sender_whitelist"]
	case "update_retention":
		days, e := intField(v, "retention_days", 0, 1, 365)
		if e != nil {
			return nil, e
		}
		if days == 0 {
			return nil, bad("retention_days is required")
		}
		fields["retention_days"] = days
		fields["expires_at"] = time.Now().Unix() + int64(days)*86400
	case "regenerate_mailbox_key":
		key, e := textField(v, "current_key", true)
		if e != nil {
			return nil, e
		}
		if !equalSecret(key, str(m["mailbox_key"])) {
			return nil, bad("Invalid current key")
		}
		fields["mailbox_key"] = mailboxKey()
		response["new_key"] = fields["mailbox_key"]
	case "add_sender_whitelist", "remove_sender_whitelist":
		sender, e := textField(v, "sender", true)
		if e != nil {
			return nil, e
		}
		if e = validateWhitelist([]string{sender}); e != nil {
			return nil, e
		}
		items := stringsValue(m["sender_whitelist"])
		if action == "add_sender_whitelist" {
			if !contains(items, sender) {
				items = append(items, sender)
			}
		} else {
			filtered := []string{}
			for _, v := range items {
				if !strings.EqualFold(v, sender) {
					filtered = append(filtered, v)
				}
			}
			items = filtered
		}
		fields["sender_whitelist"] = items
	}
	return response, a.Store.UpdateMailbox(id, fields, strings.ToUpper(action), a.ip(r))
}
