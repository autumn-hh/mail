// Command maildrop 在同一进程内运行 Web 服务、SMTP 收信服务与定时清理任务。
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"maildrop"
	"maildrop/internal/cleanup"
	"maildrop/internal/config"
	"maildrop/internal/inbox"
	"maildrop/internal/ipguard"
	"maildrop/internal/smtpd"
	"maildrop/internal/store"
	"maildrop/internal/web"

	"github.com/emersion/go-smtp"
)

// shutdownTimeout 为收到退出信号后等待在途 HTTP 请求与 SMTP 会话结束的上限。
const shutdownTimeout = 15 * time.Second

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err := run(); err != nil {
		slog.Error("maildrop stopped with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Password == "" {
		// 与 Python 版一致：未配置密码时管理接口全部拒绝认证，但收信与公开邮箱照常工作。
		slog.Warn("PASSWORD is not configured; admin login is disabled")
	}
	if !cfg.UseDatabase {
		slog.Warn("USE_DATABASE=false is no longer supported; SQLite storage is always used", "path", cfg.DatabasePath)
		// 存储固定为 SQLite，旧 .env 中的 false 不能让公开接口误报“未启用数据库”。
		cfg.UseDatabase = true
	}

	db, err := store.OpenWithConfig(cfg.DatabasePath, cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	// .env 中的 DOMAINS 只在域名表为空（首次启动）时导入，之后以管理后台为准。
	if err := db.SeedDomainsIfEmpty(cfg.Domains); err != nil {
		return err
	}

	frontend, err := fs.Sub(maildrop.FrontendFS, "src/frontend")
	if err != nil {
		return err
	}

	blocker := ipguard.NewBlocker(cfg.IPBlockDuration, cfg.IPMaxFailedAttempts, cfg.IPAttemptWindow)
	whitelist := ipguard.NewWhitelist(cfg.EnableIPWhitelist, cfg.IPWhitelist)
	inboxSvc := inbox.New(cfg, db, nil)

	webHandler := web.New(web.Deps{
		Config:    cfg,
		Store:     db,
		Blocker:   blocker,
		Whitelist: whitelist,
		Frontend:  frontend,
	})
	// 处理器上下文在强制关闭时取消，并统计在途请求，确保数据库在所有处理器退出后才关闭。
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	var inflight sync.WaitGroup
	httpServer := &http.Server{
		Addr: net.JoinHostPort(cfg.FlaskHost, strconv.Itoa(cfg.FlaskPort)),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inflight.Add(1)
			defer inflight.Done()
			webHandler.ServeHTTP(w, r)
		}),
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	smtpServer := smtpd.New(cfg, inboxSvc, whitelist)
	smtpAddr := net.JoinHostPort(cfg.SMTPHost, strconv.Itoa(cfg.SMTPPort))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 任一服务异常退出都触发整体关闭，避免进程只剩半边功能却看似健康。
	errCh := make(chan error, 2)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		cleanup.Run(ctx, cfg, inboxSvc, blocker)
	}()

	go func() {
		slog.Info("web server listening", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go func() {
		slog.Info("smtp server listening", "addr", smtpAddr)
		if err := smtpServer.ListenAndServe(smtpAddr); err != nil && !isSMTPClosed(err) {
			errCh <- err
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	case runErr = <-errCh:
		slog.Error("server failed, shutting down", "error", runErr)
	}
	stop()

	// 两个服务并行关闭，共享同一个期限；超时未结束的 HTTP 请求被强制断开并取消上下文。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	var (
		stopWG      sync.WaitGroup
		mu          sync.Mutex
		shutdownErr error
	)
	record := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		shutdownErr = errors.Join(shutdownErr, err)
	}
	stopWG.Add(2)
	go func() {
		defer stopWG.Done()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			slog.Warn("web server graceful shutdown timed out, closing connections", "error", err)
			_ = httpServer.Close()
			cancelBase()
			record(fmt.Errorf("web server shutdown: %w", err))
		}
	}()
	go func() {
		defer stopWG.Done()
		if err := smtpServer.Shutdown(shutdownCtx); err != nil {
			record(fmt.Errorf("smtp server shutdown: %w", err))
		}
	}()
	stopWG.Wait()
	cancelBase()
	if !waitTimeout(&inflight, handlerDrainTimeout) {
		slog.Warn("http handlers still running after shutdown; closing database anyway")
	}
	wg.Wait()
	// db.Close 由 defer 执行，并会等待仍在进行的查询结束。
	return errors.Join(runErr, shutdownErr)
}

// handlerDrainTimeout 为强制断开连接后等待处理器退出的上限；上下文已取消，正常会很快返回。
const handlerDrainTimeout = 5 * time.Second

// waitTimeout 在限定时间内等待 WaitGroup 归零，返回是否按时完成。
func waitTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// isSMTPClosed 判断 SMTP 服务是否因主动关闭而返回。
func isSMTPClosed(err error) bool {
	return errors.Is(err, smtp.ErrServerClosed) || errors.Is(err, net.ErrClosed)
}
