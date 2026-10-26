package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"maildrop/internal/addrpolicy"
	"maildrop/internal/inbox"
	"maildrop/internal/store"
)

func (s *Server) registerPublicAPI() {
	api := &publicAPI{server: s, service: inbox.New(s.cfg, s.store, s.now),
		protected:     addrpolicy.NewProtected(s.cfg.ProtectedAddresses),
		prefixPattern: regexp.MustCompile(`^[a-zA-Z0-9_]{3,20}$`),
		domainPattern: regexp.MustCompile(`^[a-zA-Z0-9.-]+$`)}
	routes := []struct {
		pattern  string
		handler  http.HandlerFunc
		detailed bool
	}{
		{"GET /api/get_random_address", api.randomAddress, false},
		{"GET /api/mailbox_info_v2", api.mailboxInfo, true},
		{"GET /api/get_domain", api.domain, false},
		{"GET /api/get_inbox", api.getInbox, true},
		{"GET /api/get_email", api.getEmail, true},
	}
	for _, route := range routes {
		s.mux.HandleFunc(route.pattern, func(w http.ResponseWriter, r *http.Request) {
			if api.allowed(w, r, route.detailed) {
				route.handler(w, r)
			}
		})
	}
	// 注册与登录先执行封禁和认证，再按 Python 顺序检查白名单。
	s.mux.HandleFunc("POST /api/register", api.register)
	s.mux.HandleFunc("POST /api/register_with_token", api.registerWithToken)
	s.mux.HandleFunc("POST /api/admin_login", api.adminLogin)
	s.mux.HandleFunc("POST /api/get_mailbox_token", api.mailboxToken)
	for _, action := range []string{"mark_email_read", "delete_email", "delete_emails_batch", "mark_all_read", "add_sender_whitelist", "remove_sender_whitelist", "update_retention", "regenerate_mailbox_key", "toggle_mailbox_status", "toggle_whitelist"} {
		s.mux.HandleFunc("POST /api/"+action, func(w http.ResponseWriter, r *http.Request) {
			if api.allowed(w, r, false) {
				api.mutate(w, r, action)
			}
		})
	}
}

func (a *publicAPI) randomAddress(w http.ResponseWriter, r *http.Request) {
	domains, ok := a.enabledDomains(w, r)
	if !ok {
		return
	}
	prefix, err := publicRandomPrefix()
	if err != nil {
		a.failed(w, r, err, "Failed to generate address", "Unable to generate mailbox address")
		return
	}
	index, err := publicRandomIndex(len(domains))
	if err != nil {
		a.failed(w, r, err, "Failed to generate address", "Unable to generate mailbox address")
		return
	}
	writeJSON(w, 200, map[string]any{"address": prefix + "@" + domains[index], "available_domains": domains})
}

func (a *publicAPI) domain(w http.ResponseWriter, r *http.Request) {
	domains, ok := a.enabledDomains(w, r)
	if ok {
		writeJSON(w, 200, map[string]any{"domain": domains[0]})
	}
}

