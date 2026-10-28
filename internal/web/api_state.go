package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"maildrop/internal/addrpolicy"
	"maildrop/internal/inbox"
	"maildrop/internal/store"
)

type publicAPI struct {
	server        *Server
	service       *inbox.Service
	protected     *addrpolicy.Protected
	prefixPattern *regexp.Regexp
	domainPattern *regexp.Regexp
}

func publicError(w http.ResponseWriter, status int, text, message string) {
	data := map[string]any{"error": text}
	if message != "" {
		data["message"] = message
	}
	writeJSON(w, status, data)
}

func publicSuccess(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": message})
}

func (a *publicAPI) failed(w http.ResponseWriter, r *http.Request, err error, text, message string) {
	slog.Error("public api request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	if message == "" {
		text += ": " + err.Error()
	}
	publicError(w, http.StatusInternalServerError, text, message)
}

func (a *publicAPI) allowed(w http.ResponseWriter, r *http.Request, detailed bool) bool {
	if a.server.whitelist.IsIPWhitelisted(a.server.clientIP(r)) {
		return true
	}
	message := ""
	if detailed {
		message = "Your IP address is not allowed"
	}
	publicError(w, http.StatusForbidden, "Access denied - IP not whitelisted", message)
	return false
}

func publicObject(w http.ResponseWriter, r *http.Request, detailed bool) (map[string]json.RawMessage, bool) {
	var data map[string]json.RawMessage
	ok, err := readJSON(w, r, &data)
	if errors.Is(err, errBodyTooLarge) {
		publicError(w, http.StatusRequestEntityTooLarge, "Request body too large", "")
		return nil, false
	}
	if err != nil || !ok || len(data) == 0 {
		message := ""
		if detailed {
			message = "Request JSON is required"
		}
		publicError(w, http.StatusBadRequest, "No data provided", message)
		return nil, false
	}
	return data, true
}

// 缺省字符串与显式 null 分开，兼容凭据类型检查。
func publicString(data map[string]json.RawMessage, key string) (string, bool) {
	raw, exists := data[key]
	if !exists {
		return "", true
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	text, ok := value.(string)
	return text, ok
}

func publicTruthy(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case string:
		return value != ""
	case float64:
		return value != 0
	case []any:
		return len(value) != 0
	case map[string]any:
		return len(value) != 0
	}
	return false
}

// Python bool 是 int 子类；true 仍按一天处理，浮点和字符串保持拒绝。
func publicRetention(raw json.RawMessage) (int, bool) {
	if string(raw) == "true" {
		return 1, true
	}
	var days int
	if json.Unmarshal(raw, &days) != nil || string(raw) == "null" || days < 1 || days > 365 {
		return 0, false
	}
	return days, true
}

func publicSecretEqual(expected, supplied string) bool {
	left, right := sha256.Sum256([]byte(expected)), sha256.Sum256([]byte(supplied))
	return subtle.ConstantTimeCompare(left[:], right[:]) == 1
}

func (a *publicAPI) authenticate(w http.ResponseWriter, r *http.Request, address *string, allowDisabled bool) (*inbox.Mailbox, bool) {
	if !a.server.cfg.UseDatabase {
		publicError(w, 400, "Database storage not enabled", "Mailbox authentication requires database storage")
		return nil, false
	}
	token, ok := bearerToken(r)
	if !ok {
		publicError(w, 401, "Authentication required", "Provide an access token using the Authorization: Bearer header")
		return nil, false
	}
	mailbox, err := a.service.GetMailboxByToken(r.Context(), token)
	if errors.Is(err, store.ErrNotFound) || err == nil && mailbox == nil {
		publicError(w, 401, "Invalid access token", "The provided access token is invalid")
		return nil, false
	}
	if err != nil {
		a.failed(w, r, err, "Failed to authenticate mailbox", "Unable to authenticate mailbox")
		return nil, false
	}
	if a.server.now().Unix() > mailbox.ExpiresAt {
		publicError(w, 410, "Mailbox expired", "This mailbox has expired")
		return nil, false
	}
	if !allowDisabled && !mailbox.IsActive {
		publicError(w, 423, "Mailbox disabled", "This mailbox has been disabled")
		return nil, false
	}
	if address != nil && mailbox.Address != *address {
		publicError(w, 403, "Permission denied", "The access token does not belong to the requested mailbox")
		return nil, false
	}
	return mailbox, true
}

