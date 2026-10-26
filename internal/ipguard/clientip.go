package ipguard

import (
	"net/http"
	"net/netip"
	"strings"
)

// ClientIP 取 XFF 倒数第 N 项，项数不足或选中地址无效则用连接地址。
// trustProxyHops 只能在入口经过固定可信代理时启用；返回归一化 IP。
func ClientIP(r *http.Request, trustProxyHops int) string {
	if r == nil {
		return "unknown"
	}
	remote := normalizeClientIP(r.RemoteAddr)
	if remote == "" {
		remote = "unknown"
	}
	if trustProxyHops <= 0 {
		return remote
	}
	values := parseForwardedFor(strings.Join(r.Header.Values("X-Forwarded-For"), ","))
	if len(values) >= trustProxyHops {
		if value := normalizeClientIP(values[len(values)-trustProxyHops]); value != "" {
			return value
		}
	}
	return remote
}

// 逗号始终分隔条目，引号不能改变可信跳数的位置。
func parseForwardedFor(value string) []string {
	items := strings.Split(value, ",")
	for i, item := range items {
		item = strings.TrimSpace(item)
		if len(item) >= 2 && item[0] == '"' && item[len(item)-1] == '"' {
			item = item[1 : len(item)-1]
		}
		items[i] = item
	}
	return items
}

// 校验 IP，去除端口、方括号和 zone，统一 IPv4-mapped 表示。
func normalizeClientIP(value string) string {
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		addr, err := netip.ParseAddr(value[1 : len(value)-1])
		if err != nil || !addr.Is6() {
			return ""
		}
		return addr.WithZone("").Unmap().String()
	}
	addr, err := netip.ParseAddr(value)
	if err != nil {
		addrPort, err := netip.ParseAddrPort(value)
		if err != nil {
			return ""
		}
		addr = addrPort.Addr()
	}
	return addr.WithZone("").Unmap().String()
}
