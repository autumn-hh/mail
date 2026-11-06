// Package mailparse 将 MIME 邮件转换为兼容 Python 存储格式的消息。
package mailparse

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	"github.com/google/uuid"
)

var ErrLimits = errors.New("MIME resource limit exceeded")

// 解码体积、部件数量和深度均有上限，附件内容不混入 JSON 响应。
type Attachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ContentID   string `json:"content_id,omitempty"`
	Inline      bool   `json:"inline"`
	Size        int64  `json:"size"`
	Data        []byte `json:"-"`
}
type AuthResult struct {
	Result string `json:"result"`
	Domain string `json:"domain,omitempty"`
	Reason string `json:"reason,omitempty"`
}
type Authentication struct {
	SPF    AuthResult   `json:"spf"`
	DKIM   []AuthResult `json:"dkim"`
	DMARC  AuthResult   `json:"dmarc"`
	Policy string       `json:"policy,omitempty"`
}
type Details struct {
	MessageID      string          `json:"message_id,omitempty"`
	ReplyTo        []string        `json:"reply_to,omitempty"`
	To             []string        `json:"to,omitempty"`
	Cc             []string        `json:"cc,omitempty"`
	References     string          `json:"references,omitempty"`
	InReplyTo      string          `json:"in_reply_to,omitempty"`
	Date           string          `json:"date,omitempty"`
	Text           string          `json:"text,omitempty"`
	HTML           string          `json:"html,omitempty"`
	Attachments    []Attachment    `json:"attachments"`
	Authentication *Authentication `json:"authentication,omitempty"`
	Warnings       []string        `json:"warnings,omitempty"`
}

type Message struct {
	Raw         []byte  `json:"-"`
	Details     Details `json:"-"`
	ID          string  `json:"id"`
	From        string  `json:"From"`
	To          string  `json:"To"`
	Subject     string  `json:"Subject"`
	Timestamp   int64   `json:"Timestamp"`
	Sent        string  `json:"Sent"`
	Body        string  `json:"Body"`
	ContentType string  `json:"ContentType"`
}

// Parse 保留原始字节；alternative 使用最后一个可展示版本，转发邮件作为附件。
func Parse(raw []byte, now time.Time) (*Message, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	root := readEntity(raw)
	subject := "No Subject"
	if root.Header.Has("Subject") {
		subject = DecodeEmailHeader(root.Header.Get("Subject"))
	}
	result := &Message{ID: id.String(), Raw: raw, From: ExtractEmailAddress(root.Header.Get("From")),
		To: ExtractEmailAddress(root.Header.Get("To")), Subject: subject, Timestamp: now.Unix(), Sent: FormatTime(now.Unix()), ContentType: "Text"}
	result.Details = Details{MessageID: root.Header.Get("Message-ID"), ReplyTo: addresses(root.Header.Get("Reply-To")),
		To: addresses(root.Header.Get("To")), Cc: addresses(root.Header.Get("Cc")), References: root.Header.Get("References"),
		InReplyTo: root.Header.Get("In-Reply-To"), Date: root.Header.Get("Date"), Attachments: []Attachment{}}
	// 使用尚未进行字符集转换的 body，保证文本附件下载的字节与原件一致。
	if original, err := mail.ReadMessage(bytes.NewReader(raw)); err == nil {
		root = &message.Entity{Header: message.HeaderFromMap(original.Header), Body: original.Body}
		body, err := parseEntity(root.Header, root.Body, result, 0, new(int), new(int64), false)
		if err != nil {
			return nil, err
		}
		result.Details.Text, result.Details.HTML = body.text, body.html
		result.Body, result.ContentType = body.body, body.kind
	} else {
		result.Details.Text = validText(readBounded(root.Body))
		result.Body = result.Details.Text
		result.Details.Warnings = append(result.Details.Warnings, "邮件头格式不完整，已保留原文")
	}
	if result.ContentType == "" {
		result.ContentType = "Text"
	}
	return result, nil
}