// 按数据库 mailbox_id 授权，不能信任邮件头 To。
func (a *publicAPI) ownedEmail(w http.ResponseWriter, r *http.Request, mailbox *inbox.Mailbox, id string) (*store.Email, bool) {
	email, err := a.server.store.GetEmailByID(id)
	if errors.Is(err, store.ErrNotFound) || err == nil && email == nil {
		publicError(w, 404, "Email not found", "The requested email does not exist")
		return nil, false
	}
	if err != nil {
		a.failed(w, r, err, "Failed to get email", "Unable to load the requested email")
		return nil, false
	}
	if email.MailboxID != mailbox.ID {
		publicError(w, 403, "Permission denied", "The email does not belong to the authenticated mailbox")
		return nil, false
	}
	return email, true
}

func (a *publicAPI) enabledDomains(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	rows, err := a.server.store.ListEnabledDomains()
	if err != nil {
		a.failed(w, r, err, "Failed to get domains", "Unable to load configured domains")
		return nil, false
	}
	if len(rows) == 0 {
		publicError(w, 503, "No domain configured", "请先在管理后台添加并启用域名")
		return nil, false
	}
	domains := make([]string, 0, len(rows))
	for _, row := range rows {
		domains = append(domains, row.Domain)
	}
	return domains, true
}

// subAdminDomains 返回子管理员可用域名：授权域名中排除域名表里已被禁用的条目。
func (a *publicAPI) subAdminDomains(w http.ResponseWriter, r *http.Request, sub *store.SubAdmin) ([]string, bool) {
	rows, err := a.server.store.ListDomains()
	if err != nil {
		a.failed(w, r, err, "Failed to create mailbox", "")
		return nil, false
	}
	disabled := make(map[string]bool, len(rows))
	for _, row := range rows {
		if !row.Enabled {
			disabled[row.Domain] = true
		}
	}
	allowed := make([]string, 0, len(sub.Domains))
	for _, candidate := range sub.Domains {
		candidate = strings.ToLower(candidate)
		if !disabled[candidate] {
			allowed = append(allowed, candidate)
		}
	}
	return allowed, true
}

func publicRandomIndex(count int) (int, error) {
	value, err := rand.Int(rand.Reader, big.NewInt(int64(count)))
	if err != nil {
		return 0, err
	}
	return int(value.Int64()), nil
}

func publicRandomPrefix() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var value [16]byte
	for i := range value {
		index, err := publicRandomIndex(len(alphabet))
		if err != nil {
			return "", err
		}
		value[i] = alphabet[index]
	}
	return string(value[:]), nil
}

func (a *publicAPI) blocked(w http.ResponseWriter, r *http.Request) bool {
	ip := a.server.clientIP(r)
	if !a.server.blocker.IsBlocked(ip) {
		return false
	}
	remaining, _ := a.server.blocker.GetRemainingBlockTime(ip)
	publicError(w, 403, "IP blocked", fmt.Sprintf("IP已被临时封禁，剩余 %d 秒", remaining))
	return true
}

func (a *publicAPI) badRegistrationAuth(w http.ResponseWriter, r *http.Request, text string) {
	if a.server.blocker.RecordFailedAttempt(a.server.clientIP(r)) {
		publicError(w, 403, "Too many failed attempts", fmt.Sprintf("认证失败次数过多，IP已被封禁 %d 秒", a.server.blocker.GetConfig().BlockDuration))
		return
	}
	publicError(w, 401, text, "")
}

// badMailboxAuth 记录一次失败尝试；状态码保持原接口语义（类型错误 400、不存在 404、密钥错误 401）。
func (a *publicAPI) badMailboxAuth(w http.ResponseWriter, r *http.Request, status int, text, message string) {
	if a.server.blocker.RecordFailedAttempt(a.server.clientIP(r)) {
		publicError(w, 403, "Too many failed attempts", fmt.Sprintf("认证失败次数过多，IP已被封禁 %d 秒", a.server.blocker.GetConfig().BlockDuration))
		return
	}
	publicError(w, status, text, message)
}

func publicContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func publicDomainDenied(w http.ResponseWriter, domains []string) {
	publicError(w, 403, "Domain not allowed", "只能使用以下域名: "+strings.Join(domains, ", "))
}

// Python str.strip 比 Unicode White_Space 多接受四个信息分隔符。
func publicStrip(value string) string {
	return strings.TrimFunc(value, func(ch rune) bool {
		return unicode.IsSpace(ch) || ch >= '\u001c' && ch <= '\u001f'
	})
}
