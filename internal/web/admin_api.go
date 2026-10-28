package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"maildrop/internal/admin"
	"maildrop/internal/ipguard"
	"maildrop/internal/store"
)

// maxAdminPageSize 限制管理列表单页条数，避免一次请求拉取全部邮箱。
const maxAdminPageSize = 200

type adminAPI struct {
	server  *Server
	service *admin.Service
}

func (s *Server) registerAdminAPI() {
	api := &adminAPI{server: s, service: admin.New(s.cfg, s.store, s.now)}
	routes := []struct {
		pattern string
		handler http.HandlerFunc
	}{
		{"GET /api/admin/mailboxes", api.listMailboxes},
		{"POST /api/admin/mailboxes", api.createMailbox},
		{"POST /api/admin/mailboxes/batch-delete", api.batchDelete},
		{"GET /api/admin/mailboxes/{id}", api.getMailbox},
		{"PUT /api/admin/mailboxes/{id}", api.updateMailbox},
		{"DELETE /api/admin/mailboxes/{id}", api.deleteMailbox},
		{"GET /api/admin/mailboxes/{id}/audit-logs", api.auditLogs},
		{"POST /api/admin/mailboxes/{id}/reset-token", api.resetToken},
		{"POST /api/admin/mailboxes/{id}/enable", api.enableMailbox},
		{"GET /api/admin/audit-logs", api.auditLogs},
		{"GET /api/admin/stats", api.stats},
		{"GET /api/admin/source-stats", api.sourceStats},
		{"GET /api/admin/blocked-ips", api.blockedIPs},
		{"DELETE /api/admin/blocked-ips/{ip}", api.unblockIP},
		{"GET /api/admin/security-config", api.securityConfig},
		{"PUT /api/admin/security-config", api.updateSecurityConfig},
		{"GET /api/admin/sub-admins", api.subAdmins},
		{"POST /api/admin/sub-admins", api.createSubAdmin},
		{"PUT /api/admin/sub-admins/{id}", api.updateSubAdmin},
		{"DELETE /api/admin/sub-admins/{id}", api.deleteSubAdmin},
		{"GET /api/admin/domains", api.domains},
		{"POST /api/admin/domains", api.addDomain},
		{"POST /api/admin/domains/batch-add", api.batchAddDomains},
		{"POST /api/admin/domains/batch-delete", api.batchDeleteDomains},
		{"PUT /api/admin/domains/{domain}", api.updateDomain},
		{"DELETE /api/admin/domains/{domain}", api.deleteDomain},
	}
	for _, route := range routes {
		s.mux.Handle(route.pattern, api.auth(route.handler))
	}
}

func adminError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"success": false, "error": message})
}
func adminData(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": data})
}
func adminMessage(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": message})
}

