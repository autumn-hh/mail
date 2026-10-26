package maildrop

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	frontend "maildrop/src/frontend"
)

type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string { return e.Message }
func bad(msg string) error         { return &HTTPError{400, msg} }
func equalSecret(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}

type attempt struct {
	Count        int
	First, Until int64
}
type Limiter struct {
	mu                    sync.Mutex
	entries               map[string]attempt
	Duration, Max, Window int
}

func (l *Limiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.entries[ip].Until > time.Now().Unix()
}
func (l *Limiter) fail(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now().Unix()
	for k, v := range l.entries {
		if v.Until <= now && v.First+int64(l.Window) < now {
			delete(l.entries, k)
		}
	}
	v := l.entries[ip]
	if v.First+int64(l.Window) < now {
		v = attempt{First: now}
	}
	v.Count++
	if v.Count >= l.Max {
		v.Until = now + int64(l.Duration)
	}
	l.entries[ip] = v
	return v.Until > now
}
func (l *Limiter) config() Object {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Object{"block_duration": l.Duration, "max_attempts": l.Max, "attempt_window": l.Window}
}

type App struct {
	Store     *Store
	Config    Config
	limiter   *Limiter
	mux       *http.ServeMux
	templates map[string]*template.Template
}

func NewApp(s *Store) (*App, error) {
	c := s.Config
	a := &App{Store: s, Config: c, mux: http.NewServeMux(), templates: map[string]*template.Template{}, limiter: &Limiter{entries: map[string]attempt{}, Duration: c.BlockDuration, Max: c.MaxAttempts, Window: c.AttemptWindow}}
	// 模板仅用 url_for 引用静态资源；在构建运行时转换为 Go 函数，继续共用前端文件。
	pattern := regexp.MustCompile(`\{\{\s*url_for\('static',\s*filename='([^']+)'\)\s*\}\}`)
	files, e := fs.Glob(frontend.Files, "templates/*.html")
	if e != nil {
		return nil, e
	}
	for _, name := range files {
		b, e := frontend.Files.ReadFile(name)
		if e != nil {
			return nil, e
		}
		source := pattern.ReplaceAllString(string(b), `{{static "$1"}}`)
		t, e := template.New(name).Funcs(template.FuncMap{"static": func(p string) string { return "/static/" + p }}).Parse(source)
		if e != nil {
			return nil, e
		}
		a.templates[strings.TrimPrefix(name, "templates/")] = t
	}
	assets, e := fs.Sub(frontend.Files, "static")
	if e != nil {
		return nil, e
	}
	a.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(assets))))
	a.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if e := s.DB.PingContext(r.Context()); e != nil {
			http.Error(w, "unhealthy", 503)
			return
		}
		w.Write([]byte("ok"))
	})
	for path, name := range map[string]string{"/": "mailbox_login.html", "/web": "mailbox_login.html", "/admin": "admin_mailbox.html", "/register": "register.html", "/api-test": "api_test.html"} {
		p, n := path, name
		a.mux.HandleFunc("GET "+p, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != p {
				http.NotFound(w, r)
				return
			}
			a.page(w, r, n)
		})
	}
	a.mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/", http.StatusFound) })
	a.mux.HandleFunc("GET /web/{credential...}", func(w http.ResponseWriter, r *http.Request) {
		v := r.PathValue("credential")
		i := strings.LastIndex(v, "----")
		if i < 0 || !regexp.MustCompile(`^[^\s/@]+@[^\s/@]+$`).MatchString(v[:i]) || !regexp.MustCompile(`^[A-Za-z0-9._~-]{6,128}$`).MatchString(v[i+4:]) {
			http.NotFound(w, r)
			return
		}
		a.page(w, r, "mailbox_manager.html")
	})
	a.routes()
	a.adminRoutes()
	return a, nil
}
func (a *App) page(w http.ResponseWriter, r *http.Request, name string) {
	var b bytes.Buffer
	if e := a.templates[name].Execute(&b, nil); e != nil {
		slog.Error("Render page failed", "template", name, "error", e)
		http.Error(w, "Unable to render page", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b.Bytes())
}
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !strings.HasPrefix(r.URL.Path, "/static/") {
		w.Header().Set("Cache-Control", "no-store, max-age=0")
		w.Header().Set("Pragma", "no-cache")
	}
	if strings.HasPrefix(r.URL.Path, "/web/") {
		w.Header().Set("Expires", "0")
		w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	}
	a.mux.ServeHTTP(w, r)
}
func (a *App) ip(r *http.Request) string {
	ip, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		ip = r.RemoteAddr
	}
	if a.Config.ProxyHops > 0 {
		chain := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		if len(chain) >= a.Config.ProxyHops {
			candidate := strings.TrimSpace(chain[len(chain)-a.Config.ProxyHops])
			if net.ParseIP(candidate) != nil {
				ip = candidate
			}
		}
	}
	return ip
}

