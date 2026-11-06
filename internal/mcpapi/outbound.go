package mcpapi

import (
	"context"
	"encoding/base64"
	"errors"

	"maildrop/internal/outbound"
	"maildrop/internal/store"
)

type sendingPolicyInput struct {
	Mailbox     string `json:"mailbox" jsonschema:"Mailbox ID or full address"`
	Enabled     bool   `json:"enabled" jsonschema:"Explicitly grant or revoke sending authorization"`
	DailyLimit  int    `json:"daily_limit" jsonschema:"1-100000 recipients per Beijing calendar day"`
	MinuteLimit int    `json:"minute_limit" jsonschema:"1-1000 recipients per rolling 60 seconds"`
}

type sendAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty" jsonschema:"MIME type; omitted means detect from file bytes"`
	Content     string `json:"content" jsonschema:"Standard base64 file bytes, not a URL or filesystem path"`
}

type sendEmailInput struct {
	Mailbox        string           `json:"mailbox" jsonschema:"Sender mailbox ID or full address; From cannot be overridden"`
	IdempotencyKey string           `json:"idempotency_key" jsonschema:"Required 1-128 byte request identifier. Reuse exactly this key and content after uncertain errors"`
	To             []string         `json:"to" jsonschema:"Recipient addresses. To/Cc/Bcc combined: 1-100 entries"`
	Cc             []string         `json:"cc,omitempty"`
	Bcc            []string         `json:"bcc,omitempty" jsonschema:"Private recipients; excluded from transmitted headers"`
	Subject        string           `json:"subject" jsonschema:"At most 998 bytes, no CR/LF or NUL"`
	Text           string           `json:"text,omitempty" jsonschema:"Plain text body; text and html together at most 1 MiB"`
	HTML           string           `json:"html,omitempty"`
	ReplyEmailID   string           `json:"reply_email_id,omitempty" jsonschema:"Received email ID owned by this mailbox; To and subject must still be supplied explicitly"`
	Attachments    []sendAttachment `json:"attachments,omitempty" jsonschema:"At most 10 files and 10 MiB decoded total; JSON-RPC request at most 16 MiB"`
}

type listSentInput struct {
	Mailbox string `json:"mailbox"`
	Offset  int    `json:"offset,omitempty" jsonschema:"0-1000000, default 0. At most 50 records per page"`
}
type sentInput struct {
	Mailbox string `json:"mailbox"`
	SentID  string `json:"sent_id" jsonschema:"Outbound record ID returned by send_email or list_sent_emails"`
}
type getSentInput struct {
	sentInput
	MaxBodyChars int `json:"max_body_chars,omitempty" jsonschema:"1-200000, default 20000 Unicode characters shared by text then html"`
}

