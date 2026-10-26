package maildrop

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

func decodeChanges(raw string, out *Object) error { return json.Unmarshal([]byte(raw), out) }

func (a *App) adminRoutes() {
	a.handle("GET /api/admin/domains", true, func(r *http.Request) (any, error) { d, e := a.Store.Domains(); return success(Object{"domains": d}), e })
	a.handle("PUT /api/admin/domains", true, func(r *http.Request) (any, error) {
		v, e := body(r)
		if e != nil {
			return nil, e
		}
		d, e := listField(v, "domains")
		if e != nil {
			return nil, e
		}
		d, e = NormalizeDomains(d)
		if e != nil {
			return nil, e
		}
		if e = a.Store.SaveDomains(d, a.ip(r)); e != nil {
			return nil, e
		}
		return success(Object{"domains": d}), nil
	})
	a.handle("GET /api/admin/mailboxes", true, a.listMailboxes)
	a.handle("POST /api/admin/mailboxes", true, func(r *http.Request) (any, error) {
		v, e := body(r)
		if e != nil {
			return nil, e
		}
		address, e := textField(v, "address", true)
		if e != nil {
			return nil, e
		}
		if e = a.validateAddress(address, true); e != nil {
			return nil, e
		}
		days, e := intField(v, "retention_days", a.Config.RetentionDays, 1, a.Config.MaxRetentionDays)
		if e != nil {
			return nil, e
		}
		whitelist, e := listField(v, "sender_whitelist")
		if e != nil {
			return nil, e
		}
		if e = validateWhitelist(whitelist); e != nil {
			return nil, e
		}
		allowed, e := listField(v, "allowed_domains")
		if e != nil {
			return nil, e
		}
		if len(allowed) > 0 {
			allowed, e = NormalizeDomains(allowed)
			if e != nil {
				return nil, e
			}
		}
		m, e := a.Store.CreateMailbox(address, days, whitelist, allowed, a.ip(r), "admin")
		return success(m), e
	})
	a.handle("GET /api/admin/mailboxes/{id}", true, func(r *http.Request) (any, error) {
		m, e := a.Store.Mailbox("id", r.PathValue("id"))
		return success(m), e
	})
	a.handle("PUT /api/admin/mailboxes/{id}", true, func(r *http.Request) (any, error) {
		v, e := body(r)
		if e != nil {
			return nil, e
		}
		fields := Object{}
		if _, ok := v["retention_days"]; ok {
			n, e := intField(v, "retention_days", 0, 1, a.Config.MaxRetentionDays)
			if e != nil {
				return nil, e
			}
			fields["retention_days"] = n
			fields["expires_at"] = time.Now().Unix() + int64(n)*86400
		}
		for _, key := range []string{"is_active", "whitelist_enabled"} {
			if _, ok := v[key]; ok {
				b, e := boolField(v, key, false)
				if e != nil {
					return nil, e
				}
				fields[key] = b
			}
		}
		for _, key := range []string{"sender_whitelist", "allowed_domains"} {
			if _, ok := v[key]; ok {
				items, e := listField(v, key)
				if e != nil {
					return nil, e
				}
				if key == "sender_whitelist" {
					e = validateWhitelist(items)
				} else if len(items) > 0 {
					items, e = NormalizeDomains(items)
				}
				if e != nil {
					return nil, e
				}
				fields[key] = items
			}
		}
		return ok(), a.Store.UpdateMailbox(r.PathValue("id"), fields, "UPDATE", a.ip(r))
	})
	a.handle("DELETE /api/admin/mailboxes/{id}", true, func(r *http.Request) (any, error) {
		soft := r.URL.Query().Get("soft") != "false"
		return ok(), a.Store.DeleteMailbox(r.PathValue("id"), soft, a.ip(r))
	})
	a.handle("POST /api/admin/mailboxes/{id}/enable", true, func(r *http.Request) (any, error) {
		return ok(), a.Store.UpdateMailbox(r.PathValue("id"), Object{"is_active": true}, "RESTORE", a.ip(r))
	})
	a.handle("POST /api/admin/mailboxes/{id}/reset-token", true, func(r *http.Request) (any, error) {
		t := token()
		id := r.PathValue("id")
		if e := a.Store.UpdateMailbox(id, Object{"access_token": t}, "RESET_TOKEN", a.ip(r)); e != nil {
			return nil, e
		}
		return success(Object{"mailbox_id": id, "new_token": t}), nil
	})
	a.handle("POST /api/admin/mailboxes/batch-delete", true, func(r *http.Request) (any, error) {
		v, e := body(r)
		if e != nil {
			return nil, e
		}
		ids, e := listField(v, "mailbox_ids")
		if e != nil {
			return nil, e
		}
		if len(ids) == 0 {
			return nil, bad("未提供邮箱ID")
		}
		soft, e := boolField(v, "soft_delete", true)
		if e != nil {
			return nil, e
		}
		count := 0
		failures := []Object{}
		for _, id := range ids {
			if e := a.Store.DeleteMailbox(id, soft, a.ip(r)); e != nil {
				failures = append(failures, Object{"mailbox_id": id, "error": "删除失败"})
			} else {
				count++
			}
		}
		return Object{"success": true, "success_count": count, "failed_count": len(failures), "errors": failures}, nil
	})
	a.handle("GET /api/admin/stats", true, func(r *http.Request) (any, error) {
		m, e := one(a.Store.DB, `SELECT COUNT(*) AS total_mailboxes,COALESCE(SUM(is_active=1 AND expires_at>?),0) AS active_mailboxes,COALESCE(SUM(expires_at<=?),0) AS expired_mailboxes,COALESCE(SUM(is_active=0),0) AS disabled_mailboxes,(SELECT COUNT(*) FROM emails) AS total_emails,(SELECT COUNT(*) FROM emails WHERE is_read=0) AS unread_emails FROM mailboxes`, time.Now().Unix(), time.Now().Unix())
		return success(m), e
	})
	a.handle("GET /api/admin/source-stats", true, func(r *http.Request) (any, error) {
		rows, e := query(a.Store.DB, "SELECT COALESCE(created_source,'unknown') AS source,COUNT(*) AS count FROM mailboxes GROUP BY created_source")
		out := Object{}
		for _, row := range rows {
			out[str(row["source"])] = row["count"]
		}
		return success(out), e
	})
	for _, path := range []string{"GET /api/admin/audit-logs", "GET /api/admin/mailboxes/{id}/audit-logs"} {
		a.handle(path, true, a.auditLogs)
	}
	a.handle("GET /api/admin/blocked-ips", true, func(r *http.Request) (any, error) {
		l := a.limiter
		l.mu.Lock()
		defer l.mu.Unlock()
		rows := []Object{}
		now := time.Now().Unix()
		for ip, v := range l.entries {
			if v.Until > now {
				rows = append(rows, Object{"ip": ip, "remaining_seconds": v.Until - now})
			}
		}
		return success(Object{"blocked_ips": rows, "total": len(rows)}), nil
	})
	a.handle("DELETE /api/admin/blocked-ips/{ip}", true, func(r *http.Request) (any, error) {
		a.limiter.mu.Lock()
		delete(a.limiter.entries, r.PathValue("ip"))
		a.limiter.mu.Unlock()
		return ok(), nil
	})
	a.handle("GET /api/admin/security-config", true, func(r *http.Request) (any, error) { return success(a.limiter.config()), nil })
	a.handle("PUT /api/admin/security-config", true, func(r *http.Request) (any, error) {
		v, e := body(r)
		if e != nil {
			return nil, e
		}
		c := a.limiter.config()
		duration, e := intField(v, "block_duration", int(num(c["block_duration"])), 60, 86400)
		if e != nil {
			return nil, e
		}
		attempts, e := intField(v, "max_attempts", int(num(c["max_attempts"])), 1, 10)
		if e != nil {
			return nil, e
		}
		window, e := intField(v, "attempt_window", int(num(c["attempt_window"])), 30, 600)
		if e != nil {
			return nil, e
		}
		if e = audit(a.Store.DB, "UPDATE_CONFIG", nil, Object{"block_duration": duration, "max_attempts": attempts, "attempt_window": window}, a.ip(r)); e != nil {
			return nil, e
		}
		a.limiter.mu.Lock()
		a.limiter.Duration = duration
		a.limiter.Max = attempts
		a.limiter.Window = window
		a.limiter.mu.Unlock()
		return success(a.limiter.config()), nil
	})
	a.handle("GET /api/admin/sub-admins", true, func(r *http.Request) (any, error) {
		rows, e := query(a.Store.DB, "SELECT * FROM sub_admins ORDER BY created_at DESC")
		for _, v := range rows {
			v["domains"] = stringsValue(v["domains"])
			v["sender_whitelist"] = stringsValue(v["sender_whitelist"])
			v["is_active"] = truth(v["is_active"])
			delete(v, "password_hash")
		}
		return success(rows), e
	})
	a.handle("POST /api/admin/sub-admins", true, func(r *http.Request) (any, error) { return a.saveSubAdmin(r, true) })
	a.handle("PUT /api/admin/sub-admins/{id}", true, func(r *http.Request) (any, error) { return a.saveSubAdmin(r, false) })
	a.handle("DELETE /api/admin/sub-admins/{id}", true, func(r *http.Request) (any, error) {
		_, e := a.Store.DB.Exec("DELETE FROM sub_admins WHERE id=?", r.PathValue("id"))
		return ok(), e
	})
}
func (a *App) listMailboxes(r *http.Request) (any, error) {
	page, e := queryInt(r, "page", 1, 1, 1000000)
	if e != nil {
		return nil, e
	}
	size, e := queryInt(r, "page_size", 20, 1, 1000)
	if e != nil {
		return nil, e
	}
	where := []string{"1=1"}
	args := []any{}
	if q := r.URL.Query().Get("search"); q != "" {
		where = append(where, "(m.address LIKE ? OR m.id LIKE ?)")
		args = append(args, "%"+q+"%", "%"+q+"%")
	}
	switch r.URL.Query().Get("status") {
	case "active":
		where = append(where, "m.is_active=1 AND m.expires_at>?")
		args = append(args, time.Now().Unix())
	case "expired":
		where = append(where, "m.expires_at<=?")
		args = append(args, time.Now().Unix())
	case "disabled":
		where = append(where, "m.is_active=0")
	}
	if source := r.URL.Query().Get("source"); source != "" && source != "all" {
		where = append(where, "m.created_source=?")
		args = append(args, source)
	}
	clause := " WHERE " + strings.Join(where, " AND ")
	count, e := one(a.Store.DB, "SELECT COUNT(*) AS total FROM mailboxes m"+clause, args...)
	if e != nil {
		return nil, e
	}
	args = append(args, size, (page-1)*size)
	rows, e := query(a.Store.DB, mailboxSelect+clause+" ORDER BY m.created_at DESC,m.id LIMIT ? OFFSET ?", args...)
	if e != nil {
		return nil, e
	}
	for i, m := range rows {
		rows[i] = publicMailbox(normalizeMailbox(m))
	}
	total := num(count["total"])
	return success(Object{"mailboxes": rows, "total": total, "page": page, "page_size": size, "total_pages": (total + int64(size) - 1) / int64(size)}), nil
}
func (a *App) auditLogs(r *http.Request) (any, error) {
	limit, e := queryInt(r, "limit", 50, 1, 1000)
	if e != nil {
		return nil, e
	}
	statement := "SELECT a.*,m.address AS mailbox_address FROM audit_logs a LEFT JOIN mailboxes m ON m.id=a.mailbox_id"
	args := []any{}
	if id := r.PathValue("id"); id != "" {
		statement += " WHERE a.mailbox_id=?"
		args = append(args, id)
	}
	args = append(args, limit)
	rows, e := query(a.Store.DB, statement+" ORDER BY a.timestamp DESC LIMIT ?", args...)
	for _, m := range rows {
		changes := Object{}
		if raw := str(m["changes"]); raw != "" {
			if err := decodeChanges(raw, &changes); err != nil {
				changes = Object{}
			}
		}
		delete(changes, "new_token")
		delete(changes, "access_token")
		delete(changes, "mailbox_key")
		m["changes"] = changes
	}
	return success(rows), e
}
func (a *App) saveSubAdmin(r *http.Request, create bool) (any, error) {
	v, e := body(r)
	if e != nil {
		return nil, e
	}
	id := r.PathValue("id")
	old := Object{}
	if !create {
		old, e = one(a.Store.DB, "SELECT * FROM sub_admins WHERE id=?", id)
		if e != nil {
			return nil, e
		}
	}
	domains := stringsValue(old["domains"])
	if _, ok := v["domains"]; ok || create {
		domains, e = listField(v, "domains")
		if e != nil {
			return nil, e
		}
		domains, e = NormalizeDomains(domains)
		if e != nil {
			return nil, e
		}
		available, e := a.Store.Domains()
		if e != nil {
			return nil, e
		}
		for _, d := range domains {
			if !contains(available, d) {
				return nil, bad("只能分配当前可用域名")
			}
		}
	}
	whitelist := stringsValue(old["sender_whitelist"])
	if _, ok := v["sender_whitelist"]; ok {
		whitelist, e = listField(v, "sender_whitelist")
		if e != nil {
			return nil, e
		}
		if e = validateWhitelist(whitelist); e != nil {
			return nil, e
		}
	}
	fallback := 30
	if !create {
		fallback = int(num(old["max_retention_days"]))
	}
	days, e := intField(v, "max_retention_days", fallback, 1, a.Config.MaxRetentionDays)
	if e != nil {
		return nil, e
	}
	active, e := boolField(v, "is_active", create || truth(old["is_active"]))
	if e != nil {
		return nil, e
	}
	notes := str(old["notes"])
	if _, ok := v["notes"]; ok {
		notes, e = textField(v, "notes", false)
		if e != nil {
			return nil, e
		}
		if len(notes) > 4000 {
			return nil, bad("备注过长")
		}
	}
	if create {
		t, e := textField(v, "token", true)
		if e != nil {
			return nil, e
		}
		if len(t) > 256 {
			return nil, bad("Token 过长")
		}
		id = token()
		// 兼容早期带 NOT NULL password_hash 的子管理员表，保留原列和数据。
		columns, err := query(a.Store.DB, "PRAGMA table_info(sub_admins)")
		if err != nil {
			return nil, err
		}
		extraColumn, extraValue := "", ""
		for _, column := range columns {
			if column["name"] == "password_hash" {
				extraColumn, extraValue = ",password_hash", ",''"
			}
		}
		_, e = a.Store.DB.Exec("INSERT INTO sub_admins(id,token,domains,sender_whitelist,max_retention_days,created_at,updated_at,is_active,created_by,notes"+extraColumn+") VALUES(?,?,?,?,?,?,?,?,?,?"+extraValue+")", id, t, jsonText(domains), jsonText(whitelist), days, time.Now().Unix(), time.Now().Unix(), active, "admin", notes)
		if e != nil && strings.Contains(e.Error(), "UNIQUE") {
			return nil, bad("Token 已存在")
		}
		return success(Object{"id": id, "token": t}), e
	}
	_, e = a.Store.DB.Exec("UPDATE sub_admins SET domains=?,sender_whitelist=?,max_retention_days=?,updated_at=?,is_active=?,notes=? WHERE id=?", jsonText(domains), jsonText(whitelist), days, time.Now().Unix(), active, notes, id)
	return ok(), e
}