func (a *adminAPI) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if message := a.authError(r); message != "" {
			adminError(w, http.StatusUnauthorized, message)
			return
		}
		next(w, r)
	}
}
func (a *adminAPI) authError(r *http.Request) string {
	s := a.server
	ip := s.clientIP(r)
	if s.cfg.Password == "" {
		return "管理员密码未配置"
	}
	if s.blocker.IsBlocked(ip) {
		remaining, _ := s.blocker.GetRemainingBlockTime(ip)
		return fmt.Sprintf("IP已被临时封禁，剩余 %d 秒", remaining)
	}
	header := r.Header.Get("Authorization")
	if header == "" {
		s.blocker.RecordFailedAttempt(ip)
		return "缺少Authorization请求头"
	}
	scheme, password, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "bearer") || password == "" {
		s.blocker.RecordFailedAttempt(ip)
		return "Authorization 请求头格式错误"
	}
	// helper 宽松处理空格；保留 Python 对原始密码空格的严格比较。
	token, ok := bearerToken(r)
	expected := sha256.Sum256([]byte(s.cfg.Password))
	provided := sha256.Sum256([]byte(token))
	equal := subtle.ConstantTimeCompare(expected[:], provided[:])
	if !ok || password != token || equal != 1 {
		if s.blocker.RecordFailedAttempt(ip) {
			return fmt.Sprintf("认证失败次数过多，IP已被封禁 %d 秒", s.blocker.GetConfig().BlockDuration)
		}
		return "认证失败"
	}
	// Python 管理 Blueprint 无额外 IP 白名单门禁；认证统一使用 clientIP。
	return ""
}
func (a *adminAPI) actor(r *http.Request) admin.Actor {
	return admin.Actor{User: "admin", IP: a.server.clientIP(r)}
}
func (a *adminAPI) internalError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("admin api request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	adminError(w, http.StatusInternalServerError, err.Error())
}
func adminObject(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	var data map[string]any
	ok, err := readJSON(w, r, &data)
	if errors.Is(err, errBodyTooLarge) {
		adminError(w, http.StatusRequestEntityTooLarge, "请求体过大")
		return nil, false
	}
	if err != nil || !ok || data == nil {
		adminError(w, http.StatusBadRequest, "无效的请求体格式，必须为 JSON 对象")
		return nil, false
	}
	return data, true
}
func adminQueryInt(r *http.Request, key string, fallback int) (int, error) {
	values, ok := r.URL.Query()[key]
	if !ok || len(values) == 0 {
		return fallback, nil
	}
	raw := values[0]
	number, err := admin.Integer(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid literal for int() with base 10: '%s'", raw)
	}
	return number, nil
}