func (a *publicAPI) mailboxToken(w http.ResponseWriter, r *http.Request) {
	if a.blocked(w, r) || !a.allowed(w, r, true) {
		return
	}
	data, ok := publicObject(w, r, true)
	if !ok {
		return
	}
	address, addressOK := publicString(data, "address")
	key, keyOK := publicString(data, "mailbox_key")
	if !addressOK || !keyOK {
		a.badMailboxAuth(w, r, 400, "Invalid credential types", "Mailbox address and key must be strings")
		return
	}
	address, key = publicStrip(address), publicStrip(key)
	if address == "" {
		publicError(w, 400, "Address is required", "Mailbox address is required")
		return
	}
	if key == "" {
		publicError(w, 400, "Mailbox key is required", "Mailbox key is required")
		return
	}
	if !strings.Contains(address, "@") {
		publicError(w, 400, "Invalid email address format", "Mailbox address is invalid")
		return
	}
	if !a.server.cfg.UseDatabase {
		publicError(w, 400, "Database storage not enabled", "This endpoint requires database storage")
		return
	}
	info, err := a.service.GetMailboxInfo(r.Context(), address)
	if err != nil {
		a.failed(w, r, err, "Failed to get access token", "Unable to authenticate mailbox")
		return
	}
	if info == nil {
		a.badMailboxAuth(w, r, 404, "Mailbox not found", "Mailbox does not exist")
		return
	}
	if !publicSecretEqual(info.MailboxKey, key) {
		a.badMailboxAuth(w, r, 401, "Invalid mailbox key", "Mailbox key is incorrect")
		return
	}
	if info.IsExpired {
		publicError(w, 410, "Mailbox has expired", "This mailbox has expired")
		return
	}
	if !info.IsActive {
		publicError(w, 423, "Mailbox disabled", "This mailbox has been disabled")
		return
	}
	// 保留历史 SQL NULL 令牌。
	raw, _ := json.Marshal(info)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	writeJSON(w, 200, map[string]any{"success": true, "address": address, "access_token": fields["access_token"], "mailbox_id": info.ID, "expires_at": info.ExpiresAt, "message": "Access token retrieved successfully"})
}

func (a *publicAPI) mailboxInfo(w http.ResponseWriter, r *http.Request) {
	mailbox, ok := a.authenticate(w, r, nil, false)
	if !ok {
		return
	}
	stats, err := a.server.store.GetMailboxStats(mailbox.ID)
	if err != nil {
		a.failed(w, r, err, "Failed to get mailbox info", "Unable to load mailbox information")
		return
	}
	raw, _ := json.Marshal(mailbox)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	writeJSON(w, 200, map[string]any{"success": true, "storage_type": "database", "mailbox": map[string]any{
		"id": mailbox.ID, "address": mailbox.Address, "created_at": mailbox.CreatedAt, "expires_at": mailbox.ExpiresAt,
		"retention_days": fields["retention_days"], "sender_whitelist": mailbox.SenderWhitelist,
		"whitelist_enabled": mailbox.WhitelistEnabled, "is_active": mailbox.IsActive, "admin_disabled": mailbox.AdminDisabled, "is_expired": false,
		"email_count": stats.TotalEmails, "unread_count": stats.UnreadEmails, "last_email_time": stats.LastEmailTime,
		"storage_used": stats.StorageUsed, "storage_limit": stats.StorageLimit,
	}})
}

func (a *publicAPI) getInbox(w http.ResponseWriter, r *http.Request) {
	address := r.URL.Query().Get("address")
	if address == "" {
		publicError(w, 400, "Address is required", "Mailbox address is required")
		return
	}
	mailbox, ok := a.authenticate(w, r, &address, false)
	if !ok {
		return
	}
	if err := a.server.store.UpdateMailboxAccess(mailbox.ID); err != nil {
		a.failed(w, r, err, "Failed to get inbox", "Unable to load mailbox messages")
		return
	}
	emails, err := a.service.GetEmailsByMailbox(r.Context(), mailbox.ID)
	if err != nil {
		a.failed(w, r, err, "Failed to get inbox", "Unable to load mailbox messages")
		return
	}
	writeJSON(w, 200, emails)
}

func (a *publicAPI) getEmail(w http.ResponseWriter, r *http.Request) {
	address, id := r.URL.Query().Get("address"), r.URL.Query().Get("id")
	if address == "" || id == "" {
		publicError(w, 400, "Missing address or email ID", "Mailbox address and email ID are required")
		return
	}
	mailbox, ok := a.authenticate(w, r, &address, false)
	if !ok {
		return
	}
	email, ok := a.ownedEmail(w, r, mailbox, id)
	if !ok {
		return
	}
	if _, err := a.service.MarkEmailAsRead(r.Context(), id); err != nil {
		a.failed(w, r, err, "Failed to get email", "Unable to load the requested email")
		return
	}
	email.IsRead = true
	writeJSON(w, 200, email)
}

