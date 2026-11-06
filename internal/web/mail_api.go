package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"time"

	"maildrop/internal/mailparse"
	"maildrop/internal/outbound"
	"maildrop/internal/store"
)

func mailError(w http.ResponseWriter, err error) {
	var problem *outbound.Error
	switch {
	case errors.Is(err, store.ErrNotFound):
		publicError(w, 404, "邮件或邮箱不存在", "")
	case errors.As(err, &problem):
		publicError(w, problem.Status, problem.Message, "")
	default:
		slog.Error("mail api failed", "error", err)
		publicError(w, 500, "邮件操作失败，请稍后重试", "")
	}
}

func (a *publicAPI) emailContent(w http.ResponseWriter, r *http.Request) {
	mailbox, ok := a.authenticate(w, r, nil, false)
	if !ok {
		return
	}
	content, err := a.server.store.ContentForEmail(r.Context(), mailbox.ID, r.PathValue("id"))
	if err != nil {
		mailError(w, err)
		return
	}
	writeJSON(w, 200, content.Details)
}
func (a *publicAPI) emailRaw(w http.ResponseWriter, r *http.Request) {
	mailbox, ok := a.authenticate(w, r, nil, false)
	if !ok {
		return
	}
	content, err := a.server.store.ContentForEmail(r.Context(), mailbox.ID, r.PathValue("id"))
	if err != nil {
		mailError(w, err)
		return
	}
	downloadMail(w, r, "message.eml", "message/rfc822", content.Raw)
}
func (a *publicAPI) emailAttachment(w http.ResponseWriter, r *http.Request) {
	mailbox, ok := a.authenticate(w, r, nil, false)
	if !ok {
		return
	}
	content, err := a.server.store.ContentForEmail(r.Context(), mailbox.ID, r.PathValue("id"))
	if err != nil {
		mailError(w, err)
		return
	}
	downloadAttachment(w, r, content.Raw)
}
func downloadAttachment(w http.ResponseWriter, r *http.Request, raw []byte) {
	// 只保存一份原文；下载时解码，附件 ID 与原解析顺序一致。
	message, err := mailparse.Parse(raw, time.Unix(0, 0))
	if err != nil {
		mailError(w, err)
		return
	}
	for _, attachment := range message.Details.Attachments {
		if attachment.ID == r.PathValue("attachment") {
			downloadMail(w, r, attachment.Filename, attachment.ContentType, attachment.Data)
			return
		}
	}
	mailError(w, store.ErrNotFound)
}
func downloadMail(w http.ResponseWriter, r *http.Request, name, kind string, data []byte) {
	w.Header().Set("Content-Type", kind)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

func (a *publicAPI) sendingPolicy(w http.ResponseWriter, r *http.Request) {
	mailbox, ok := a.authenticate(w, r, nil, false)
	if !ok {
		return
	}
	policy, err := a.server.outbound.Policy(r.Context(), mailbox.ID)
	if err != nil {
		mailError(w, err)
		return
	}
	writeJSON(w, 200, policy)
}
func (a *publicAPI) sendEmail(w http.ResponseWriter, r *http.Request) {
	mailbox, ok := a.authenticate(w, r, nil, false)
	if !ok {
		return
	}
	// 10 MiB 附件编码为 base64 后会膨胀；限制 JSON 总体积并拒绝未知发件人字段。
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	decoder.DisallowUnknownFields()
	var request outbound.Request
	err := decoder.Decode(&request)
	if err == nil {
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			err = errors.New("multiple JSON values")
		}
	}
	if err != nil {
		var size *http.MaxBytesError
		if errors.As(err, &size) {
			publicError(w, 413, "请求体过大", "")
		} else {
			publicError(w, 400, "发件请求必须是有效的 JSON，发件人固定为当前邮箱", "")
		}
		return
	}
	sent, err := a.server.outbound.Submit(r.Context(), mailbox.ID, r.Header.Get("Idempotency-Key"), request)
	if err != nil {
		mailError(w, err)
		return
	}
	writeJSON(w, 202, sent)
}
func (a *publicAPI) sentList(w http.ResponseWriter, r *http.Request) {
	mailbox, ok := a.authenticate(w, r, nil, false)
	if !ok {
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	items, err := a.server.outbound.List(r.Context(), mailbox.ID, offset)
	if err != nil {
		mailError(w, err)
		return
	}
	writeJSON(w, 200, items)
}
func (a *publicAPI) sentDetail(w http.ResponseWriter, r *http.Request) {
	mailbox, ok := a.authenticate(w, r, nil, false)
	if !ok {
		return
	}
	item, err := a.server.outbound.Get(r.Context(), mailbox.ID, r.PathValue("id"))
	if err != nil {
		mailError(w, err)
		return
	}
	writeJSON(w, 200, item)
}
func (a *publicAPI) sentRaw(w http.ResponseWriter, r *http.Request) {
	mailbox, ok := a.authenticate(w, r, nil, false)
	if !ok {
		return
	}
	raw, err := a.server.outbound.Raw(r.Context(), mailbox.ID, r.PathValue("id"))
	if err != nil {
		mailError(w, err)
		return
	}
	downloadMail(w, r, "sent.eml", "message/rfc822", raw)
}
func (a *publicAPI) sentAttachment(w http.ResponseWriter, r *http.Request) {
	mailbox, ok := a.authenticate(w, r, nil, false)
	if !ok {
		return
	}
	raw, err := a.server.outbound.Raw(r.Context(), mailbox.ID, r.PathValue("id"))
	if err != nil {
		mailError(w, err)
		return
	}
	downloadAttachment(w, r, raw)
}
func (a *publicAPI) sentDelete(w http.ResponseWriter, r *http.Request) {
	mailbox, ok := a.authenticate(w, r, nil, false)
	if !ok {
		return
	}
	if err := a.server.outbound.Delete(r.Context(), mailbox.ID, r.PathValue("id")); err != nil {
		mailError(w, err)
		return
	}
	publicSuccess(w, "发送记录已删除，额度不会恢复")
}
func (a *publicAPI) reportRaw(w http.ResponseWriter, r *http.Request) {
	mailbox, ok := a.authenticate(w, r, nil, false)
	if !ok {
		return
	}
	raw, err := a.server.outbound.ReportRaw(r.Context(), mailbox.ID, r.PathValue("id"))
	if err != nil {
		mailError(w, err)
		return
	}
	downloadMail(w, r, "delivery-report.eml", "message/rfc822", raw)
}

func (a *adminAPI) sendingPolicy(w http.ResponseWriter, r *http.Request) {
	if _, err := a.server.store.GetMailboxByID(r.PathValue("id")); err != nil {
		mailError(w, err)
		return
	}
	policy, err := a.server.outbound.Policy(r.Context(), r.PathValue("id"))
	if err != nil {
		mailError(w, err)
		return
	}
	adminData(w, policy)
}
func (a *adminAPI) setSendingPolicy(w http.ResponseWriter, r *http.Request) {
	// 只接纳主管理员 auth 中间件，子管理员令牌不能进入此路由。
	var input struct {
		Enabled     *bool `json:"enabled"`
		DailyLimit  *int  `json:"daily_limit"`
		MinuteLimit *int  `json:"minute_limit"`
	}
	ok, err := readJSON(w, r, &input)
	if err != nil || !ok || input.Enabled == nil || input.DailyLimit == nil || input.MinuteLimit == nil {
		adminError(w, 400, "必须提供 enabled、daily_limit 和 minute_limit")
		return
	}
	err = a.server.outbound.SetPolicy(r.Context(), r.PathValue("id"), outbound.Policy{Enabled: *input.Enabled, DailyLimit: *input.DailyLimit, MinuteLimit: *input.MinuteLimit}, a.server.clientIP(r))
	if err != nil {
		mailError(w, err)
		return
	}
	adminMessage(w, "发件权限与额度已保存")
}
