package web_test

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"maildrop/internal/config"
	"maildrop/internal/ipguard"
	"maildrop/internal/store"
	"maildrop/internal/web"
)

func pageFrontend() fstest.MapFS {
	files := fstest.MapFS{}
	for _, name := range []string{"mailbox_login.html", "mailbox_manager.html", "admin_mailbox.html", "register.html", "api_test.html"} {
		files["templates/"+name] = &fstest.MapFile{Data: []byte("<!doctype html><title>" + name + " 邮箱</title>\n")}
	}
	files["static/styles/app.css"] = &fstest.MapFile{Data: []byte("body { color: red; }\n")}
	files["static/scripts/app.js"] = &fstest.MapFile{Data: []byte("console.log('mail');\n")}
	files["static/icon.svg"] = &fstest.MapFile{Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)}
	files["static/blob.unknownext"] = &fstest.MapFile{Data: []byte{0, 1, 2, 3}}
	files["static/directory/index.html"] = &fstest.MapFile{Data: []byte("private listing")}
	return files
}

func pageNewServer(t *testing.T, frontend fs.FS) http.Handler {
	t.Helper()
	cfg := config.FromEnv(func(string) string { return "" })
	db, err := store.OpenWithConfig(filepath.Join(t.TempDir(), "mailbox.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	clock := func() time.Time { return time.Unix(1800000000, 0) }
	return web.New(web.Deps{
		Config: cfg, Store: db, Frontend: frontend, Now: clock,
		Blocker:   ipguard.NewBlockerWithClock(300, 3, 60, clock),
		Whitelist: ipguard.NewWhitelist(false, ""),
	})
}

func pageRequest(handler http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func pageAssertPrivate(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	for name, value := range map[string]string{
		"Cache-Control": "no-store, max-age=0",
		"Pragma":        "no-cache", "Expires": "0",
		"Referrer-Policy": "no-referrer",
		"X-Robots-Tag":    "noindex, nofollow, noarchive",
		"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff",
		"Permissions-Policy":      "camera=(), microphone=(), geolocation=()",
		"Content-Security-Policy": "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'",
	} {
		if got := w.Header().Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
}

func TestPageRoutes(t *testing.T) {
	frontend := pageFrontend()
	handler := pageNewServer(t, frontend)
	for target, name := range map[string]string{
		"/": "mailbox_login.html", "/web": "mailbox_login.html",
		"/admin": "admin_mailbox.html", "/register": "register.html", "/api-test": "api_test.html",
		"/web/box@example.test----aB0._~": "mailbox_manager.html",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method+target, func(t *testing.T) {
				w := pageRequest(handler, method, target, nil)
				if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
					t.Fatalf("status/content type: %d %v", w.Code, w.Header())
				}
				body := frontend["templates/"+name].Data
				if w.Header().Get("Content-Length") != strconv.Itoa(len(body)) {
					t.Error("incorrect content length")
				}
				if method == http.MethodHead {
					if w.Body.Len() != 0 {
						t.Error("HEAD returned a body")
					}
				} else if w.Body.String() != string(body) {
					t.Errorf("template changed: %q", w.Body.String())
				}
				if w.Header().Get("X-Content-Type-Options") != "nosniff" {
					t.Error("missing nosniff")
				}
				if strings.HasPrefix(target, "/web/") {
					pageAssertPrivate(t, w)
				} else if w.Header().Get("Content-Security-Policy") != "" {
					t.Error("private CSP on public/admin page")
				}
			})
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := pageRequest(handler, method, "/login?token=old", nil)
		if w.Code != http.StatusFound || w.Header().Get("Location") != "/" {
			t.Fatalf("login redirect: %d %v", w.Code, w.Header())
		}
		if method == http.MethodHead && w.Body.Len() != 0 {
			t.Error("redirect HEAD returned a body")
		}
	}
	for _, target := range []string{"/unknown", "/admin/", "/register/", "/api-test/"} {
		if w := pageRequest(handler, http.MethodGet, target, nil); w.Code != http.StatusNotFound {
			t.Errorf("%s = %d", target, w.Code)
		}
	}
	for _, target := range []string{"/", "/web", "/login", "/admin", "/register", "/api-test", "/static/icon.svg"} {
		if w := pageRequest(handler, http.MethodPost, target, nil); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d", target, w.Code)
		}
	}
}

func TestPageCredentialValidation(t *testing.T) {
	handler := pageNewServer(t, pageFrontend())
	for _, address := range []string{"a@b", "a----b@example.test", "邮箱@例子.测试", `a\b@c`, "a\u200bb@c", "a\ufeffb@c", "a\x00b@c"} {
		for _, key := range []string{"Ab09._", "aB0._~-", strings.Repeat("x", 128)} {
			target := "/web/" + url.PathEscape(address+"----"+key)
			if w := pageRequest(handler, http.MethodGet, target, nil); w.Code != http.StatusOK {
				t.Errorf("valid credential %q %q: %d", address, key, w.Code)
			}
		}
	}
	invalid := []string{
		"", "missing", "a@b", "----abcdef", "a@b----", "a@b----abcde",
		"a@b----" + strings.Repeat("x", 129), "a@b----abc+de", "a@b----abc/def", "a@b----密钥abcdef",
		"a@b----abc def", "a@b----abc\ndef", "a@b----ab@cde", "a@b----abcdef----x",
		"@b----abcdef", "a@----abcdef", "a@@b----abcdef", "a/b@c----abcdef", "a@b/c----abcdef",
	}
	for _, whitespace := range []rune{' ', '\t', '\n', '\r', '\v', '\f', '\u001c', '\u001d', '\u001e', '\u001f', '\u0085', '\u00a0', '\u1680', '\u2000', '\u2001', '\u2002', '\u2003', '\u2004', '\u2005', '\u2006', '\u2007', '\u2008', '\u2009', '\u200a', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000'} {
		invalid = append(invalid, "a"+string(whitespace)+"@b----abcdef", "a@b"+string(whitespace)+"----abcdef")
	}
	for _, credential := range invalid {
		t.Run(fmt.Sprintf("%q", credential), func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				w := pageRequest(handler, method, "/web/"+url.PathEscape(credential), nil)
				if w.Code != http.StatusNotFound {
					t.Fatalf("status = %d", w.Code)
				}
				pageAssertPrivate(t, w)
				if method == http.MethodHead && w.Body.Len() != 0 {
					t.Error("404 HEAD returned a body")
				}
			}
		})
	}
}

