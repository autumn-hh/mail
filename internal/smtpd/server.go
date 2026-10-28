// Package smtpd 提供仅收信、无认证的 SMTP 服务。
package smtpd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"maildrop/internal/config"
	"maildrop/internal/inbox"
	"maildrop/internal/ipguard"
	"maildrop/internal/mailparse"

	"github.com/emersion/go-smtp"
)

const maxRecipients = 100

type Server struct {
	smtp        *smtp.Server
	mu          sync.Mutex
	connections map[*trackedConn]struct{}
	stopping    bool
	cancel      context.CancelFunc
}

type trackedListener struct {
	net.Listener
	server *Server
}

type trackedConn struct {
	net.Conn
	server *Server
}

func (l *trackedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tracked := &trackedConn{Conn: conn, server: l.server}
	l.server.mu.Lock()
	defer l.server.mu.Unlock()
	if l.server.stopping {
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	l.server.connections[tracked] = struct{}{}
	return tracked, nil
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.server.mu.Lock()
	delete(c.server.connections, c)
	c.server.mu.Unlock()
	return err
}

type backend struct {
	ctx       context.Context
	inbox     *inbox.Service
	whitelist *ipguard.Whitelist
	rawLimit  int64
}

type session struct {
	backend    *backend
	clientIP   string
	from       string
	recipients []string
}

var _ smtp.Backend = (*backend)(nil)
var _ smtp.Session = (*session)(nil)

// New 限制原始 DATA 为正文限制的两倍加 1 MiB，容纳 MIME 编码和邮件头。
func New(cfg *config.Config, inboxSvc *inbox.Service, whitelist *ipguard.Whitelist) *Server {
	if whitelist == nil {
		whitelist = ipguard.NewWhitelist(cfg.EnableIPWhitelist, cfg.IPWhitelist)
	}
	rawLimit := 2*cfg.MaxEmailSizeBytes + 1024*1024
	ctx, cancel := context.WithCancel(context.Background())
	b := &backend{ctx: ctx, inbox: inboxSvc, whitelist: whitelist, rawLimit: rawLimit}
	s := smtp.NewServer(b)
	s.Domain = cfg.Domain
	if s.Domain == "" {
		s.Domain = "localhost"
	}
	s.ReadTimeout = 60 * time.Second
	s.WriteTimeout = 30 * time.Second
	s.MaxRecipients = maxRecipients
	s.MaxMessageBytes = rawLimit
	s.AllowInsecureAuth = false
	s.ErrorLog = smtpLogger{}
	return &Server{smtp: s, connections: make(map[*trackedConn]struct{}), cancel: cancel}
}

func (s *Server) ListenAndServe(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(listener)
}

func (s *Server) Serve(listener net.Listener) error {
	defer listener.Close()
	return s.smtp.Serve(&trackedListener{Listener: listener, server: s})
}

// Shutdown 等待会话退出；超时后关闭连接，确保服务不会继续占用资源。
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	err := s.smtp.Shutdown(ctx)
	s.cancel()
	if err != nil {
		// go-smtp 已关闭 done，不能再通过其 Close 强制关闭会话。
		s.mu.Lock()
		connections := make([]*trackedConn, 0, len(s.connections))
		for conn := range s.connections {
			connections = append(connections, conn)
		}
		s.mu.Unlock()
		for _, conn := range connections {
			_ = conn.Close()
		}
	}
	return err
}

func (b *backend) NewSession(conn *smtp.Conn) (smtp.Session, error) {
	clientIP := conn.Conn().RemoteAddr().String()
	if host, _, err := net.SplitHostPort(clientIP); err == nil {
		clientIP = host
	}
	return &session{backend: b, clientIP: clientIP}, nil
}

func (s *session) Reset()        { s.from = ""; s.recipients = nil }
func (s *session) Logout() error { s.Reset(); return nil }

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	s.Reset()
	if !s.backend.whitelist.IsIPWhitelisted(s.clientIP) {
		return smtpFailure(550, smtp.EnhancedCode{5, 7, 1}, "Access denied - IP not whitelisted")
	}
	s.from = from
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	if len(s.recipients) >= maxRecipients {
		return smtpFailure(452, smtp.EnhancedCode{4, 5, 3}, "Maximum limit of 100 recipients reached")
	}
	s.recipients = append(s.recipients, to)
	return nil
}

func (s *session) Data(reader io.Reader) error {
	if !s.backend.whitelist.IsIPWhitelisted(s.clientIP) {
		return smtpFailure(550, smtp.EnhancedCode{5, 7, 1}, "Access denied - IP not whitelisted")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, s.backend.rawLimit+1))
	if errors.Is(err, smtp.ErrDataTooLarge) || int64(len(raw)) > s.backend.rawLimit {
		return smtp.ErrDataTooLarge
	}
	if err != nil {
		slog.Error("SMTP 读取失败", "error", err)
		return smtpFailure(451, smtp.EnhancedCode{4, 3, 0}, "Could not process email")
	}
	msg, err := mailparse.Parse(raw, time.Now())
	if err != nil {
		slog.Error("SMTP 解析失败", "error", err)
		return smtpFailure(451, smtp.EnhancedCode{4, 3, 0}, "Could not process email")
	}
	ctx, cancel := context.WithTimeout(s.backend.ctx, 30*time.Second)
	defer cancel()
	result, err := s.backend.inbox.Deliver(ctx, s.from, s.recipients, msg)
	if err != nil {
		slog.Error("SMTP 存储失败", "error", err)
		return smtpFailure(451, smtp.EnhancedCode{4, 3, 0}, "Could not process email")
	}
	for _, recipient := range result.Recipients {
		if !recipient.Accepted {
			slog.Warn("SMTP 收件人拒收", "recipient", recipient.Recipient, "reason", recipient.Reason)
		}
	}
	if result.Accepted > 0 {
		return nil
	}
	reason := "No recipient specified"
	if len(result.Recipients) > 0 {
		reason = result.Recipients[0].Reason
	}
	return smtpFailure(550, smtp.EnhancedCode{5, 7, 1}, reason)
}

func smtpFailure(code int, enhanced smtp.EnhancedCode, message string) error {
	return &smtp.SMTPError{Code: code, EnhancedCode: enhanced, Message: message}
}

type smtpLogger struct{}

func (smtpLogger) Printf(format string, args ...any) {
	slog.Error("SMTP 协议错误", "error", fmt.Sprintf(format, args...))
}
func (smtpLogger) Println(args ...any) { slog.Error("SMTP 协议错误", "error", fmt.Sprint(args...)) }
