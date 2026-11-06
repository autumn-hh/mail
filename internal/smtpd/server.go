// Package smtpd 提供仅收信、无认证的 SMTP 服务。
package smtpd

import (
	"context"
	"crypto/tls"
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
	"maildrop/internal/mailauth"
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
	startupErr  error
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
	verify    bool
	verifier  mailauth.Verifier
	slots     chan struct{}
	bounce    BounceHandler
}

type session struct {
	backend    *backend
	clientIP   string
	from       string
	helo       string
	bounceOnly bool
	recipients []string
}

var _ smtp.Backend = (*backend)(nil)
var _ smtp.Session = (*session)(nil)

// BounceHandler 只处理已生成的退信地址，公网 SMTP 不提供外发中继。
type BounceHandler interface {
	IsBounceRecipient(context.Context, string) (bool, error)
	ReceiveBounce(context.Context, []string, []byte) (bool, error)
}

// New 原始 RFC 邮件包含附件，整体使用同一个大小上限。
func New(cfg *config.Config, inboxSvc *inbox.Service, whitelist *ipguard.Whitelist) *Server {
	if whitelist == nil {
		whitelist = ipguard.NewWhitelist(cfg.EnableIPWhitelist, cfg.IPWhitelist)
	}
	rawLimit := cfg.MaxEmailSizeBytes
	ctx, cancel := context.WithCancel(context.Background())
	b := &backend{ctx: ctx, inbox: inboxSvc, whitelist: whitelist, rawLimit: rawLimit, verify: cfg.VerifyInbound, slots: make(chan struct{}, 4)}
	s := smtp.NewServer(b)
	s.Domain = cfg.SMTPHostname
	if s.Domain == "" {
		s.Domain = cfg.Domain
	}
	if s.Domain == "" {
		s.Domain = "localhost"
	}
	s.ReadTimeout = 60 * time.Second
	s.WriteTimeout = 30 * time.Second
	s.MaxRecipients = maxRecipients
	s.MaxMessageBytes = rawLimit
	s.AllowInsecureAuth = false
	s.ErrorLog = smtpLogger{}
	server := &Server{smtp: s, connections: make(map[*trackedConn]struct{}), cancel: cancel}
	if cfg.SMTPTLSCertFile != "" || cfg.SMTPTLSKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.SMTPTLSCertFile, cfg.SMTPTLSKeyFile)
		if err != nil {
			server.startupErr = fmt.Errorf("load SMTP TLS certificate: %w", err)
		} else {
			s.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
		}
	}
	return server
}

func (s *Server) ListenAndServe(addr string) error {
	if s.startupErr != nil {
		return s.startupErr
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(listener)
}

func (s *Server) Serve(listener net.Listener) error {
	defer listener.Close()
	if s.startupErr != nil {
		return s.startupErr
	}
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
	return &session{backend: b, clientIP: clientIP, helo: conn.Hostname()}, nil
}

func (s *session) Reset()        { s.from = ""; s.recipients = nil; s.bounceOnly = false }
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
	ctx, cancel := context.WithTimeout(s.backend.ctx, 10*time.Second)
	defer cancel()
	if s.backend.bounce != nil {
		special, err := s.backend.bounce.IsBounceRecipient(ctx, to)
		if err != nil {
			return smtpFailure(451, smtp.EnhancedCode{4, 3, 0}, "Could not validate recipient")
		}
		if special {
			if len(s.recipients) > 0 && !s.bounceOnly {
				return smtpFailure(553, smtp.EnhancedCode{5, 5, 3}, "Delivery reports require a separate transaction")
			}
			s.bounceOnly = true
			s.recipients = append(s.recipients, to)
			return nil
		}
	}
	if s.bounceOnly {
		return smtpFailure(553, smtp.EnhancedCode{5, 5, 3}, "Delivery reports require a separate transaction")
	}
	code, reason, err := s.backend.inbox.RecipientStatus(ctx, to)
	if err != nil {
		return smtpFailure(451, smtp.EnhancedCode{4, 3, 0}, "Could not validate recipient")
	}
	if code != 0 {
		enh := smtp.EnhancedCode{5, 1, 1}
		if code < 500 {
			enh = smtp.EnhancedCode{4, 2, 2}
		}
		return smtpFailure(code, enh, reason)
	}
	s.recipients = append(s.recipients, to)
	return nil
}

func (s *session) Data(reader io.Reader) error {
	select {
	case s.backend.slots <- struct{}{}:
		defer func() { <-s.backend.slots }()
	default:
		return smtpFailure(451, smtp.EnhancedCode{4, 3, 2}, "Server busy, please retry")
	}
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
	ctx, cancel := context.WithTimeout(s.backend.ctx, 30*time.Second)
	defer cancel()
	if s.backend.bounce != nil {
		handled, err := s.backend.bounce.ReceiveBounce(ctx, s.recipients, raw)
		if err != nil {
			return smtpFailure(451, smtp.EnhancedCode{4, 3, 0}, "Could not store delivery report")
		}
		if handled {
			return nil
		}
	}
	msg, err := mailparse.Parse(raw, time.Now())
	if err != nil {
		if errors.Is(err, mailparse.ErrLimits) {
			return smtpFailure(552, smtp.EnhancedCode{5, 3, 4}, "MIME limits exceeded")
		}
		slog.Error("SMTP 解析失败", "error", err)
		return smtpFailure(451, smtp.EnhancedCode{4, 3, 0}, "Could not process email")
	}
	if s.backend.verify {
		msg.Details.Authentication = s.backend.verifier.Verify(ctx, raw, net.ParseIP(s.clientIP), s.helo, s.from)
	}
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
	code := 550
	for _, o := range result.Recipients {
		if o.Code >= 400 {
			code, reason = o.Code, o.Reason
			if code >= 500 {
				break
			}
		}
	}
	enhanced := smtp.EnhancedCode{5, 7, 1}
	if code < 500 {
		enhanced = smtp.EnhancedCode{4, 2, 2}
	}
	return smtpFailure(code, enhanced, reason)
}

func smtpFailure(code int, enhanced smtp.EnhancedCode, message string) error {
	return &smtp.SMTPError{Code: code, EnhancedCode: enhanced, Message: message}
}

type smtpLogger struct{}

func (smtpLogger) Printf(format string, args ...any) {
	slog.Error("SMTP 协议错误", "error", fmt.Sprintf(format, args...))
}
func (smtpLogger) Println(args ...any) { slog.Error("SMTP 协议错误", "error", fmt.Sprint(args...)) }

// SetBounceHandler 在监听前注入退信接收服务。
func (s *Server) SetBounceHandler(handler BounceHandler) { s.smtp.Backend.(*backend).bounce = handler }
