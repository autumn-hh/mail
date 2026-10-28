package ipguard

import (
	"net/http"
	"testing"
)

func TestClientIP(t *testing.T) {
	for _, tc := range []struct {
		name, remote, xff string
		hops              int
		want              string
	}{
		{"no proxy ignores spoof", "192.0.2.10:4321", "198.51.100.1", 0, "192.0.2.10"},
		{"negative hops", "192.0.2.10:4321", "198.51.100.1", -1, "192.0.2.10"},
		{"one proxy", "127.0.0.1:8080", "198.51.100.1, 192.0.2.1", 1, "192.0.2.1"},
		{"two proxies", "127.0.0.1:8080", "203.0.113.1, 198.51.100.1, 192.0.2.1", 2, "198.51.100.1"},
		{"exactly two entries", "127.0.0.1:8080", "198.51.100.1, 192.0.2.1", 2, "198.51.100.1"},
		{"short list falls back", "192.0.2.10:4321", "198.51.100.1", 2, "192.0.2.10"},
		{"short list normalizes remote", "[::ffff:192.0.2.10]:4321", "198.51.100.1", 2, "192.0.2.10"},
		{"empty one hop", "192.0.2.10:4321", "", 1, "192.0.2.10"},
		{"empty two hops", "192.0.2.10:4321", "", 2, "192.0.2.10"},
		{"IPv6 remote", "[2001:db8::10]:4321", "ignored", 0, "2001:db8::10"},
		{"bare IPv6 remote", "2001:db8::10", "", 0, "2001:db8::10"},
		{"bracketed IPv6 remote", "[2001:db8::10]", "", 0, "2001:db8::10"},
		{"canonical IPv6 remote", "[2001:0DB8:0:0:0:0:0:0010]:4321", "", 0, "2001:db8::10"},
		{"mapped remote", "[::ffff:192.0.2.10]:4321", "", 0, "192.0.2.10"},
		{"zoned remote", "[fe80::10%eth0]:4321", "", 0, "fe80::10"},
		{"mapped zoned remote", "[::ffff:192.0.2.10%eth0]:4321", "", 0, "192.0.2.10"},
		{"IPv6 XFF", "127.0.0.1:8080", "2001:db8::10, 2001:db8::20", 2, "2001:db8::10"},
		{"quoted entry", "127.0.0.1:8080", `"198.51.100.1", "192.0.2.1"`, 2, "198.51.100.1"},
		{"trim before stripping quotes", "127.0.0.1:8080", " \t\"198.51.100.1\" \t", 1, "198.51.100.1"},
		{"quoted canonical IPv6", "127.0.0.1:8080", `"2001:0DB8:0:0:0:0:0:0010"`, 1, "2001:db8::10"},
		{"bracketed IPv6 XFF", "127.0.0.1:8080", "[2001:db8::10]", 1, "2001:db8::10"},
		{"IPv4 port XFF", "127.0.0.1:8080", "198.51.100.1:4321", 1, "198.51.100.1"},
		{"IPv6 port XFF", "127.0.0.1:8080", "[2001:db8::10]:4321", 1, "2001:db8::10"},
		{"quoted bracketed IPv6", "127.0.0.1:8080", `"[2001:db8::10]"`, 1, "2001:db8::10"},
		{"quoted IPv4 port", "127.0.0.1:8080", `"198.51.100.1:4321"`, 1, "198.51.100.1"},
		{"quoted IPv6 port", "127.0.0.1:8080", `"[2001:db8::10]:4321"`, 1, "2001:db8::10"},
		{"bare IPv6 ending in digits", "127.0.0.1:8080", "2001:db8::10:4321", 1, "2001:db8::10:4321"},
		{"mapped XFF", "127.0.0.1:8080", "::ffff:c633:6401", 1, "198.51.100.1"},
		{"mapped port XFF", "127.0.0.1:8080", "[::ffff:198.51.100.1]:4321", 1, "198.51.100.1"},
		{"zoned XFF", "127.0.0.1:8080", "fe80::10%eth0", 1, "fe80::10"},
		{"bracketed zoned XFF", "127.0.0.1:8080", "[fe80::10%eth0]", 1, "fe80::10"},
		{"zoned port XFF", "127.0.0.1:8080", "[fe80::10%eth0]:4321", 1, "fe80::10"},
		{"mapped zoned XFF", "127.0.0.1:8080", "::ffff:198.51.100.1%eth0", 1, "198.51.100.1"},
		{"garbage left of selection ignored", "127.0.0.1:8080", "not-an-ip, 203.0.113.9", 1, "203.0.113.9"},
		{"garbage left of two hop selection ignored", "127.0.0.1:8080", "not-an-ip, 198.51.100.1, 203.0.113.9", 2, "198.51.100.1"},
		{"quoted comma still splits", "127.0.0.1:8080", `"a,b",192.0.2.1`, 2, "127.0.0.1"},
		{"single quote falls back", "127.0.0.1:8080", `"`, 1, "127.0.0.1"},
		{"unclosed selected quote falls back", "127.0.0.1:8080", `"198.51.100.1`, 1, "127.0.0.1"},
		{"closing selected quote falls back", "127.0.0.1:8080", `198.51.100.1"`, 1, "127.0.0.1"},
		{"only one quote pair stripped", "127.0.0.1:8080", `""198.51.100.1""`, 1, "127.0.0.1"},
		{"backslash is not unescaped", "127.0.0.1:8080", `"198.51.100.\1"`, 1, "127.0.0.1"},
		{"selected empty falls back", "127.0.0.1:8080", `198.51.100.1,""`, 1, "127.0.0.1"},
		{"leading empty counts", "127.0.0.1:8080", ",198.51.100.1", 2, "127.0.0.1"},
		{"trailing empty falls back", "127.0.0.1:8080", "198.51.100.1,", 1, "127.0.0.1"},
		{"trailing empty counts", "127.0.0.1:8080", "198.51.100.1,", 2, "198.51.100.1"},
		{"trailing whitespace falls back", "127.0.0.1:8080", "198.51.100.1, ", 1, "127.0.0.1"},
		{"trailing whitespace entry counts", "127.0.0.1:8080", "198.51.100.1, ", 2, "198.51.100.1"},
		{"garbage falls back", "127.0.0.1:8080", "not-an-ip", 1, "127.0.0.1"},
		{"invalid selected item never scans left", "127.0.0.1:8080", "198.51.100.1, not-an-ip", 1, "127.0.0.1"},
		{"invalid two hop selection never scans left", "127.0.0.1:8080", "198.51.100.1, not-an-ip, 192.0.2.1", 2, "127.0.0.1"},
		{"invalid selection normalizes remote", "[fe80::10%eth0]:4321", "not-an-ip", 1, "fe80::10"},
		{"hostname falls back", "127.0.0.1:8080", "example.com:4321", 1, "127.0.0.1"},
		{"invalid IPv4 falls back", "127.0.0.1:8080", "999.51.100.1", 1, "127.0.0.1"},
		{"CIDR falls back", "127.0.0.1:8080", "198.51.100.0/24", 1, "127.0.0.1"},
		{"invalid port falls back", "127.0.0.1:8080", "198.51.100.1:garbage", 1, "127.0.0.1"},
		{"out of range port falls back", "127.0.0.1:8080", "[2001:db8::10]:65536", 1, "127.0.0.1"},
		{"bracketed IPv4 falls back", "127.0.0.1:8080", "[198.51.100.1]", 1, "127.0.0.1"},
		{"empty remote", "", "", 0, "unknown"},
		{"invalid remote", "not-an-ip", "", 0, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{RemoteAddr: tc.remote, Header: make(http.Header)}
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := ClientIP(r, tc.hops); got != tc.want {
				t.Fatalf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
	if got := ClientIP(nil, 2); got != "unknown" {
		t.Fatalf("nil request = %q", got)
	}
}

func TestClientIPMultipleHeaderLines(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		hops   int
		want   string
	}{
		{"one hop", []string{"198.51.100.1", "192.0.2.1"}, 1, "192.0.2.1"},
		{"two hops", []string{"198.51.100.1", "192.0.2.1"}, 2, "198.51.100.1"},
		{"commas across lines", []string{"203.0.113.9, 198.51.100.1", "192.0.2.1"}, 2, "198.51.100.1"},
		{"short list", []string{"198.51.100.1", "192.0.2.1"}, 3, "127.0.0.1"},
		{"empty final line falls back", []string{"198.51.100.1", ""}, 1, "127.0.0.1"},
		{"empty final line counts", []string{"198.51.100.1", ""}, 2, "198.51.100.1"},
		{"unclosed quote cannot span lines", []string{`"nonce1`, "203.0.113.9"}, 1, "203.0.113.9"},
		{"unclosed quote with comma cannot span lines", []string{`"nonce2, 198.51.100.1`, "203.0.113.9"}, 1, "203.0.113.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{RemoteAddr: "127.0.0.1:8080", Header: make(http.Header)}
			for _, value := range tc.values {
				r.Header.Add("X-Forwarded-For", value)
			}
			if got := ClientIP(r, tc.hops); got != tc.want {
				t.Fatalf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClientIPUnclosedQuoteCannotBypassBlocker(t *testing.T) {
	clock := newFakeClock()
	blocker := NewBlockerWithClock(60, 2, 30, clock.Now)
	for i, xff := range []string{`"nonce1, 203.0.113.9`, `"nonce2, 203.0.113.9`} {
		r := &http.Request{RemoteAddr: "127.0.0.1:8080", Header: make(http.Header)}
		r.Header.Set("X-Forwarded-For", xff)
		ip := ClientIP(r, 1)
		if ip != "203.0.113.9" {
			t.Fatalf("ClientIP(%q) = %q, want 203.0.113.9", xff, ip)
		}
		if got, want := blocker.RecordFailedAttempt(ip), i == 1; got != want {
			t.Fatalf("attempt %d blocked = %v, want %v", i+1, got, want)
		}
	}
	r := &http.Request{RemoteAddr: "127.0.0.1:8080", Header: make(http.Header)}
	r.Header.Set("X-Forwarded-For", `"nonce3, 203.0.113.9`)
	if !blocker.IsBlocked(ClientIP(r, 1)) {
		t.Fatal("new nonce bypassed the existing client IP block")
	}
	if got := len(blocker.GetBlockedIPs()); got != 1 {
		t.Fatalf("blocked IP count = %d, want 1", got)
	}
}