func addresses(value string) []string {
	parsed, err := mail.ParseAddressList(value)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(parsed))
	for _, a := range parsed {
		out = append(out, a.Address)
	}
	return out
}

func readBounded(r io.Reader) []byte { b, _ := io.ReadAll(io.LimitReader(r, 50<<20)); return b }

func FormatTime(timestamp int64) string {
	return time.Unix(timestamp, 0).UTC().Format("Jan 02 at 15:04:05")
}

// ExtractEmailAddress 保留 Python 仅取第一对尖括号的行为，不展开收件人列表。
func ExtractEmailAddress(field string) string {
	start, end := strings.IndexByte(field, '<'), strings.IndexByte(field, '>')
	if start >= 0 && end >= 0 {
		if end < start+1 {
			return ""
		}
		return field[start+1 : end]
	}
	return strings.TrimSpace(field)
}

func DecodeEmailHeader(value string) string {
	decoder := mime.WordDecoder{CharsetReader: headerCharsetReader}
	decoded, err := decoder.DecodeHeader(value)
	if err != nil {
		return validText([]byte(value))
	}
	return validText([]byte(decoded))
}

func headerCharsetReader(charset string, input io.Reader) (io.Reader, error) {
	raw, err := io.ReadAll(input)
	if err != nil {
		return nil, err
	}
	if message.CharsetReader != nil {
		if reader, err := message.CharsetReader(charset, bytes.NewReader(raw)); err == nil {
			decoded, err := io.ReadAll(reader)
			if err == nil {
				return strings.NewReader(validText(decoded)), nil
			}
		}
	}
	// 未知字符集依次尝试 UTF-8、Latin-1，与 Python 邮件头回退一致。
	return strings.NewReader(validText(raw)), nil
}

func validText(raw []byte) string {
	if utf8.Valid(raw) {
		return string(raw)
	}
	runes := make([]rune, len(raw))
	for i, char := range raw {
		runes[i] = rune(char)
	}
	return string(runes)
}

func readEntity(raw []byte) *message.Entity {
	entity, _ := message.Read(bytes.NewReader(raw))
	if entity != nil {
		return entity
	}
	// Python 将首个非邮件头行及后续内容视为正文，并保留前面的有效邮件头。
	var headerEnd int
	for offset := 0; offset < len(raw); {
		lineEnd := bytes.IndexByte(raw[offset:], '\n')
		if lineEnd < 0 {
			lineEnd = len(raw)
		} else {
			lineEnd += offset + 1
		}
		line := bytes.TrimRight(raw[offset:lineEnd], "\r\n")
		if len(line) == 0 {
			break
		}
		continuation := line[0] == ' ' || line[0] == '\t'
		colon := bytes.IndexByte(line, ':')
		if (!continuation && colon <= 0) || (continuation && headerEnd == 0) {
			break
		}
		if !continuation {
			valid := true
			for _, char := range line[:colon] {
				if char < 33 || char > 126 {
					valid = false
					break
				}
			}
			if !valid {
				break
			}
		}
		headerEnd = lineEnd
		offset = lineEnd
	}
	if headerEnd > 0 {
		normalized := make([]byte, 0, len(raw)+4)
		normalized = append(normalized, raw[:headerEnd]...)
		if raw[headerEnd-1] != '\n' {
			normalized = append(normalized, '\r', '\n')
		}
		normalized = append(normalized, '\r', '\n')
		normalized = append(normalized, raw[headerEnd:]...)
		entity, _ = message.Read(bytes.NewReader(normalized))
	}
	if entity == nil {
		entity, _ = message.New(message.Header{}, bytes.NewReader(raw))
	}
	return entity
}

type bodySelection struct{ text, html, body, kind string }

