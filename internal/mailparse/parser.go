// Package mailparse 将 MIME 邮件转换为兼容 Python 存储格式的消息。
package mailparse

import (
	"bytes"
	"io"
	"mime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	"github.com/google/uuid"
)

type Message struct {
	ID          string `json:"id"`
	From        string `json:"From"`
	To          string `json:"To"`
	Subject     string `json:"Subject"`
	Timestamp   int64  `json:"Timestamp"`
	Sent        string `json:"Sent"`
	Body        string `json:"Body"`
	ContentType string `json:"ContentType"`
}

// Parse 使用接收时间，忽略 Date；可恢复的 MIME/字符集错误保留已解析内容。
// 只有生成 UUID 的系统随机源失败会返回 error。
// 与 Python 一致，深度优先遍历中最后一个内联 text/plain 或 text/html 覆盖正文，忽略文本附件。
func Parse(raw []byte, now time.Time) (*Message, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	entity := readEntity(raw)
	subject := "No Subject"
	if entity.Header.Has("Subject") {
		subject = DecodeEmailHeader(entity.Header.Get("Subject"))
	}
	result := &Message{
		ID: id.String(), From: ExtractEmailAddress(entity.Header.Get("From")),
		To: ExtractEmailAddress(entity.Header.Get("To")), Subject: subject,
		Timestamp: now.Unix(), Sent: FormatTime(now.Unix()), ContentType: "Text",
	}
	walkBody(entity, result, 0)
	return result, nil
}

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

func walkBody(root *message.Entity, result *Message, depth int) {
	// 限制嵌套转发邮件的递归，畸形输入也不会耗尽调用栈。
	if depth >= 100 {
		return
	}
	_ = root.Walk(func(_ []int, part *message.Entity, _ error) error {
		contentType, _, err := part.Header.ContentType()
		if err != nil {
			contentType = strings.ToLower(strings.TrimSpace(strings.SplitN(part.Header.Get("Content-Type"), ";", 2)[0]))
		}
		contentType = strings.ToLower(contentType)
		if strings.Count(contentType, "/") != 1 {
			contentType = "text/plain"
		}
		switch contentType {
		case "text/plain", "text/html":
			disposition := strings.TrimSpace(strings.SplitN(part.Header.Get("Content-Disposition"), ";", 2)[0])
			if strings.EqualFold(disposition, "attachment") {
				return nil
			}
			body, _ := io.ReadAll(part.Body)
			result.Body = validText(body)
			result.ContentType = "Text"
			if contentType == "text/html" {
				result.ContentType = "HTML"
			}
		case "message/rfc822", "message/global":
			raw, _ := io.ReadAll(part.Body)
			walkBody(readEntity(raw), result, depth+1)
		}
		return nil
	})
}
