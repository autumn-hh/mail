package web

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// registerPages 注册页面，每个实例持有独立静态缓存。
func (s *Server) registerPages() {
	for route, name := range map[string]string{
		"GET /{$}":      "mailbox_login.html",
		"GET /web":      "mailbox_login.html",
		"GET /admin":    "admin_mailbox.html",
		"GET /register": "register.html",
		"GET /api-test": "api_test.html",
	} {
		s.mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
			s.pageTemplate(w, r, name)
		})
	}
	s.mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusFound)
	})
	s.mux.HandleFunc("GET /web/{credential...}", func(w http.ResponseWriter, r *http.Request) {
		credential := r.PathValue("credential")
		separator := strings.LastIndex(credential, "----")
		if separator < 0 || !pageEmailValid(credential[:separator]) || !pageKeyValid(credential[separator+4:]) {
			pageNotFound(w, r)
			return
		}
		s.pageTemplate(w, r, "mailbox_manager.html")
	})
	assets := &pageAssets{frontend: s.frontend, files: make(map[string]*pageAsset)}
	s.mux.Handle("GET /static/{path...}", assets)
}

func (s *Server) pageTemplate(w http.ResponseWriter, r *http.Request, name string) {
	if s.frontend == nil {
		pageNotFound(w, r)
		return
	}
	body, err := fs.ReadFile(s.frontend, "templates/"+name)
	if err != nil {
		pageNotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// Python 的 Unicode 空白还包含四个信息分隔符。
func pageEmailValid(address string) bool {
	local, domain, found := strings.Cut(address, "@")
	if !found || local == "" || domain == "" || strings.Contains(domain, "@") {
		return false
	}
	for _, ch := range address {
		if ch == '/' || unicode.IsSpace(ch) || ch >= '\u001c' && ch <= '\u001f' {
			return false
		}
	}
	return true
}

func pageKeyValid(key string) bool {
	if len(key) < 6 || len(key) > 128 {
		return false
	}
	for _, ch := range key {
		if !(ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || strings.ContainsRune("._~-", ch)) {
			return false
		}
	}
	return true
}

// pageHeaders 包住路由器，覆盖 404、405 和路径重定向。
func (s *Server) pageHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api" && !strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("X-Content-Type-Options", "nosniff")
		}
		if strings.HasPrefix(r.URL.Path, "/web/") {
			h := w.Header()
			h.Set("Cache-Control", "no-store, max-age=0")
			h.Set("Pragma", "no-cache")
			h.Set("Expires", "0")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("X-Robots-Tag", "noindex, nofollow, noarchive")
			h.Set("X-Frame-Options", "DENY")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
			h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		}
		next.ServeHTTP(w, r)
	})
}

type pageAsset struct {
	body        []byte
	etag        string
	contentType string
}

type pageAssets struct {
	frontend fs.FS
	mu       sync.Mutex
	files    map[string]*pageAsset
}

func (a *pageAssets) load(name string) (*pageAsset, error) {
	// 首次读取也持锁，避免并发请求重复读取和计算摘要。
	a.mu.Lock()
	defer a.mu.Unlock()
	if asset, ok := a.files[name]; ok {
		return asset, nil
	}
	file, err := a.frontend.Open("static/" + name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fs.ErrNotExist
	}
	body, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType == "" {
		contentType = http.DetectContentType(body)
	}
	asset := &pageAsset{body: body, etag: fmt.Sprintf(`"%x"`, sha256.Sum256(body)), contentType: contentType}
	a.files[name] = asset
	return asset, nil
}

func (a *pageAssets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("path")
	// 补充拒绝 Windows 分隔符和 NTFS 替代数据流。
	if a.frontend == nil || !fs.ValidPath(name) || !filepath.IsLocal(name) || strings.ContainsAny(name, "\\:\x00") {
		pageNotFound(w, r)
		return
	}
	asset, err := a.load(name)
	if err != nil {
		pageNotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("ETag", asset.etag)
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, path.Base(name), time.Time{}, bytes.NewReader(asset.body))
}

func pageNotFound(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		return
	}
	http.NotFound(w, r)
}
