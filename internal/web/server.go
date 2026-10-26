// Package web 提供 Maildrop 的 HTTP 层：页面、静态资源、公开 API 与管理 API。
//
// 文件归属约定：
//   - server.go / respond.go：公共骨架与工具函数。
//   - pages.go：页面与 /static 路由（registerPages）。
//   - api.go：公开 /api/* 路由（registerPublicAPI）。
//   - admin_api.go：管理 /api/admin/* 路由（registerAdminAPI）。
//
// 业务逻辑放在 internal/inbox、internal/admin 等服务包中，本包只负责请求解析、鉴权与响应。
package web

import (
	"io/fs"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"maildrop/internal/config"
	"maildrop/internal/ipguard"
	"maildrop/internal/store"
)

// Deps 汇总 HTTP 层依赖；由 cmd/maildrop 统一构造，测试可替换任意一项。
type Deps struct {
	Config    *config.Config
	Store     *store.Store
	Blocker   *ipguard.Blocker
	Whitelist *ipguard.Whitelist
	// Frontend 的根目录需包含 templates/ 与 static/ 两个子目录。
	Frontend fs.FS
	// Now 为空时使用 time.Now，测试可注入固定时钟。
	Now func() time.Time
}

// Server 实现 http.Handler；各路由文件在 register* 中按需构造自己的服务对象。
type Server struct {
	cfg       *config.Config
	store     *store.Store
	blocker   *ipguard.Blocker
	whitelist *ipguard.Whitelist
	frontend  fs.FS
	now       func() time.Time

	mux     *http.ServeMux
	handler http.Handler
}

// New 构造并注册全部路由。
func New(deps Deps) *Server {
	s := &Server{
		cfg:       deps.Config,
		store:     deps.Store,
		blocker:   deps.Blocker,
		whitelist: deps.Whitelist,
		frontend:  deps.Frontend,
		now:       deps.Now,
		mux:       http.NewServeMux(),
	}
	if s.now == nil {
		s.now = time.Now
	}

	s.registerPages()
	s.registerPublicAPI()
	s.registerAdminAPI()

	s.handler = s.pageHeaders(s.recoverPanics(s.mux))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// clientIP 是全站唯一的客户端地址来源，统一遵循 TRUST_PROXY_HOPS。
func (s *Server) clientIP(r *http.Request) string {
	return ipguard.ClientIP(r, s.cfg.TrustProxyHops)
}

// recoverPanics 防止单个请求的 panic 终止进程，并且不向客户端泄露内部细节。
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				slog.Error("http handler panic", "method", r.Method, "path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()))
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error":   "Internal server error",
					"message": "An unexpected error occurred",
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}
