package mailparse

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/quotedprintable"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

func TestParseBodies(t *testing.T) {
	for _, tc := range []struct {
		name, raw, body, contentType string
	}{
		{"plain text", "Content-Type: text/plain; charset=utf-8\r\n\r\nhello\r\nworld\r\n", "hello\r\nworld\r\n", "Text"},
		{"html only", "Content-Type: text/html; charset=UTF-8\r\n\r\n<p>Hello</p>", "<p>Hello</p>", "HTML"},
		{"default plain", "Subject: body\n\nhello", "hello", "Text"},
		{"alternative html last", "Content-Type: multipart/alternative; boundary=alt\r\n\r\n--alt\r\nContent-Type: text/plain\r\n\r\nplain\r\n--alt\r\nContent-Type: text/html\r\n\r\n<b>html</b>\r\n--alt--\r\n", "<b>html</b>", "HTML"},
		{"alternative plain last", "Content-Type: multipart/alternative; boundary=alt\n\n--alt\nContent-Type: text/html\n\n<b>html</b>\n--alt\nContent-Type: text/plain\n\nlast plain\n--alt--\n", "last plain", "Text"},
		{"nested multipart binary attachment", "Content-Type: multipart/mixed; boundary=outer\r\n\r\n--outer\r\nContent-Type: multipart/alternative; boundary=inner\r\n\r\n--inner\r\nContent-Type: text/plain\r\n\r\nplain\r\n--inner\r\nContent-Type: text/html\r\n\r\n<p>nested</p>\r\n--inner--\r\n--outer\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=blob.bin\r\nContent-Transfer-Encoding: base64\r\n\r\nAAECAw==\r\n--outer--\r\n", "<p>nested</p>", "HTML"},
		{"text attachment preserves body", "Content-Type: multipart/mixed; boundary=outer\r\n\r\n--outer\r\nContent-Type: text/html\r\n\r\n<p>body</p>\r\n--outer\r\nContent-Type: text/plain; name=notes.txt\r\nContent-Disposition: attachment; filename=notes.txt\r\n\r\nattachment text\r\n--outer--\r\n", "<p>body</p>", "HTML"},
		{"text attachment only", "Content-Type: text/plain\r\nContent-Disposition: ATTACHMENT; filename=notes.txt\r\n\r\nattachment", "", "Text"},
		{"html attachment preserves inline", "Content-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: text/plain\r\nContent-Disposition: inline\r\n\r\nbody\r\n--m\r\nContent-Type: text/html\r\nContent-Disposition: attachment\r\n\r\n<p>attachment</p>\r\n--m--\r\n", "body", "Text"},
		{"binary only", "Content-Type: application/octet-stream\r\n\r\nignored", "", "Text"},
		{"base64", "Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\naGVsbG8g5LiW55WM", "hello 世界", "Text"},
		{"base64 whitespace", "Content-Transfer-Encoding: base64\r\n\r\n aGVs bG8=\r\n", "hello", "Text"},
		{"quoted printable", "Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nhello=20=E4=B8=96=\r\n=E7=95=8C", "hello 世界", "Text"},
		{"attached forwarded message", "Content-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: text/plain\r\n\r\nouter\r\n--m\r\nContent-Type: message/rfc822\r\nContent-Disposition: attachment\r\n\r\nSubject: forwarded\r\nContent-Type: text/html\r\n\r\n<p>forwarded</p>\r\n--m--\r\n", "<p>forwarded</p>", "HTML"},
		{"invalid content type defaults to text", "Content-Type: invalid\r\n\r\nfallback body", "fallback body", "Text"},
		{"unknown charset falls back UTF8", "Content-Type: text/plain; charset=x-does-not-exist\r\n\r\nhello 世界", "hello 世界", "Text"},
		{"unknown charset falls back Latin1", "Content-Type: text/plain; charset=x-does-not-exist\r\n\r\ncaf\xe9", "café", "Text"},
		{"unknown transfer encoding preserves body", "Content-Transfer-Encoding: x-does-not-exist\r\n\r\nraw body", "raw body", "Text"},
		{"garbage", "this is not an email\x00\xff", "this is not an email\x00ÿ", "Text"},
		{"malformed headers keep body and subject", "Subject: kept\r\nnot-a-header\r\ntext", "not-a-header\r\ntext", "Text"},
		{"empty", "", "", "Text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := Parse([]byte(tc.raw), time.Unix(1704067200, 0))
			if err != nil {
				t.Fatal(err)
			}
			if msg.Body != tc.body || msg.ContentType != tc.contentType {
				t.Fatalf("body/type = (%q, %q), want (%q, %q)", msg.Body, msg.ContentType, tc.body, tc.contentType)
			}
			if !utf8.ValidString(msg.Body) {
				t.Fatal("invalid UTF-8 body")
			}
			if tc.name == "malformed headers keep body and subject" && msg.Subject != "kept" {
				t.Fatalf("subject = %q", msg.Subject)
			}
		})
	}
}

