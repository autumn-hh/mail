package ipguard

import (
	"log/slog"
	"net/netip"
	"strings"
)

// Whitelist 构造后只读，可被多个请求并发使用；启用时不隐式放行本机或 unknown。
type Whitelist struct {
	enabled  bool
	prefixes []netip.Prefix
}

// NewWhitelist 只解析一次逗号分隔的 IP/CIDR；错误项记录日志后跳过。
func NewWhitelist(enabled bool, entries string) *Whitelist {
	w := &Whitelist{enabled: enabled}
	if !enabled {
		return w
	}
	for _, entry := range strings.Split(entries, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			prefix, err := netip.ParsePrefix(entry)
			if err != nil {
				slog.Warn("跳过无效 IP 白名单项", "entry", entry, "error", err)
				continue
			}
			if prefix.Addr().Is4In6() && prefix.Bits() >= 96 {
				prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
			}
			w.prefixes = append(w.prefixes, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil || addr.Zone() != "" {
			slog.Warn("跳过无效 IP 白名单项", "entry", entry)
			continue
		}
		addr = addr.Unmap()
		w.prefixes = append(w.prefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return w
}

func (w *Whitelist) IsIPWhitelisted(clientIP string) bool {
	if !w.enabled {
		return true
	}
	addr, err := netip.ParseAddr(clientIP)
	if err != nil || addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range w.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
