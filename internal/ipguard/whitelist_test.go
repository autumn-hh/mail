package ipguard

import "testing"

func TestWhitelist(t *testing.T) {
	for _, tc := range []struct {
		name, entries, ip string
		enabled, want     bool
	}{
		{"IPv4", "192.0.2.1", "192.0.2.1", true, true},
		{"IPv4 denied", "192.0.2.1", "192.0.2.2", true, false},
		{"IPv6", "2001:db8::1", "2001:0db8:0:0:0:0:0:1", true, true},
		{"IPv4 CIDR host bits", "192.0.2.99/24", "192.0.2.255", true, true},
		{"IPv4 outside CIDR", "192.0.2.99/24", "192.0.3.1", true, false},
		{"IPv6 CIDR host bits", "2001:db8::42/32", "2001:db8:abcd::1", true, true},
		{"IPv6 outside CIDR", "2001:db8::/32", "2001:db9::1", true, false},
		{"IPv4 mapped client", "192.0.2.0/24", "::ffff:192.0.2.1", true, true},
		{"IPv4 mapped rule", "::ffff:192.0.2.1", "192.0.2.1", true, true},
		{"IPv4 mapped CIDR", "::ffff:192.0.2.99/120", "192.0.2.1", true, true},
		{"mapped CIDR outside", "::ffff:192.0.2.0/120", "::ffff:192.0.3.1", true, false},
		{"malformed rule skipped", "bad,192.0.2.1", "192.0.2.1", true, true},
		{"malformed CIDR skipped", "192.0.2.0/99,2001:db8::1", "2001:db8::1", true, true},
		{"only malformed", "bad,also bad/99", "192.0.2.1", true, false},
		{"empty enabled", "", "127.0.0.1", true, false},
		{"spaces and empties", " , 192.0.2.1, ,", "192.0.2.1", true, true},
		{"malformed client", "0.0.0.0/0,::/0", "bad", true, false},
		{"unknown denied", "0.0.0.0/0,::/0", "unknown", true, false},
		{"localhost denied", "0.0.0.0/0,::/0", "localhost", true, false},
		{"loopback not implicit", "192.0.2.1", "::1", true, false},
		{"zone denied", "fe80::/10", "fe80::1%eth0", true, false},
		{"bad zone rule skipped", "fe80::1%eth0,192.0.2.1", "192.0.2.1", true, true},
		{"disabled", "bad", "192.0.2.1", false, true},
		{"disabled invalid client", "", "unknown", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := NewWhitelist(tc.enabled, tc.entries)
			if got := w.IsIPWhitelisted(tc.ip); got != tc.want {
				t.Fatalf("IsIPWhitelisted(%q) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}