func TestPagePrivateErrorsAndRedirects(t *testing.T) {
	handler := pageNewServer(t, pageFrontend())
	for _, tc := range []struct {
		method, target string
		status         int
	}{
		{"GET", "/web/missing", 404}, {"POST", "/web/a@b----abcdef", 405},
		{"POST", "/web/missing", 405}, {"OPTIONS", "/web/a@b----abcdef", 405},
		{"GET", "/web//a@b----abcdef", http.StatusTemporaryRedirect}, {"GET", "/web/./a@b----abcdef", http.StatusTemporaryRedirect},
		{"GET", "/web/../admin", http.StatusTemporaryRedirect},
		{"HEAD", "/web//a@b----abcdef", http.StatusTemporaryRedirect},
		{"HEAD", "/web/./a@b----abcdef", http.StatusTemporaryRedirect},
		{"HEAD", "/web/../admin", http.StatusTemporaryRedirect},
	} {
		w := pageRequest(handler, tc.method, tc.target, nil)
		if w.Code != tc.status {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.target, w.Code, tc.status)
		}
		pageAssertPrivate(t, w)
		if tc.method == http.MethodHead && w.Body.Len() != 0 {
			t.Error("canonical redirect HEAD returned a body")
		}
	}
	w := pageRequest(handler, http.MethodGet, "/unknown", nil)
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff on page 404")
	}
	w = pageRequest(handler, http.MethodGet, "/api/missing", nil)
	if w.Header().Get("Content-Security-Policy") != "" || w.Header().Get("Cache-Control") == "no-store, max-age=0" {
		t.Error("private page headers applied to API")
	}
}