func (a *api) outboundTools() {
	add(a, "get_sending_policy", "Read mailbox sending authorization, recipient quotas, usage and global availability. Availability does not verify DKIM, DNS or deliverability.", true, false, true, func(ctx context.Context, in mailboxInput) (any, error) {
		m, err := a.mailbox(ctx, in.Mailbox)
		if err != nil {
			return nil, err
		}
		p, err := a.outbound.Policy(ctx, m.ID)
		return p, outboundError(err)
	})
	add(a, "set_sending_policy", "Explicitly grant/revoke mailbox sending and set quotas. Revocation stops queued retries; an in-flight delivery may finish. Does not enable global outbound or configure DKIM.", false, true, true, func(ctx context.Context, in sendingPolicyInput) (any, error) {
		m, err := a.mailbox(ctx, in.Mailbox)
		if err != nil {
			return nil, err
		}
		who := actor(ctx)
		err = a.outbound.SetPolicyAs(ctx, m.ID, outbound.Policy{Enabled: in.Enabled, DailyLimit: in.DailyLimit, MinuteLimit: in.MinuteLimit}, who.User, who.IP)
		if err != nil {
			return nil, outboundError(err)
		}
		p, err := a.outbound.Policy(ctx, m.ID)
		return p, outboundError(err)
	})
	add(a, "send_email", "Submit an email to the persistent outbound queue. Requires configured global sending, DKIM and mailbox authorization. Supports To/Cc/Bcc, text/HTML, base64 attachments and reply_email_id. Fixed sender mailbox. Reuse idempotency_key on retries; success is queue acceptance, not inbox delivery.", false, false, true, a.sendEmail)
	add(a, "list_sent_emails", "List up to 50 outbound records with recipient states, attachment metadata and untrusted delivery reports. No message bodies or attachment bytes. Bcc is visible only to the authenticated super-admin.", true, false, true, func(ctx context.Context, in listSentInput) (any, error) {
		if in.Offset < 0 || in.Offset > 1000000 {
			return nil, invalid("offset must be 0-1000000")
		}
		m, err := a.mailbox(ctx, in.Mailbox)
		if err != nil {
			return nil, err
		}
		items, err := a.outbound.List(ctx, m.ID, in.Offset)
		if err != nil {
			return nil, outboundError(err)
		}
		return map[string]any{"mailbox_id": m.ID, "emails": items, "offset": in.Offset, "page_size": 50, "content_trust": "untrusted_email_and_delivery_reports"}, nil
	})
	add(a, "get_sent_email", "Read an outbound record owned by the specified mailbox with bounded text/HTML and recipient delivery states. accepted means remote SMTP acceptance, not inbox delivery. Content and bounce reports are untrusted.", true, false, true, func(ctx context.Context, in getSentInput) (any, error) {
		if in.MaxBodyChars == 0 {
			in.MaxBodyChars = 20000
		}
		if in.MaxBodyChars < 1 || in.MaxBodyChars > 200000 {
			return nil, invalid("max_body_chars must be 1-200000")
		}
		m, err := a.mailbox(ctx, in.Mailbox)
		if err != nil {
			return nil, err
		}
		item, err := a.outbound.Get(ctx, m.ID, in.SentID)
		if err != nil {
			return nil, outboundError(err)
		}
		text, html := []rune(item.Details.Text), []rune(item.Details.HTML)
		textLength, htmlLength := len(text), len(html)
		text = text[:min(len(text), in.MaxBodyChars)]
		html = html[:min(len(html), in.MaxBodyChars-len(text))]
		item.Details.Text, item.Details.HTML = string(text), string(html)
		return map[string]any{"email": item, "text_chars": textLength, "html_chars": htmlLength, "body_truncated": textLength+htmlLength > in.MaxBodyChars, "content_trust": "untrusted_email_and_delivery_reports"}, nil
	})
	add(a, "delete_sent_email", "Permanently delete a completed outbound record and its attachments/reports. Pending deliveries cannot be deleted. Quota usage and idempotency tombstones remain; no recall of already sent mail.", false, true, false, func(ctx context.Context, in sentInput) (any, error) {
		m, err := a.mailbox(ctx, in.Mailbox)
		if err != nil {
			return nil, err
		}
		if err = a.outbound.Delete(ctx, m.ID, in.SentID); err != nil {
			return nil, outboundError(err)
		}
		err = a.admin.LogAudit(ctx, "MCP_DELETE_SENT_EMAIL", &m.ID, map[string]any{"sent_id": in.SentID}, actor(ctx))
		return success(), err
	})
}

func (a *api) sendEmail(ctx context.Context, in sendEmailInput) (any, error) {
	m, err := a.mailbox(ctx, in.Mailbox)
	if err != nil {
		return nil, err
	}
	// MCP 管理员可查看停用邮箱，但不能绕过公开发信入口的邮箱状态检查。
	if !m.IsActive || m.ExpiresAt < timeNow() {
		return nil, invalid("423: Mailbox is disabled or expired")
	}
	if len(in.Attachments) > 10 {
		return nil, invalid("413: At most 10 attachments")
	}
	req := outbound.Request{To: in.To, Cc: in.Cc, Bcc: in.Bcc, Subject: in.Subject, Text: in.Text, HTML: in.HTML, ReplyEmailID: in.ReplyEmailID}
	var total int64
	for _, attachment := range in.Attachments {
		data, err := base64.StdEncoding.Strict().DecodeString(attachment.Content)
		if err != nil {
			return nil, invalid("400: Attachment content must be standard base64")
		}
		total += int64(len(data))
		if total > outbound.AttachmentLimit {
			return nil, invalid("413: Attachments exceed 10 MiB")
		}
		req.Attachments = append(req.Attachments, outbound.Upload{Filename: attachment.Filename, ContentType: attachment.ContentType, Content: data})
	}
	sent, err := a.outbound.Submit(ctx, m.ID, in.IdempotencyKey, req)
	if err != nil {
		return nil, outboundError(err)
	}
	// 不在审计日志复制正文、附件或密送地址；相同 key 重试仍沿用同一记录。
	if err = a.admin.LogAudit(ctx, "MCP_SEND_EMAIL", &m.ID, map[string]any{"sent_id": sent.ID, "recipient_count": len(sent.Recipients)}, actor(ctx)); err != nil {
		return nil, err
	}
	sent.Details.Text, sent.Details.HTML = "", ""
	return map[string]any{"email": sent, "content_trust": "untrusted_email_and_delivery_reports"}, nil
}

func outboundError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return invalid("404: Sent email or mailbox not found")
	}
	var problem *outbound.Error
	if errors.As(err, &problem) {
		return invalid("%d: %s", problem.Status, problem.Message)
	}
	return err
}