func (a *publicAPI) adminLogin(w http.ResponseWriter, r *http.Request) {
	ip := a.server.clientIP(r)
	if a.server.blocker.IsBlocked(ip) {
		remaining, _ := a.server.blocker.GetRemainingBlockTime(ip)
		writeJSON(w, 403, map[string]any{"success": false, "message": fmt.Sprintf("IP已被临时封禁，剩余 %d 秒", remaining)})
		return
	}
	if !a.server.whitelist.IsIPWhitelisted(ip) {
		writeJSON(w, 403, map[string]any{"success": false, "message": "IP not whitelisted"})
		return
	}
	if a.server.cfg.Password == "" {
		writeJSON(w, 503, map[string]any{"success": false, "message": "Admin password is not configured"})
		return
	}
	var data map[string]json.RawMessage
	_, err := readJSON(w, r, &data)
	if err == errBodyTooLarge {
		writeJSON(w, 413, map[string]any{"success": false, "message": "Request body too large"})
		return
	}
	password, ok := publicString(data, "password")
	if !ok || !publicSecretEqual(a.server.cfg.Password, password) {
		status, message := 401, "Invalid password"
		if a.server.blocker.RecordFailedAttempt(ip) {
			status, message = 403, fmt.Sprintf("认证失败次数过多，IP已被封禁 %d 秒", a.server.blocker.GetConfig().BlockDuration)
		}
		writeJSON(w, status, map[string]any{"success": false, "message": message})
		return
	}
	publicSuccess(w, "Login successful")
}

func (a *publicAPI) register(w http.ResponseWriter, r *http.Request) {
	if a.blocked(w, r) {
		return
	}
	if a.server.cfg.Password == "" {
		publicError(w, 503, "Admin password is not configured", "")
		return
	}
	password := r.Header.Get("Authorization")
	if password == "" {
		a.server.blocker.RecordFailedAttempt(a.server.clientIP(r))
		publicError(w, 401, "Authentication required", "Registration requires admin password in Authorization header")
		return
	}
	if !publicSecretEqual(a.server.cfg.Password, password) {
		a.badRegistrationAuth(w, r, "Invalid admin password")
		return
	}
	if !a.allowed(w, r, false) {
		return
	}
	a.createMailbox(w, r, nil)
}

func (a *publicAPI) registerWithToken(w http.ResponseWriter, r *http.Request) {
	if a.blocked(w, r) {
		return
	}
	token := r.Header.Get("X-Sub-Admin-Token")
	if token == "" {
		a.server.blocker.RecordFailedAttempt(a.server.clientIP(r))
		publicError(w, 401, "Authentication required", "Sub-admin token required in X-Sub-Admin-Token header")
		return
	}
	if !a.server.cfg.UseDatabase {
		publicError(w, 500, "Database not enabled", "")
		return
	}
	sub, err := a.server.store.GetSubAdminByToken(token)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		a.failed(w, r, err, "Failed to create mailbox", "")
		return
	}
	if sub == nil {
		a.badRegistrationAuth(w, r, "Invalid token")
		return
	}
	if sub.IsActive == 0 {
		publicError(w, 403, "Sub-admin is disabled", "")
		return
	}
	if !a.allowed(w, r, false) {
		return
	}
	a.createMailbox(w, r, sub)
}