func TestPageStatic(t *testing.T) {
	frontend := pageFrontend()
	handler := pageNewServer(t, frontend)
	for _, tc := range []struct{ name, mime string }{
		{"styles/app.css", "text/css"}, {"scripts/app.js", "javascript"},
		{"icon.svg", "image/svg+xml"}, {"blob.unknownext", "application/octet-stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := "/static/" + tc.name
			w := pageRequest(handler, http.MethodGet, target, nil)
			body := frontend["static/"+tc.name].Data
			etag := fmt.Sprintf(`"%x"`, sha256.Sum256(body))
			if w.Code != 200 || w.Body.String() != string(body) || !strings.Contains(w.Header().Get("Content-Type"), tc.mime) {
				t.Fatalf("asset response: %d %v %q", w.Code, w.Header(), w.Body.String())
			}
			if w.Header().Get("ETag") != etag || w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("asset headers: %v", w.Header())
			}
			head := pageRequest(handler, http.MethodHead, target, nil)
			if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(len(body)) || head.Header().Get("ETag") != etag {
				t.Errorf("asset HEAD: %d %v %q", head.Code, head.Header(), head.Body.String())
			}
			for _, conditional := range []string{etag, "W/" + etag, `"other", ` + etag, `W/"other", W/` + etag, "*"} {
				for _, method := range []string{http.MethodGet, http.MethodHead} {
					cached := pageRequest(handler, method, target, map[string]string{"If-None-Match": conditional})
					if cached.Code != 304 || cached.Body.Len() != 0 || cached.Header().Get("ETag") != etag || cached.Header().Get("Cache-Control") != "no-cache" {
						t.Errorf("%s If-None-Match %q: %d %v", method, conditional, cached.Code, cached.Header())
					}
				}
			}
			miss := pageRequest(handler, http.MethodGet, target, map[string]string{"If-None-Match": `"unmatched"`})
			if miss.Code != 200 || miss.Body.String() != string(body) {
				t.Error("nonmatching ETag did not return content")
			}
		})
	}
	for _, target := range []string{
		"/static/", "/static/missing", "/static/styles", "/static/styles/", "/static/directory/",
		"/static/%2e%2e/templates/mailbox_login.html", "/static/%2e%2e%2ftemplates%2fmailbox_login.html",
		"/static/..%5ctemplates%5cmailbox_login.html", "/static/styles%5capp.css", "/static/C:%5cWindows%5cwin.ini",
		"/static/styles/app.css:secret", "/static/%00", "/static/%2fetc%2fpasswd",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := pageRequest(handler, method, target, nil)
			if w.Code != 404 {
				t.Errorf("unsafe/missing %s %s = %d", method, target, w.Code)
			}
			if strings.Contains(w.Body.String(), "private listing") || strings.Contains(w.Body.String(), "mailbox_login.html") {
				t.Errorf("file/directory leaked for %s", target)
			}
			if w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("missing nosniff on %s %s", method, target)
			}
			if method == http.MethodHead && w.Body.Len() != 0 {
				t.Errorf("static error HEAD returned a body for %s", target)
			}
		}
	}
}

type pageCountingFS struct {
	fs.FS
	mu    sync.Mutex
	opens int
}

func (f *pageCountingFS) Open(name string) (fs.File, error) {
	f.mu.Lock()
	f.opens++
	f.mu.Unlock()
	return f.FS.Open(name)
}

func TestPageStaticCacheIsPerServerAndConcurrent(t *testing.T) {
	counted := &pageCountingFS{FS: pageFrontend()}
	handler := pageNewServer(t, counted)
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			w := pageRequest(handler, http.MethodGet, "/static/styles/app.css", nil)
			if w.Code != 200 {
				t.Errorf("concurrent status %d", w.Code)
			}
		})
	}
	workers.Wait()
	counted.mu.Lock()
	opens := counted.opens
	counted.mu.Unlock()
	if opens != 1 {
		t.Errorf("immutable asset opened %d times, want once", opens)
	}
	other := pageFrontend()
	other["static/styles/app.css"] = &fstest.MapFile{Data: []byte("other server")}
	w := pageRequest(pageNewServer(t, other), http.MethodGet, "/static/styles/app.css", nil)
	if w.Body.String() != "other server" {
		t.Error("cache leaked between server instances")
	}
}

func TestPageNilAndMissingFrontend(t *testing.T) {
	for _, frontend := range []fs.FS{nil, fstest.MapFS{}} {
		handler := pageNewServer(t, frontend)
		for _, target := range []string{"/", "/admin", "/web/a@b----abcdef", "/static/missing"} {
			w := pageRequest(handler, http.MethodGet, target, nil)
			if w.Code != 404 {
				t.Errorf("missing frontend %s = %d", target, w.Code)
			}
		}
	}
}

func TestPageDirFSRejectsWindowsTraversal(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "static"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("outside static secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "static", "safe.txt"), []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	handler := pageNewServer(t, os.DirFS(root))
	for _, target := range []string{"/static/..%5csecret.txt", "/static/%2e%2e%2fsecret.txt", "/static/safe.txt:stream", "/static/%5csecret.txt"} {
		w := pageRequest(handler, http.MethodGet, target, nil)
		if w.Code != 404 || strings.Contains(w.Body.String(), "outside static secret") {
			t.Errorf("DirFS traversal %s = %d %q", target, w.Code, w.Body.String())
		}
	}
	if w := pageRequest(handler, http.MethodGet, "/static/safe.txt", nil); w.Code != 200 || w.Body.String() != "safe" {
		t.Errorf("DirFS safe file: %d %q", w.Code, w.Body.String())
	}
}