func TestParseCharsets(t *testing.T) {
	for _, tc := range []struct {
		charset, text string
		encoding      encoding.Encoding
	}{
		{"GBK", "中文邮件", simplifiedchinese.GBK},
		{"GB2312", "中文邮件", simplifiedchinese.GBK},
		{"GB18030", "中文邮件𠀀", simplifiedchinese.GB18030},
		{"Big5", "中文郵件", traditionalchinese.Big5},
		{"ISO-8859-1", "café", charmap.ISO8859_1},
		{"ISO-8859-2", "Zażółć", charmap.ISO8859_2},
		{"ISO-8859-15", "€ café", charmap.ISO8859_15},
		{"UTF-8", "世界 🌍", nil},
	} {
		for _, transfer := range []string{"8bit", "base64", "quoted-printable"} {
			t.Run(tc.charset+"/"+transfer, func(t *testing.T) {
				encoded := []byte(tc.text)
				if tc.encoding != nil {
					var err error
					encoded, err = tc.encoding.NewEncoder().Bytes(encoded)
					if err != nil {
						t.Fatal(err)
					}
				}
				subject := "=?" + tc.charset + "?B?" + base64.StdEncoding.EncodeToString(encoded) + "?="
				body := encoded
				switch transfer {
				case "base64":
					body = []byte(base64.StdEncoding.EncodeToString(encoded))
				case "quoted-printable":
					var buffer bytes.Buffer
					writer := quotedprintable.NewWriter(&buffer)
					if _, err := writer.Write(encoded); err != nil {
						t.Fatal(err)
					}
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
					body = buffer.Bytes()
				}
				raw := []byte(fmt.Sprintf("Subject: %s\r\nContent-Type: text/plain; charset=%s\r\nContent-Transfer-Encoding: %s\r\n\r\n", subject, tc.charset, transfer))
				raw = append(raw, body...)
				msg, err := Parse(raw, time.Unix(1704067200, 0))
				if err != nil {
					t.Fatal(err)
				}
				if msg.Subject != tc.text || msg.Body != tc.text {
					t.Fatalf("subject/body = (%q, %q), want %q", msg.Subject, msg.Body, tc.text)
				}
			})
		}
	}
}