func (a *adminAPI) listMailboxes(w http.ResponseWriter, r *http.Request) {
	// 分页参数非法时回落默认值并限制范围，不再沿用 Python 的除零 500 与负数取全量。
	page, err := adminQueryInt(r, "page", 1)
	if err != nil || page < 1 {
		page = 1
	}
	size, err := adminQueryInt(r, "page_size", 20)
	if err != nil || size < 1 {
		size = 20
	}
	size = min(size, maxAdminPageSize)
	q := r.URL.Query()
	result, err := a.service.ListMailboxes(r.Context(), admin.ListOptions{Page: page, PageSize: size, Search: q.Get("search"), Status: q.Get("status"), Source: q.Get("source")})
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	adminData(w, result)
}
func (a *adminAPI) getMailbox(w http.ResponseWriter, r *http.Request) {
	result, err := a.service.GetMailboxDetail(r.Context(), r.PathValue("id"))
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if result == nil {
		adminError(w, 404, "邮箱不存在")
		return
	}
	adminData(w, result)
}
func (a *adminAPI) createMailbox(w http.ResponseWriter, r *http.Request) {
	data, ok := adminObject(w, r)
	if !ok {
		return
	}
	days := a.server.cfg.MailboxRetentionDays
	if value := data["retention_days"]; value != nil {
		var err error
		days, err = admin.Integer(value)
		if err != nil {
			adminError(w, 400, "保留天数必须是有效的数字")
			return
		}
	}
	if !admin.Truthy(data["address"]) {
		adminError(w, 400, "邮箱地址不能为空")
		return
	}
	address, ok := data["address"].(string)
	if !ok {
		adminError(w, 400, "邮箱地址格式不正确")
		return
	}
	if err := a.service.ValidateLocalPart(address); err != nil {
		adminError(w, 400, err.Error())
		return
	}
	// 省略字段使用默认值；显式 null 仍须满足字符串列表契约。
	for _, field := range []struct{ key, message string }{
		{"sender_whitelist", "白名单必须是列表格式"},
		{"allowed_domains", "允许的域名必须是字符串列表"},
	} {
		if value, exists := data[field.key]; exists && value == nil {
			adminError(w, 400, field.message)
			return
		}
	}
	result, err := a.service.CreateMailbox(r.Context(), admin.CreateOptions{Address: address, RetentionDays: &days, SenderWhitelist: data["sender_whitelist"], AllowedDomains: data["allowed_domains"]}, a.actor(r))
	if err != nil {
		adminError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "message": "邮箱创建成功", "data": result})
}
func (a *adminAPI) updateMailbox(w http.ResponseWriter, r *http.Request) {
	data, ok := adminObject(w, r)
	if !ok {
		return
	}
	if err := a.service.UpdateMailbox(r.Context(), r.PathValue("id"), data, a.actor(r)); err != nil {
		adminError(w, 400, err.Error())
		return
	}
	adminMessage(w, "更新成功")
}
func (a *adminAPI) deleteMailbox(w http.ResponseWriter, r *http.Request) {
	soft := true
	if values, ok := r.URL.Query()["soft"]; ok && len(values) > 0 {
		soft = strings.EqualFold(values[0], "true")
	}
	if err := a.service.DeleteMailbox(r.Context(), r.PathValue("id"), soft, a.actor(r)); err != nil {
		adminError(w, 400, err.Error())
		return
	}
	if soft {
		adminMessage(w, "邮箱已禁用")
	} else {
		adminMessage(w, "邮箱已删除")
	}
}
func (a *adminAPI) batchDelete(w http.ResponseWriter, r *http.Request) {
	data, ok := adminObject(w, r)
	if !ok {
		return
	}
	values, ok := data["mailbox_ids"].([]any)
	if !ok || len(values) == 0 {
		adminError(w, 400, "未提供邮箱ID")
		return
	}
	ids := make([]string, len(values))
	for i, v := range values {
		var ok bool
		ids[i], ok = v.(string)
		if !ok {
			adminError(w, 400, "未提供邮箱ID")
			return
		}
	}
	soft := true
	if v, ok := data["soft_delete"]; ok {
		soft = admin.Truthy(v)
	}
	writeJSON(w, 200, a.service.BatchDelete(r.Context(), ids, soft, a.actor(r)))
}
func (a *adminAPI) auditLogs(w http.ResponseWriter, r *http.Request) {
	fallback := 100
	if r.PathValue("id") != "" {
		fallback = 50
	}
	limit, err := adminQueryInt(r, "limit", fallback)
	if err != nil {
		limit = fallback
	}
	limit = admin.ClampAuditLogLimit(r.PathValue("id"), limit)
	logs, err := a.service.GetAuditLogs(r.Context(), r.PathValue("id"), limit)
	// Python 审计读取失败返回空数组；记录服务端原因以便排查。
	if err != nil {
		slog.Error("read admin audit logs", "error", err)
		logs = []store.AuditLog{}
	}
	adminData(w, logs)
}
func (a *adminAPI) stats(w http.ResponseWriter, r *http.Request) {
	data, err := a.service.Stats(r.Context())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	adminData(w, data)
}
func (a *adminAPI) sourceStats(w http.ResponseWriter, r *http.Request) {
	data, err := a.service.SourceStats(r.Context())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	adminData(w, data)
}
func (a *adminAPI) resetToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	token, err := a.service.ResetToken(r.Context(), id, a.actor(r))
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "message": "令牌重置成功", "data": map[string]any{"mailbox_id": id, "new_token": token}})
}
func (a *adminAPI) enableMailbox(w http.ResponseWriter, r *http.Request) {
	if err := a.service.EnableMailbox(r.Context(), r.PathValue("id"), a.actor(r)); err != nil {
		a.internalError(w, r, err)
		return
	}
	adminMessage(w, "邮箱已恢复")
}
func (a *adminAPI) blockedIPs(w http.ResponseWriter, r *http.Request) {
	blocked := a.server.blocker.GetBlockedIPs()
	keys := make([]string, 0, len(blocked))
	for ip := range blocked {
		keys = append(keys, ip)
	}
	sort.Strings(keys)
	items := make([]map[string]any, 0, len(blocked))
	for _, ip := range keys {
		items = append(items, map[string]any{"ip": ip, "remaining_seconds": blocked[ip]})
	}
	adminData(w, map[string]any{"blocked_ips": items, "total": len(items)})
}
func (a *adminAPI) unblockIP(w http.ResponseWriter, r *http.Request) {
	ip := r.PathValue("ip")
	a.server.blocker.UnblockIP(ip)
	adminMessage(w, fmt.Sprintf("IP %s 已解除封禁", ip))
}
func (a *adminAPI) securityConfig(w http.ResponseWriter, r *http.Request) {
	adminData(w, a.server.blocker.GetConfig())
}