type endpoint func(*http.Request) (any, error)

func (a *App) handle(pattern string, admin bool, f endpoint) {
	a.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		var value any
		var e error
		if !a.Config.IPAllowed(a.ip(r)) {
			e = &HTTPError{403, "IP not whitelisted"}
		} else if admin {
			e = a.adminAuth(r, bearer(r))
		}
		if e == nil {
			value, e = f(r)
		}
		status := 200
		if e == nil && (r.URL.Path == "/api/register" || r.URL.Path == "/api/register_with_token") {
			status = http.StatusCreated
		}
		if e != nil {
			var he *HTTPError
			switch {
			case errors.As(e, &he):
				status = he.Status
			case errors.Is(e, sql.ErrNoRows):
				status = 404
				e = errors.New("Not found")
			default:
				status = 500
				slog.Error("API operation failed", "error", e)
				e = errors.New("服务器处理失败")
			}
			value = Object{"success": false, "error": e.Error(), "message": e.Error()}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(value); err != nil {
			slog.Warn("Write API response failed", "error", err)
		}
	})
}
func body(r *http.Request) (Object, error) {
	defer r.Body.Close()
	d := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	d.UseNumber()
	var v Object
	if e := d.Decode(&v); e != nil || v == nil {
		return nil, bad("请求体必须是 JSON 对象")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, bad("无效的 JSON 请求")
	}
	return v, nil
}
func textField(m Object, k string, required bool) (string, error) {
	v, ok := m[k]
	if !ok && !required {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", bad(k + " 必须是字符串")
	}
	s = strings.TrimSpace(s)
	if required && s == "" {
		return "", bad(k + " 不能为空")
	}
	return s, nil
}
func intField(m Object, k string, fallback, min, max int) (int, error) {
	v, ok := m[k]
	if !ok {
		return fallback, nil
	}
	n, ok := v.(json.Number)
	if !ok {
		return 0, bad(k + " 必须是整数")
	}
	i, e := n.Int64()
	if e != nil || i < int64(min) || i > int64(max) {
		return 0, bad(fmt.Sprintf("%s 必须在 %d–%d 之间", k, min, max))
	}
	return int(i), nil
}
func boolField(m Object, k string, fallback bool) (bool, error) {
	v, ok := m[k]
	if !ok {
		return fallback, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, bad(k + " 必须是布尔值")
	}
	return b, nil
}
func listField(m Object, k string) ([]string, error) {
	v, ok := m[k]
	if !ok {
		return []string{}, nil
	}
	items, ok := v.([]any)
	if !ok || len(items) > 1000 {
		return nil, bad(k + " 必须是列表（最多 1000 项）")
	}
	out := []string{}
	for _, v := range items {
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" || len(s) > 320 {
			return nil, bad(k + " 包含无效值")
		}
		out = append(out, strings.TrimSpace(s))
	}
	return out, nil
}
func bearer(r *http.Request) string {
	p := strings.Fields(r.Header.Get("Authorization"))
	if len(p) == 2 && strings.EqualFold(p[0], "Bearer") {
		return p[1]
	}
	return ""
}
func (a *App) adminAuth(r *http.Request, provided string) error {
	if a.limiter.blocked(a.ip(r)) {
		return &HTTPError{403, "IP已被临时封禁"}
	}
	if a.Config.Password == "" {
		return &HTTPError{401, "管理员密码未配置"}
	}
	if provided == "" || !equalSecret(provided, a.Config.Password) {
		if a.limiter.fail(a.ip(r)) {
			return &HTTPError{403, "认证失败次数过多，IP已被临时封禁"}
		}
		return &HTTPError{401, "认证失败"}
	}
	return nil
}
func (a *App) authMailbox(r *http.Request, address string, allowDisabled bool) (Object, error) {
	t := bearer(r)
	if t == "" {
		return nil, &HTTPError{401, "Authentication required"}
	}
	m, e := a.Store.Mailbox("access_token", t)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, &HTTPError{401, "Invalid access token"}
	}
	if e != nil {
		return nil, e
	}
	if truth(m["is_expired"]) {
		return nil, &HTTPError{410, "Mailbox expired"}
	}
	if !allowDisabled && !truth(m["is_active"]) {
		return nil, &HTTPError{423, "Mailbox disabled"}
	}
	if address != "" && m["address"] != address {
		return nil, &HTTPError{403, "Permission denied"}
	}
	return m, nil
}
func success(data any) Object { return Object{"success": true, "data": data} }
func ok() Object              { return Object{"success": true, "message": "操作成功"} }
func queryInt(r *http.Request, key string, fallback, min, max int) (int, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return fallback, nil
	}
	i, e := strconv.Atoi(raw)
	if e != nil || i < min || i > max {
		return 0, bad("无效的 " + key)
	}
	return i, nil
}