func TestParseHeadersAndTime(t *testing.T) {
	now := time.Date(2026, 10, 26, 8, 9, 10, 999000000, time.FixedZone("UTC+8", 8*60*60))
	for _, date := range []string{"", "Date: Tue, 01 Jan 1991 01:02:03 +0000\r\n", "Date: not-a-date\r\n"} {
		t.Run("date="+date, func(t *testing.T) {
			raw := "From: =?UTF-8?B?5byg5LiJ?= <sender@example.com>\r\nTo: Recipient <to@example.com>\r\nSubject: =?UTF-8?Q?hello_=E4=B8=96=E7=95=8C?=\r\n" + date + "\r\nbody"
			msg, err := Parse([]byte(raw), now)
			if err != nil {
				t.Fatal(err)
			}
			if msg.From != "sender@example.com" || msg.To != "to@example.com" || msg.Subject != "hello 世界" {
				t.Fatalf("parsed headers = %+v", msg)
			}
			if msg.Timestamp != now.Unix() || msg.Sent != "Oct 26 at 00:09:10" {
				t.Fatalf("timestamp/sent = (%d, %q)", msg.Timestamp, msg.Sent)
			}
			id, err := uuid.Parse(msg.ID)
			if err != nil || id.Version() != 4 {
				t.Fatalf("invalid UUID4 %q: %v", msg.ID, err)
			}
			encoded, err := json.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			wantKeys := []string{"id", "From", "To", "Subject", "Timestamp", "Sent", "Body", "ContentType"}
			if len(got) != len(wantKeys) {
				t.Fatalf("JSON keys = %s", encoded)
			}
			for _, key := range wantKeys {
				if _, ok := got[key]; !ok {
					t.Errorf("missing key %s", key)
				}
			}
		})
	}
	for _, tc := range []struct{ raw, subject string }{
		{"\r\nbody", "No Subject"},
		{"Subject:\r\n\r\nbody", ""},
		{"Subject: first\r\nSubject: second\r\n\r\nbody", "first"},
	} {
		msg, err := Parse([]byte(tc.raw), now)
		if err != nil {
			t.Fatal(err)
		}
		if msg.Subject != tc.subject {
			t.Fatalf("subject = %q, want %q", msg.Subject, tc.subject)
		}
	}
	first, err := Parse(nil, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Parse(nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("message IDs were reused")
	}
}

func TestDecodeEmailHeader(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"", ""},
		{"plain subject", "plain subject"},
		{"=?UTF-8?B?5byg5LiJ?=", "张三"},
		{"=?UTF-8?Q?hello_=E4=B8=96=E7=95=8C?=", "hello 世界"},
		{"=?UTF-8?B?5LiW?=\r\n =?UTF-8?B?55WM?=", "世界"},
		{"=?GBK?B?1tDOxA==?=", "中文"},
		{"=?unknown-charset?B?5LiW55WM?=", "世界"},
		{"=?unknown-charset?B?Y2Fm6Q==?=", "café"},
		{"=?ISO-8859-1?Q?caf=E9?=", "café"},
		{"=?UTF-8?B?broken", "=?UTF-8?B?broken"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if got := DecodeEmailHeader(tc.raw); got != tc.want {
				t.Fatalf("DecodeEmailHeader = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractEmailAddress(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"Hailey Thomas <me@haileyy.dev>", "me@haileyy.dev"},
		{"=?UTF-8?B?5byg5LiJ?= <sender@example.com>", "sender@example.com"},
		{" to@example.com ", "to@example.com"},
		{"A <one@example.com>, B <two@example.com>", "one@example.com"},
		{"one@example.com, two@example.com", "one@example.com, two@example.com"},
		{"bad > order <", ""},
		{"< spaces@example.com >", " spaces@example.com "},
		{"", ""},
	} {
		if got := ExtractEmailAddress(tc.raw); got != tc.want {
			t.Errorf("ExtractEmailAddress(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestFormatTime(t *testing.T) {
	if got := FormatTime(0); got != "Jan 01 at 00:00:00" {
		t.Fatalf("FormatTime = %q", got)
	}
	if got := FormatTime(1704153599); got != "Jan 01 at 23:59:59" {
		t.Fatalf("FormatTime = %q", got)
	}
}

func TestParseMalformedInput(t *testing.T) {
	for _, raw := range [][]byte{
		nil,
		{0, 255, 254, 128},
		[]byte("Subject: ok\r\n\x00broken: header\r\n\r\nbody"),
		[]byte("Content-Type: multipart/mixed\r\n\r\nmissing boundary"),
		[]byte("Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nbad header\r\n\r\nbody\r\n--x--"),
		[]byte("Content-Transfer-Encoding: base64\r\n\r\naGVsbG8=%%%"),
		[]byte("Content-Type: text/plain; charset=GBK\r\n\r\n\xff"),
		[]byte(strings.Repeat("Content-Type: message/rfc822\r\n\r\n", 150) + "end"),
		[]byte("Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\ntruncated body"),
	} {
		msg, err := Parse(raw, time.Unix(0, 0))
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw[:min(80, len(raw))], err)
		}
		if msg == nil || !utf8.ValidString(msg.Body) || !utf8.ValidString(msg.Subject) {
			t.Fatalf("invalid fallback: %+v", msg)
		}
		if _, err := json.Marshal(msg); err != nil {
			t.Fatal(err)
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"", "garbage\x00\xff", "Subject: hello\r\n\r\nbody",
		"Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n\r\nbody\r\n--x--",
		"Subject: =?GBK?B?1tDOxA==?=\r\nContent-Type: text/plain; charset=GBK\r\n\r\n\xd6\xd0\xce\xc4",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		msg, err := Parse(raw, time.Unix(0, 0))
		if err != nil {
			t.Fatal(err)
		}
		if msg == nil || !utf8.ValidString(msg.Body) || !utf8.ValidString(msg.Subject) {
			t.Fatalf("invalid parse result: %+v", msg)
		}
	})
}