func (a *publicAPI) createMailbox(w http.ResponseWriter, r *http.Request, sub *store.SubAdmin) {
	data, ok := publicObject(w, r, false)
	if !ok {
		return
	}
	input, ok := publicString(data, "email")
	if !ok {
		publicError(w, 400, "Email address or prefix must be a string", "")
		return
	}
	input = publicStrip(input)
	if input == "" {
		publicError(w, 400, "Email address or prefix is required", "")
		return
	}
	address, domain := input, ""
	full := strings.Contains(input, "@")
	if full {
		parts := strings.Split(input, "@")
		if len(parts) != 2 || !a.prefixPattern.MatchString(parts[0]) || !a.domainPattern.MatchString(parts[1]) {
			publicError(w, 400, "Invalid email address format", "")
			return
		}
		// 域名不区分大小写；域名表与子管理员域名均已规范化为小写 ASCII。
		domain = strings.ToLower(parts[1])
		address = parts[0] + "@" + domain
	} else if !a.prefixPattern.MatchString(input) {
		publicError(w, 400, "Email prefix must be 3-20 characters, letters, numbers, and underscores only", "")
		return
	}
	var domains []string
	if sub != nil {
		// 子管理员沿用 Python 语义只使用自身授权域名（不要求在域名表中），
		// 但管理员在「域名设置」中明确禁用的域名对所有入口生效。
		allowed, ok := a.subAdminDomains(w, r, sub)
		if !ok {
			return
		}
		domains = allowed
	} else {
		enabled, ok := a.enabledDomains(w, r)
		if !ok {
			return
		}
		domains = enabled
	}
	if len(domains) == 0 || full && !publicContains(domains, domain) {
		publicDomainDenied(w, domains)
		return
	}
	if !full {
		index, err := publicRandomIndex(len(domains))
		if err != nil {
			a.failed(w, r, err, "Failed to create mailbox", "")
			return
		}
		address = input + "@" + domains[index]
	}
	if err := a.protected.Check(address); err != nil {
		if errors.Is(err, addrpolicy.ErrProtected) {
			publicError(w, 400, "Address is protected", "该邮箱地址受保护，无法创建")
		} else {
			a.failed(w, r, err, "Registration failed", "保护地址正则表达式无效")
		}
		return
	}
	rawDays, supplied := data["retention_days"]
	days, valid := a.server.cfg.MailboxRetentionDays, true
	if supplied {
		days, valid = publicRetention(rawDays)
	}
	if !valid || days < 1 || days > 365 {
		publicError(w, 400, "Retention days must be between 1 and 365", "")
		return
	}
	if sub != nil && days > sub.MaxRetentionDays {
		publicError(w, 400, "Retention days exceeds limit", fmt.Sprintf("保留天数不能超过 %d 天", sub.MaxRetentionDays))
		return
	}
	if !a.server.cfg.UseDatabase {
		publicError(w, 400, "Database storage not enabled. User registration requires database storage.", "")
		return
	}
	failure := "Registration failed"
	if sub != nil {
		failure = "Failed to create mailbox"
	}
	existing, err := a.service.GetMailboxInfo(r.Context(), address)
	if err != nil {
		a.failed(w, r, err, failure, "")
		return
	}
	if existing != nil && !existing.IsExpired {
		fields := map[string]any{"address": existing.Address, "created_at": existing.CreatedAt, "expires_at": existing.ExpiresAt}
		if sub == nil {
			fields["mailbox_id"] = existing.ID
		}
		writeJSON(w, 409, map[string]any{"error": "Mailbox already exists", "existing_mailbox": fields})
		return
	}
	ip, source := a.server.clientIP(r), "register"
	senders := []string{}
	if sub != nil {
		source, senders = "sub_admin_token", sub.SenderWhitelist
	}
	mailbox, err := a.service.CreateOrGetMailbox(r.Context(), address, inbox.CreateMailboxOptions{
		RetentionDays: &days, SenderWhitelist: senders, CreatedByIP: &ip, CreatedSource: source,
	})
	if err != nil {
		a.failed(w, r, err, failure, "")
		return
	}
	// 服务原子区分新建与已有记录；并发落后者不能收到先创建者的凭据。
	encoded, err := json.Marshal(mailbox)
	if err != nil {
		a.failed(w, r, err, failure, "")
		return
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &shape); err != nil {
		a.failed(w, r, err, failure, "")
		return
	}
	if _, created := shape["created_source"]; !created {
		fields := map[string]any{"address": mailbox.Address, "created_at": mailbox.CreatedAt, "expires_at": mailbox.ExpiresAt}
		if sub == nil {
			fields["mailbox_id"] = mailbox.ID
		}
		writeJSON(w, 409, map[string]any{"error": "Mailbox already exists", "existing_mailbox": fields})
		return
	}
	var retention any = days
	if string(rawDays) == "true" {
		retention = true
	}
	writeJSON(w, 201, map[string]any{"success": true, "mailbox_created": true, "mailbox_address": address,
		"access_token": mailbox.AccessToken, "mailbox_key": mailbox.MailboxKey, "created_at": mailbox.CreatedAt,
		"expires_at": mailbox.ExpiresAt, "retention_days": retention, "message": "Temporary mailbox created successfully"})
}

