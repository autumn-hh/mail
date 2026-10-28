package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// maxJSONBodyBytes 限制 API 请求体，避免超大 JSON 占满内存；批量操作也远小于该值。
const maxJSONBodyBytes = 1 << 20

// errBodyTooLarge 表示请求体超过 maxJSONBodyBytes，调用方通常应返回 413。
var errBodyTooLarge = errors.New("request body too large")

// writeJSON 输出 JSON 响应；字段名与状态码由调用方按原 Flask 接口保持一致。
func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// 与 Flask jsonify 一致，不把 < > & 转义成 < 等形式。
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		slog.Error("encode json response", "error", err)
		http.Error(w, `{"error":"Internal server error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// readJSON 以 Flask get_json(silent=True) 的宽松语义读取请求体：
// 不校验 Content-Type；空体或非法 JSON 返回 ok=false，由调用方决定错误响应。
// 超过大小上限时返回 errBodyTooLarge。
func readJSON(w http.ResponseWriter, r *http.Request, dst any) (ok bool, err error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return false, errBodyTooLarge
		}
		return false, nil
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return false, nil
	}
	if json.Unmarshal(body, dst) != nil {
		return false, nil
	}
	return true, nil
}

// bearerToken 只接受 "Authorization: Bearer <token>"，scheme 不区分大小写。
func bearerToken(r *http.Request) (string, bool) {
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, token, found := strings.Cut(authorization, " ")
	token = strings.TrimSpace(token)
	if !found || !strings.EqualFold(scheme, "bearer") || token == "" {
		return "", false
	}
	return token, true
}
