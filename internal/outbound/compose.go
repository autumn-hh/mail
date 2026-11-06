package outbound

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"net/textproto"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/emersion/go-msgauth/dkim"
	"golang.org/x/net/idna"
	"maildrop/internal/mailparse"
)

func normalize(req *Request) ([]string, error) {
	if !utf8.ValidString(req.Subject) || strings.ContainsAny(req.Subject, "\r\n\x00") || len(req.Subject) > 998 {
		return nil, problem(400, "主题过长或包含换行")
	}
	if len(req.Text)+len(req.HTML) > 1<<20 {
		return nil, problem(413, "正文最多1 MiB")
	}
	if len(req.Attachments) > 10 {
		return nil, problem(413, "附件最多10个")
	}
	if len(req.To)+len(req.Cc)+len(req.Bcc) > 100 {
		return nil, problem(400, "单封最多100个收件人")
	}
	recipients := []string{}
	seen := map[string]bool{}
	for _, list := range []*[]string{&req.To, &req.Cc, &req.Bcc} {
		clean := []string{}
		for _, value := range *list {
			if strings.ContainsAny(value, "\r\n") {
				return nil, problem(400, "收件地址无效")
			}
			addr, err := mail.ParseAddress(strings.TrimSpace(value))
			if err != nil {
				return nil, problem(400, "收件地址无效")
			}
			local, domain, ok := strings.Cut(addr.Address, "@")
			if !ok || local == "" || strings.ContainsAny(local, ` <>\"()`) {
				return nil, problem(400, "收件地址无效")
			}
			for _, r := range local {
				if r > 127 || unicode.IsControl(r) {
					return nil, problem(400, "首版仅支持 ASCII 邮箱用户名")
				}
			}
			domain, err = idna.Lookup.ToASCII(strings.TrimSuffix(domain, "."))
			if err != nil || !validDomain(domain) {
				return nil, problem(400, "收件域名无效")
			}
			address := local + "@" + strings.ToLower(domain)
			if len(address) > 254 {
				return nil, problem(400, "收件地址过长")
			}
			if !seen[address] {
				seen[address] = true
				clean = append(clean, address)
				recipients = append(recipients, address)
			}
		}
		*list = clean
	}
	if len(recipients) == 0 {
		return nil, problem(400, "请填写收件人")
	}
	var size int64
	for i := range req.Attachments {
		a := &req.Attachments[i]
		a.ID = ""
		a.Size = 0
		a.Filename = path.Base(strings.ReplaceAll(a.Filename, "\\", "/"))
		if a.Filename == "" || a.Filename == "." || a.Filename == "/" || len(a.Filename) > 240 || strings.ContainsAny(a.Filename, "\r\n\x00") {
			return nil, problem(400, "附件文件名无效")
		}
		size += int64(len(a.Content))
		if size > AttachmentLimit {
			return nil, problem(413, "附件合计最多10 MiB")
		}
		if a.ContentType == "" {
			a.ContentType = http.DetectContentType(a.Content)
		}
		if len(a.ContentType) > 255 {
			return nil, problem(400, "附件类型过长")
		}
		kind, _, err := mime.ParseMediaType(a.ContentType)
		if err != nil || !strings.Contains(kind, "/") {
			return nil, problem(400, "附件类型无效")
		}
		a.ContentType = kind
	}
	return recipients, nil
}
func validDomain(domain string) bool {
	if len(domain) > 253 || !strings.Contains(domain, ".") {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
func compose(from, messageID string, req Request, reply *mailparse.Details, now time.Time) ([]byte, error) {
	if strings.ContainsAny(from, "\r\n") || !validDomain(domainOf(from)) {
		return nil, problem(400, "发件邮箱域名无效")
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", (&mail.Address{Address: from}).String())
	addressList := func(addresses []string) string {
		out := make([]string, len(addresses))
		for i, a := range addresses {
			out[i] = (&mail.Address{Address: a}).String()
		}
		return strings.Join(out, ",\r\n ")
	}
	if len(req.To) > 0 {
		fmt.Fprintf(&b, "To: %s\r\n", addressList(req.To))
	} else {
		b.WriteString("To: undisclosed-recipients:;\r\n")
	}
	if len(req.Cc) > 0 {
		fmt.Fprintf(&b, "Cc: %s\r\n", addressList(req.Cc))
	}
	fmt.Fprintf(&b, "Subject: %s\r\nDate: %s\r\nMessage-ID: %s\r\nMIME-Version: 1.0\r\n", encodedSubject(req.Subject), now.Format(time.RFC1123Z), messageID)
	if reply != nil {
		// Message-ID 应只有一个值；畸形头不应生成超长或多值的 In-Reply-To。
		if id := safeMessageIDs(reply.MessageID); id != "" && !strings.Contains(id, " ") {
			fmt.Fprintf(&b, "In-Reply-To: %s\r\n", id)
			refs := safeMessageIDs(reply.References + " " + id)
			if len(refs) > 900 {
				refs = id
			}
			fmt.Fprintf(&b, "References: %s\r\n", refs)
		}
	}
	if len(req.Attachments) == 0 {
		if err := writeBody(&b, req); err != nil {
			return nil, err
		}
	} else {
		mixed := multipart.NewWriter(&b)
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", mixed.Boundary())
		var body bytes.Buffer
		if err := writeBody(&body, req); err != nil {
			return nil, err
		}
		parsed, err := mail.ReadMessage(&body)
		if err != nil {
			return nil, err
		}
		w, err := mixed.CreatePart(textproto.MIMEHeader(parsed.Header))
		if err != nil {
			return nil, err
		}
		if _, err = io.Copy(w, parsed.Body); err != nil {
			return nil, err
		}
		for _, a := range req.Attachments {
			h := textproto.MIMEHeader{"Content-Type": {mime.FormatMediaType(a.ContentType, map[string]string{"name": a.Filename})}, "Content-Disposition": {mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename})}, "Content-Transfer-Encoding": {"base64"}}
			w, err := mixed.CreatePart(h)
			if err != nil {
				return nil, err
			}
			encoded := base64.StdEncoding.EncodeToString(a.Content)
			for len(encoded) > 0 {
				n := min(76, len(encoded))
				if _, err = io.WriteString(w, encoded[:n]+"\r\n"); err != nil {
					return nil, err
				}
				encoded = encoded[n:]
			}
		}
		if err := mixed.Close(); err != nil {
			return nil, err
		}
	}
	return b.Bytes(), nil
}
func writeBody(w io.Writer, req Request) error {
	if req.HTML == "" || req.Text == "" {
		kind, body := "text/plain", req.Text
		if req.HTML != "" {
			kind, body = "text/html", req.HTML
		}
		if _, err := fmt.Fprintf(w, "Content-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", kind); err != nil {
			return err
		}
		qp := quotedprintable.NewWriter(w)
		if _, err := io.WriteString(qp, body); err != nil {
			return err
		}
		return qp.Close()
	}
	alternative := multipart.NewWriter(w)
	if _, err := fmt.Fprintf(w, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", alternative.Boundary()); err != nil {
		return err
	}
	for _, part := range []struct{ kind, body string }{{"text/plain", req.Text}, {"text/html", req.HTML}} {
		out, err := alternative.CreatePart(textproto.MIMEHeader{"Content-Type": {part.kind + "; charset=utf-8"}, "Content-Transfer-Encoding": {"quoted-printable"}})
		if err != nil {
			return err
		}
		qp := quotedprintable.NewWriter(out)
		if _, err = io.WriteString(qp, part.body); err != nil {
			return err
		}
		if err = qp.Close(); err != nil {
			return err
		}
	}
	return alternative.Close()
}
func encodedSubject(subject string) string {
	parts := []string{}
	for len(subject) > 0 {
		n := min(42, len(subject))
		for n > 0 && !utf8.ValidString(subject[:n]) {
			n--
		}
		if n == 0 {
			return ""
		}
		parts = append(parts, "=?UTF-8?B?"+base64.StdEncoding.EncodeToString([]byte(subject[:n]))+"?=")
		subject = subject[n:]
	}
	return strings.Join(parts, "\r\n ")
}
func safeMessageIDs(value string) string {
	out := []string{}
	for _, id := range strings.Fields(value) {
		if len(id) > 2 && len(id) < 255 && strings.HasPrefix(id, "<") && strings.HasSuffix(id, ">") && !strings.ContainsAny(id, "\r\n") {
			out = append(out, id)
		}
	}
	return strings.Join(out, " ")
}
func (s *Service) signMessage(from string, raw []byte) ([]byte, error) {
	domain := domainOf(from)
	if !validDomain(domain) || !validDomain(s.cfg.DKIMSelector+".invalid") {
		return nil, fmt.Errorf("invalid signing domain or selector")
	}
	encoded, err := os.ReadFile(filepath.Join(s.cfg.DKIMKeyDir, domain+".pem"))
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(encoded)
	if block == nil {
		return nil, fmt.Errorf("invalid DKIM PEM")
	}
	var signer crypto.Signer
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		signer, _ = key.(crypto.Signer)
	} else if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		signer = key
	}
	if signer == nil {
		return nil, fmt.Errorf("invalid DKIM key")
	}
	var result bytes.Buffer
	err = dkim.Sign(&result, bytes.NewReader(raw), &dkim.SignOptions{Domain: domain, Selector: s.cfg.DKIMSelector, Signer: signer, HeaderCanonicalization: dkim.CanonicalizationRelaxed, BodyCanonicalization: dkim.CanonicalizationRelaxed, HeaderKeys: []string{"From", "To", "Cc", "Subject", "Date", "Message-ID", "MIME-Version", "Content-Type", "Content-Transfer-Encoding", "In-Reply-To", "References"}})
	return result.Bytes(), err
}
