package outbound

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"sort"
	"strings"
	"time"
)

type MXResolver interface {
	LookupMX(context.Context, string) ([]*net.MX, error)
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}
type DirectTransport struct {
	Hostname string
	IPv4Only bool
	Resolver MXResolver
	// 测试注入连接器；生产始终拨号已验证的公共 IP，阻止 DNS 重绑定。
	Dial func(context.Context, string, string) (net.Conn, error)
}

func (t *DirectTransport) Deliver(ctx context.Context, from, to string, raw []byte) error {
	resolver := t.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	domain := domainOf(to)
	mx, err := resolver.LookupMX(ctx, domain)
	if err != nil {
		var de *net.DNSError
		if !errors.As(err, &de) || !de.IsNotFound {
			return &DeliveryError{Reason: "MX 查询暂时失败"}
		}
	}
	if len(mx) == 1 && mx[0].Host == "." {
		return &DeliveryError{Permanent: true, Reason: "收件域名声明不接受邮件（Null MX）"}
	}
	if len(mx) == 0 {
		mx = []*net.MX{{Host: domain}}
	}
	sort.SliceStable(mx, func(i, j int) bool { return mx[i].Pref < mx[j].Pref })
	var last error = &DeliveryError{Reason: "没有可连接的收件服务器"}
	temporary := false
	for _, host := range mx {
		if host.Host == "." {
			continue
		}
		name := strings.TrimSuffix(host.Host, ".")
		ips, err := resolver.LookupIPAddr(ctx, name)
		if err != nil {
			var de *net.DNSError
			if errors.As(err, &de) && de.IsNotFound {
				last = &DeliveryError{Permanent: true, Reason: "收件服务器没有地址记录"}
			} else {
				temporary = true
				last = &DeliveryError{Reason: "收件服务器地址查询暂时失败"}
			}
			continue
		}
		for _, ip := range ips {
			if t.IPv4Only && ip.IP.To4() == nil {
				continue
			}
			if t.Dial == nil && !publicIP(ip.IP) {
				last = &DeliveryError{Permanent: true, Reason: "收件服务器指向非公网地址"}
				continue
			}
			dial := t.Dial
			if dial == nil {
				dial = (&net.Dialer{Timeout: 30 * time.Second}).DialContext
			}
			conn, err := dial(ctx, "tcp", net.JoinHostPort(ip.IP.String(), "25"))
			if err != nil {
				temporary = true
				last = &DeliveryError{Reason: "无法连接收件服务器"}
				continue
			}
			err = t.send(ctx, conn, name, from, to, raw)
			if err == nil {
				return nil
			}
			last = err
			temporary = true
			var de *DeliveryError
			if errors.As(err, &de) && (de.Permanent || de.Uncertain) {
				return err
			}
		}
	}
	var permanent *DeliveryError
	if temporary && errors.As(last, &permanent) && permanent.Permanent {
		return &DeliveryError{Reason: "部分 MX 暂时不可用，将重试"}
	}
	return last
}
func publicIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "64:ff9b::/96", "64:ff9b:1::/48", "2002::/16", "2001::/32"} {
		_, block, _ := net.ParseCIDR(cidr)
		if block.Contains(ip) {
			return false
		}
	}
	return true
}
func (t *DirectTransport) send(ctx context.Context, conn net.Conn, host, from, to string, raw []byte) error {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline := func(d time.Duration) { _ = conn.SetDeadline(time.Now().Add(d)) }
	deadline(5 * time.Minute)
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return smtpResult(err, false)
	}
	defer client.Close()
	if err = client.Hello(t.Hostname); err != nil {
		return smtpResult(err, false)
	}
	if ok, _ := client.Extension("STARTTLS"); ok {
		deadline(time.Minute)
		// 公共 MX 的证书必须校验；握手失败重试，不悄悄降为明文。
		if err = client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return &DeliveryError{Reason: "收件服务器 TLS 握手失败"}
		}
	}
	if ok, size := client.Extension("SIZE"); ok {
		var maximum int64
		if _, e := fmt.Sscan(size, &maximum); e == nil && maximum > 0 && int64(len(raw)) > maximum {
			return &DeliveryError{Permanent: true, Reason: "邮件超过对方服务器大小限制"}
		}
	}
	deadline(5 * time.Minute)
	if err = client.Mail(from); err != nil {
		return smtpResult(err, false)
	}
	deadline(5 * time.Minute)
	if err = client.Rcpt(to); err != nil {
		return smtpResult(err, false)
	}
	deadline(3 * time.Minute)
	writer, err := client.Data()
	if err != nil {
		return smtpResult(err, false)
	}
	deadline(3 * time.Minute)
	if _, err = writer.Write(raw); err != nil {
		return smtpResult(err, false)
	}
	deadline(10 * time.Minute)
	if err = writer.Close(); err != nil {
		return smtpResult(err, true)
	}
	// DATA 的肯定响应才是接受，QUIT 失败不改变该事实。
	deadline(5 * time.Second)
	_ = client.Quit()
	return nil
}
func smtpResult(err error, afterData bool) error {
	var smtpErr *textproto.Error
	if errors.As(err, &smtpErr) {
		return &DeliveryError{Permanent: smtpErr.Code >= 500, Reason: fmt.Sprintf("SMTP %d %s", smtpErr.Code, smtpErr.Msg)}
	}
	reason := "SMTP 连接暂时失败"
	if afterData {
		reason = "正文已提交但确认丢失，将自动重试；可能重复"
	} else if errors.Is(err, io.EOF) {
		reason = "对方服务器断开连接"
	}
	return &DeliveryError{Uncertain: afterData, Reason: reason}
}
