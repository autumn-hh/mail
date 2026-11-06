package mailparse

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestInlineAndForwardedAttachments(t *testing.T) {
	raw := []byte("From: sender@test.example\r\nContent-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: multipart/related; boundary=r; start=\"<body>\"\r\n\r\n--r\r\nContent-Type: image/png\r\nContent-ID: <logo>\r\nContent-Transfer-Encoding: base64\r\n\r\nAAH/\r\n--r\r\nContent-Type: text/html\r\nContent-ID: <body>\r\n\r\n<p>body<img src=\"cid:logo\"></p>\r\n--r--\r\n--m\r\nContent-Type: message/rfc822\r\nContent-Disposition: attachment; filename=forwarded.eml\r\n\r\nFrom: nested@test.example\r\nSubject: nested\r\n\r\nnested body\r\n--m--\r\n")
	message, err := Parse(raw, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message.Body, "cid:logo") || strings.Contains(message.Body, "nested body") || len(message.Details.Attachments) != 2 {
		t.Fatal(message)
	}
	image, forwarded := message.Details.Attachments[0], message.Details.Attachments[1]
	if image.ContentID != "logo" || !image.Inline || !bytes.Equal(image.Data, []byte{0, 1, 255}) {
		t.Fatal(image)
	}
	if forwarded.Filename != "forwarded.eml" || !bytes.Contains(forwarded.Data, []byte("nested body")) {
		t.Fatal(forwarded)
	}
	if !bytes.Equal(message.Raw, raw) {
		t.Fatal("original changed")
	}
}
func TestMIMEResourceLimits(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=x\r\n\r\n"
	for range 101 {
		raw += "--x\r\nContent-Type: application/octet-stream\r\n\r\nx\r\n"
	}
	raw += "--x--\r\n"
	if _, err := Parse([]byte(raw), time.Now()); !errors.Is(err, ErrLimits) {
		t.Fatal("attachment count limit", err)
	}
	raw = "Content-Type: text/plain\r\n\r\nbody"
	for i := range 34 {
		boundary := strings.Repeat("z", i+1)
		raw = "Content-Type: multipart/mixed; boundary=" + boundary + "\r\n\r\n--" + boundary + "\r\n" + raw + "\r\n--" + boundary + "--\r\n"
	}
	if _, err := Parse([]byte(raw), time.Now()); !errors.Is(err, ErrLimits) {
		t.Fatal("nesting limit", err)
	}
}
