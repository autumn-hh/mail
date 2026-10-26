package addrpolicy

import (
	"errors"
	"testing"
)

func TestProtected(t *testing.T) {
	for _, tc := range []struct {
		pattern, address string
		want             error
	}{
		{"^admin.*", "admin@example.com", ErrProtected},
		{"^admin.*", "admin123@example.com", ErrProtected},
		{"^admin.*", "user@example.com", nil},
		// 与 re.match 一致：只在开头匹配，中间出现不算。
		{"admin", "myadmin@example.com", nil},
		{"admin", "adminx@example.com", ErrProtected},
		{"postmaster|abuse", "abuse@example.com", ErrProtected},
		{"", "admin@example.com", nil},
		{"(", "user@example.com", ErrInvalidPattern},
	} {
		if got := NewProtected(tc.pattern).Check(tc.address); !errors.Is(got, tc.want) {
			t.Errorf("pattern %q address %q: got %v want %v", tc.pattern, tc.address, got, tc.want)
		}
	}
	var nilPolicy *Protected
	if nilPolicy.Check("admin@example.com") != nil {
		t.Fatal("nil policy must allow")
	}
}