func (a *adminAPI) updateSecurityConfig(w http.ResponseWriter, r *http.Request) {
	// 必须直接解码 ConfigUpdate，让 bool/string/float 类型错误返回对应 400。
	var raw json.RawMessage
	ok, err := readJSON(w, r, &raw)
	if errors.Is(err, errBodyTooLarge) {
		adminError(w, 413, "请求体过大")
		return
	}
	var fields map[string]json.RawMessage
	if !ok || err != nil || json.Unmarshal(raw, &fields) != nil || fields == nil {
		adminError(w, 400, "无效的请求体格式，必须为 JSON 对象")
		return
	}
	messages := []struct {
		key, message     string
		minimum, maximum int
	}{
		{"block_duration", "封禁时长必须在60-86400秒之间", 60, 86400},
		{"max_attempts", "最大失败次数必须在1-10次之间", 1, 10},
		{"attempt_window", "尝试窗口时间必须在30-600秒之间", 30, 600},
	}
	// 按 Python 字段顺序校验类型和范围，混合错误仍报告首个字段。
	for _, field := range messages {
		if value, exists := fields[field.key]; exists {
			var number *int
			if err := json.Unmarshal(value, &number); err != nil || (number != nil && (*number < field.minimum || *number > field.maximum)) {
				adminError(w, 400, field.message)
				return
			}
		}
	}
	var update ipguard.ConfigUpdate
	if err := json.Unmarshal(raw, &update); err != nil {
		adminError(w, 400, "无效的请求体格式，必须为 JSON 对象")
		return
	}
	if err := a.server.blocker.UpdateConfig(update); err != nil {
		adminError(w, 400, err.Error())
		return
	}
	var changes map[string]any
	_ = json.Unmarshal(raw, &changes)
	if err := a.service.LogAudit(r.Context(), "UPDATE_CONFIG", nil, changes, a.actor(r)); err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "message": "配置已更新", "data": a.server.blocker.GetConfig()})
}
func (a *adminAPI) subAdmins(w http.ResponseWriter, r *http.Request) {
	items, err := a.service.ListSubAdmins()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	adminData(w, items)
}
func (a *adminAPI) subAdminError(w http.ResponseWriter, r *http.Request, err error) {
	var validation *admin.ValidationError
	if errors.As(err, &validation) {
		adminError(w, 400, err.Error())
		return
	}
	a.internalError(w, r, err)
}
func (a *adminAPI) createSubAdmin(w http.ResponseWriter, r *http.Request) {
	data, ok := adminObject(w, r)
	if !ok {
		return
	}
	result, err := a.service.CreateSubAdmin(r.Context(), data)
	if err != nil {
		a.subAdminError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "message": "子管理员创建成功", "data": result})
}
func (a *adminAPI) updateSubAdmin(w http.ResponseWriter, r *http.Request) {
	data, ok := adminObject(w, r)
	if !ok {
		return
	}
	if err := a.service.UpdateSubAdmin(r.Context(), r.PathValue("id"), data); err != nil {
		a.subAdminError(w, r, err)
		return
	}
	adminMessage(w, "子管理员更新成功")
}
func (a *adminAPI) deleteSubAdmin(w http.ResponseWriter, r *http.Request) {
	if err := a.service.DeleteSubAdmin(r.Context(), r.PathValue("id")); err != nil {
		a.internalError(w, r, err)
		return
	}
	adminMessage(w, "子管理员删除成功")
}
func (a *adminAPI) domainError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidDomain):
		adminError(w, 400, "域名格式不正确")
	case errors.Is(err, store.ErrDomainExists):
		adminError(w, 409, "域名已存在")
	case errors.Is(err, store.ErrNotFound):
		adminError(w, 404, "域名不存在")
	case errors.Is(err, store.ErrLastEnabledDomain):
		adminError(w, 409, "至少需要保留一个启用的域名")
	default:
		a.internalError(w, r, err)
	}
}
func (a *adminAPI) domains(w http.ResponseWriter, r *http.Request) {
	result, err := a.service.ListDomains()
	if err != nil {
		a.domainError(w, r, err)
		return
	}
	adminData(w, result)
}
func (a *adminAPI) addDomain(w http.ResponseWriter, r *http.Request) {
	var data map[string]any
	ok, err := readJSON(w, r, &data)
	if errors.Is(err, errBodyTooLarge) {
		adminError(w, 413, "请求体过大")
		return
	}
	value, isString := data["domain"].(string)
	if !ok || err != nil || !isString {
		adminError(w, 400, "域名格式不正确")
		return
	}
	domain, err := a.service.AddDomain(r.Context(), value, a.actor(r))
	if err != nil {
		a.domainError(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]any{"success": true, "data": domain})
}
func (a *adminAPI) updateDomain(w http.ResponseWriter, r *http.Request) {
	value, err := store.NormalizeDomain(r.PathValue("domain"))
	if err != nil {
		adminError(w, 404, "域名不存在")
		return
	}
	var data map[string]any
	ok, err := readJSON(w, r, &data)
	if errors.Is(err, errBodyTooLarge) {
		adminError(w, 413, "请求体过大")
		return
	}
	enabled, isBool := data["enabled"].(bool)
	if !ok || err != nil || !isBool {
		adminError(w, 400, "enabled必须是布尔值")
		return
	}
	domain, err := a.service.SetDomainEnabled(r.Context(), value, enabled, a.actor(r))
	if err != nil {
		a.domainError(w, r, err)
		return
	}
	adminData(w, domain)
}
func (a *adminAPI) deleteDomain(w http.ResponseWriter, r *http.Request) {
	value, err := store.NormalizeDomain(r.PathValue("domain"))
	if err != nil {
		adminError(w, 404, "域名不存在")
		return
	}
	if err := a.service.DeleteDomain(r.Context(), value, a.actor(r)); err != nil {
		a.domainError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"success": true})
}
func adminBatchDomains(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	var data map[string]any
	ok, err := readJSON(w, r, &data)
	if errors.Is(err, errBodyTooLarge) {
		adminError(w, http.StatusRequestEntityTooLarge, "请求体过大")
		return nil, false
	}
	items, isList := data["domains"].([]any)
	if !ok || err != nil || !isList {
		adminError(w, http.StatusBadRequest, "domains必须是非空字符串列表")
		return nil, false
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		value, isString := item.(string)
		if !isString {
			adminError(w, http.StatusBadRequest, "domains必须是非空字符串列表")
			return nil, false
		}
		if strings.TrimSpace(value) != "" {
			values = append(values, value)
		}
	}
	if len(values) == 0 {
		adminError(w, http.StatusBadRequest, "domains必须是非空字符串列表")
		return nil, false
	}
	if len(values) > 200 {
		adminError(w, http.StatusBadRequest, "单次最多处理 200 个域名")
		return nil, false
	}
	return values, true
}

func (a *adminAPI) batchAddDomains(w http.ResponseWriter, r *http.Request) {
	values, ok := adminBatchDomains(w, r)
	if !ok {
		return
	}
	added, skipped, err := a.service.AddDomains(r.Context(), values, a.actor(r))
	if err != nil {
		a.domainError(w, r, err)
		return
	}
	adminData(w, map[string]any{"added": added, "skipped": skipped})
}

func (a *adminAPI) batchDeleteDomains(w http.ResponseWriter, r *http.Request) {
	values, ok := adminBatchDomains(w, r)
	if !ok {
		return
	}
	deleted, failed, err := a.service.DeleteDomains(r.Context(), values, a.actor(r))
	if err != nil {
		a.domainError(w, r, err)
		return
	}
	adminData(w, map[string]any{"deleted": deleted, "failed": failed})
}