func parseEntity(h message.Header, encoded io.Reader, result *Message, depth int, parts *int, total *int64, forcedAttachment bool) (bodySelection, error) {
	empty := bodySelection{}
	(*parts)++
	if depth > 32 || *parts > 200 {
		return empty, ErrLimits
	}
	kind, params, err := h.ContentType()
	if err != nil || !strings.Contains(kind, "/") {
		kind, params = "text/plain", map[string]string{}
	}
	kind = strings.ToLower(kind)
	disposition, dp, _ := h.ContentDisposition()
	filename := dp["filename"]
	if filename == "" {
		filename = params["name"]
	}
	attached := forcedAttachment || strings.EqualFold(disposition, "attachment") || filename != "" || kind == "message/rfc822" || kind == "message/global"
	if strings.HasPrefix(kind, "multipart/") && !attached && params["boundary"] != "" {
		mr := multipart.NewReader(encoded, params["boundary"])
		selected := empty
		index := 0
		for {
			part, err := mr.NextRawPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				result.Details.Warnings = append(result.Details.Warnings, "部分 MIME 内容无法解析，原文已保存")
				break
			}
			secondary := false
			if kind == "multipart/related" {
				if start := strings.Trim(params["start"], "<> "); start != "" {
					secondary = strings.Trim(part.Header.Get("Content-ID"), "<> ") != start
				} else {
					secondary = index > 0
				}
			}
			child, err := parseEntity(message.HeaderFromMap(part.Header), part, result, depth+1, parts, total, secondary)
			if err != nil {
				return empty, err
			}
			index++
			if child.kind == "" {
				continue
			}
			if kind == "multipart/alternative" {
				if child.text != "" {
					selected.text = child.text
				}
				if child.html != "" {
					selected.html = child.html
				}
				selected.body, selected.kind = child.body, child.kind
			} else if selected.kind == "" {
				selected = child
			} else {
				// mixed 的补充正文保留为独立附件，不覆盖主要正文。
				data := []byte(child.body)
				media := "text/plain"
				if child.kind == "HTML" {
					media = "text/html"
				}
				if err := addAttachment(result, message.Header{}, media, "", data); err != nil {
					return empty, err
				}
			}
		}
		return selected, nil
	}
	header := h.Copy()
	if attached || (kind != "text/plain" && kind != "text/html") {
		delete(params, "charset")
		header.SetContentType(kind, params)
	}
	entity, decodeErr := message.New(header, encoded)
	if entity == nil {
		return empty, errors.New("cannot decode MIME part")
	}
	if decodeErr != nil {
		result.Details.Warnings = append(result.Details.Warnings, "未知字符集或传输编码，已保留可读取内容")
	}
	data, err := io.ReadAll(io.LimitReader(entity.Body, (50<<20)-*total+1))
	*total += int64(len(data))
	if *total > 50<<20 {
		return empty, ErrLimits
	}
	if err != nil {
		result.Details.Warnings = append(result.Details.Warnings, "部分内容解码不完整，请下载原文")
	}
	if !attached && (kind == "text/plain" || kind == "text/html") {
		body := bodySelection{body: validText(data), kind: "Text"}
		if kind == "text/html" {
			body.html, body.kind = body.body, "HTML"
		} else {
			body.text = body.body
		}
		return body, nil
	}
	return empty, addAttachment(result, h, kind, filename, data)
}

func addAttachment(result *Message, h message.Header, kind, filename string, data []byte) error {
	if len(result.Details.Attachments) >= 100 {
		return ErrLimits
	}
	filename = path.Base(strings.ReplaceAll(DecodeEmailHeader(filename), "\\", "/"))
	filename = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, filename)
	if filename == "." || filename == "/" || filename == "" {
		filename = fmt.Sprintf("attachment-%d", len(result.Details.Attachments)+1)
		if kind == "message/rfc822" || kind == "message/global" {
			filename += ".eml"
		}
	}
	disposition, _, _ := h.ContentDisposition()
	result.Details.Attachments = append(result.Details.Attachments, Attachment{ID: fmt.Sprint(len(result.Details.Attachments) + 1), Filename: filename, ContentType: kind,
		ContentID: strings.Trim(h.Get("Content-ID"), "<> "), Inline: strings.EqualFold(disposition, "inline") || h.Get("Content-ID") != "", Size: int64(len(data)), Data: data})
	return nil
}