func (a *publicAPI) mutate(w http.ResponseWriter, r *http.Request, action string) {
	data, ok := publicObject(w, r, false)
	if !ok {
		return
	}
	address, addressOK := publicString(data, "address")
	id, idOK := publicString(data, "email_id")
	sender, senderOK := publicString(data, "sender")
	key, keyOK := publicString(data, "current_key")
	var ids []string
	days := 0
	required := ""
	validTypes := true
	switch action {
	case "mark_email_read", "delete_email":
		if !publicTruthy(data["email_id"]) {
			required = "Email ID is required"
		}
		validTypes = idOK
	case "delete_emails_batch":
		if !publicTruthy(data["email_ids"]) {
			required = "Email IDs are required"
		}
		validTypes = json.Unmarshal(data["email_ids"], &ids) == nil
		for _, emailID := range ids {
			if emailID == "" {
				validTypes = false
			}
		}
	case "add_sender_whitelist", "remove_sender_whitelist":
		if !publicTruthy(data["address"]) || !publicTruthy(data["sender"]) {
			required = "Address and sender are required"
		}
		validTypes = addressOK && senderOK
	case "update_retention":
		if !publicTruthy(data["address"]) || len(data["retention_days"]) == 0 || string(data["retention_days"]) == "null" {
			required = "Address and retention_days are required"
		}
		if required == "" {
			var valid bool
			days, valid = publicRetention(data["retention_days"])
			if !valid {
				publicError(w, 400, "Retention days must be between 1 and 365", "")
				return
			}
		}
		validTypes = addressOK
	case "regenerate_mailbox_key":
		if !publicTruthy(data["address"]) || !publicTruthy(data["current_key"]) {
			required = "Address and current_key are required"
		}
		validTypes = addressOK && keyOK
	default:
		if !publicTruthy(data["address"]) {
			required = "Address is required"
		}
		validTypes = addressOK
	}
	if required != "" {
		publicError(w, 400, required, "")
		return
	}
	if !validTypes {
		publicError(w, 400, "Invalid field types", "Request fields must have the expected types")
		return
	}
	if !a.server.cfg.UseDatabase {
		publicError(w, 400, "Database storage not enabled", "")
		return
	}
	var requested *string
	if action != "mark_email_read" && action != "delete_email" && action != "delete_emails_batch" {
		requested = &address
	}
	mailbox, ok := a.authenticate(w, r, requested, action == "toggle_mailbox_status")
	if !ok {
		return
	}
	switch action {
	case "mark_email_read", "delete_email":
		if _, ok := a.ownedEmail(w, r, mailbox, id); !ok {
			return
		}
	case "delete_emails_batch":
		// 整批授权完毕后才写入，杜绝遇到越权 ID 时部分删除。
		for _, emailID := range ids {
			if _, ok := a.ownedEmail(w, r, mailbox, emailID); !ok {
				return
			}
		}
	}
	ctx := r.Context()
	var err error
	switch action {
	case "mark_email_read":
		read := true
		if value, exists := data["is_read"]; exists {
			read = publicTruthy(value)
		}
		if read {
			_, err = a.service.MarkEmailAsRead(ctx, id)
		} else {
			_, err = a.service.MarkEmailAsUnread(ctx, id)
		}
		if err != nil {
			a.failed(w, r, err, "Failed to update email status", "")
			return
		}
		publicSuccess(w, "Email status updated")
	case "delete_email":
		count, err := a.service.DeleteEmail(ctx, id)
		if err != nil {
			a.failed(w, r, err, "Failed to delete email", "")
			return
		}
		if count == 0 {
			writeJSON(w, 500, map[string]any{"error": "Failed to delete email", "email_id": id})
			return
		}
		publicSuccess(w, "Email deleted")
	case "delete_emails_batch":
		var count int64
		for _, emailID := range ids {
			deleted, err := a.service.DeleteEmail(ctx, emailID)
			if err != nil {
				a.failed(w, r, err, "Failed to delete emails", "")
				return
			}
			count += deleted
		}
		publicSuccess(w, fmt.Sprintf("Deleted %d emails", count))
	case "mark_all_read":
		count, err := a.service.MarkAllEmailsRead(ctx, mailbox.ID)
		if err != nil {
			a.failed(w, r, err, "Failed to mark emails as read", "")
			return
		}
		publicSuccess(w, fmt.Sprintf("Marked %d emails as read", count))
	case "add_sender_whitelist", "remove_sender_whitelist":
		added := action == "add_sender_whitelist"
		var changed bool
		if added {
			changed, err = a.service.AddSenderToWhitelist(ctx, mailbox.ID, sender)
		} else {
			changed, err = a.service.RemoveSenderFromWhitelist(ctx, mailbox.ID, sender)
		}
		failure, message := "Failed to remove sender", "Sender removed from whitelist"
		if added {
			failure, message = "Failed to add sender", "Sender added to whitelist"
		}
		if err != nil {
			a.failed(w, r, err, failure, "")
			return
		}
		if !changed {
			publicError(w, 400, failure, "")
			return
		}
		publicSuccess(w, message)
	case "update_retention":
		changed, err := a.service.UpdateMailboxRetention(ctx, mailbox.ID, days)
		if err != nil {
			a.failed(w, r, err, "Failed to update retention", "")
			return
		}
		if !changed {
			publicError(w, 400, "Failed to update retention", "")
			return
		}
		publicSuccess(w, "Retention period updated")
	case "regenerate_mailbox_key":
		fresh, err := a.service.RegenerateMailboxKey(ctx, mailbox.ID, key)
		if err != nil {
			a.failed(w, r, err, "Failed to regenerate key", "")
			return
		}
		if fresh == "" {
			publicError(w, 400, "Failed to regenerate key or invalid current key", "")
			return
		}
		writeJSON(w, 200, map[string]any{"success": true, "new_key": fresh, "message": "Mailbox key regenerated"})
	case "toggle_mailbox_status":
		active := !mailbox.IsActive
		changed, err := a.service.UpdateMailboxStatus(ctx, mailbox.ID, active)
		if errors.Is(err, inbox.ErrAdminDisabled) {
			publicError(w, 423, "Mailbox disabled by administrator", "该邮箱已被管理员禁用，请联系管理员")
			return
		}
		if err != nil {
			a.failed(w, r, err, "Failed to toggle mailbox status", "")
			return
		}
		if !changed {
			publicError(w, 400, "Failed to update mailbox status", "")
			return
		}
		state := "disabled"
		if active {
			state = "enabled"
		}
		writeJSON(w, 200, map[string]any{"success": true, "is_active": active, "message": "Mailbox " + state + " successfully"})
	case "toggle_whitelist":
		enabled := publicTruthy(data["enabled"])
		changed, err := a.service.UpdateWhitelistStatus(ctx, mailbox.ID, enabled)
		if err != nil {
			a.failed(w, r, err, "Failed to toggle whitelist", "")
			return
		}
		if !changed {
			publicError(w, 400, "Failed to update whitelist status", "")
			return
		}
		state := "disabled"
		if enabled {
			state = "enabled"
		}
		var value any = false
		if raw, exists := data["enabled"]; exists {
			_ = json.Unmarshal(raw, &value)
		}
		writeJSON(w, 200, map[string]any{"success": true, "whitelist_enabled": value, "whitelist": mailbox.SenderWhitelist, "message": "Whitelist " + state + " successfully"})
	}
}
