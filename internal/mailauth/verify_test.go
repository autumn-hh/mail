package mailauth

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/dmarc"
)

type resolver struct {
	*net.Resolver
	records     map[string][]string
	unavailable bool
}

func (r resolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if r.unavailable {
		return nil, &net.DNSError{Name: name, IsTemporary: true, Err: "temporary fixture failure"}
	}
	if value, ok := r.records[strings.TrimSuffix(name, ".")]; ok {
		return value, nil
	}
	return nil, &net.DNSError{Name: name, IsNotFound: true, Err: "not found"}
}
func TestAuthenticationMarksFailureAndDNSError(t *testing.T) {
	for _, tc := range []struct {
		name       string
		records    map[string][]string
		down       bool
		spf, dmarc string
	}{
		{"aligned SPF pass", map[string][]string{"sender.test": {"v=spf1 ip4:203.0.113.1 -all"}, "_dmarc.sender.test": {"v=DMARC1; p=reject"}}, false, "pass", "pass"},
		{"policy fails but result available", map[string][]string{"sender.test": {"v=spf1 -all"}, "_dmarc.sender.test": {"v=DMARC1; p=reject"}}, false, "fail", "fail"},
		{"DNS failure is recorded", nil, true, "temperror", "temperror"},
		{"no records", nil, false, "none", "none"},
		{"multiple policy records", map[string][]string{"_dmarc.sender.test": {"v=DMARC1; p=none", "v=DMARC1; p=reject"}}, false, "none", "permerror"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := (Verifier{Resolver: resolver{Resolver: net.DefaultResolver, records: tc.records, unavailable: tc.down}}).Verify(context.Background(), []byte("From: person@sender.test\r\nAuthentication-Results: attacker; spf=pass; dmarc=pass\r\n\r\nhello"), net.ParseIP("203.0.113.1"), "mx.sender.test", "person@sender.test")
			if result.SPF.Result != tc.spf || result.DMARC.Result != tc.dmarc || len(result.DKIM) != 1 || result.DKIM[0].Result != "none" {
				t.Fatalf("auth=%+v", result)
			}
		})
	}
}
func TestAlignmentAndInvalidFrom(t *testing.T) {
	if !aligned("mail.example.co.uk", "example.co.uk", dmarc.AlignmentRelaxed) || aligned("other.co.uk", "example.co.uk", dmarc.AlignmentRelaxed) || aligned("mail.example.com", "example.com", dmarc.AlignmentStrict) {
		t.Fatal("domain alignment")
	}
	result := (Verifier{Resolver: resolver{Resolver: net.DefaultResolver}}).Verify(context.Background(), []byte("From: a@sender.test\r\nFrom: b@sender.test\r\n\r\nbody"), net.ParseIP("203.0.113.1"), "mx.sender.test", "a@sender.test")
	if result.DMARC.Result != "permerror" {
		t.Fatal(result)
	}
}
