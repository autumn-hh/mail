// Package mailauth 计算本机验证结果，不信任邮件自带的 Authentication-Results。
package mailauth

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/mail"
	"strings"
	"time"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
	"golang.org/x/net/publicsuffix"
	"maildrop/internal/mailparse"
)

type Verifier struct{ Resolver spf.DNSResolver }

func (v Verifier) Verify(ctx context.Context, raw []byte, ip net.IP, helo, sender string) *mailparse.Authentication {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	resolver := v.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	auth := &mailparse.Authentication{DKIM: []mailparse.AuthResult{}}
	spfResult, spfErr := spf.CheckHostWithSender(ip, helo, sender, spf.WithContext(ctx), spf.WithResolver(resolver))
	domain := addressDomain(sender)
	if domain == "" {
		domain = strings.TrimSuffix(strings.ToLower(helo), ".")
	}
	auth.SPF = mailparse.AuthResult{Result: string(spfResult), Domain: domain}
	if spfErr != nil && (spfResult == spf.TempError || spfResult == spf.PermError) {
		auth.SPF.Reason = spfErr.Error()
	}
	results, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{MaxVerifications: 5, LookupTXT: func(name string) ([]string, error) { return resolver.LookupTXT(ctx, name) }})
	for _, r := range results {
		state := "pass"
		reason := ""
		if r.Err != nil {
			state = "fail"
			reason = r.Err.Error()
			if dkim.IsTempFail(r.Err) {
				state = "temperror"
			} else if dkim.IsPermFail(r.Err) {
				state = "permerror"
			}
		}
		auth.DKIM = append(auth.DKIM, mailparse.AuthResult{Result: state, Domain: r.Domain, Reason: reason})
	}
	if err != nil {
		auth.DKIM = append(auth.DKIM, mailparse.AuthResult{Result: "permerror", Reason: err.Error()})
	}
	if len(auth.DKIM) == 0 {
		auth.DKIM = append(auth.DKIM, mailparse.AuthResult{Result: "none"})
	}
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		auth.DMARC = mailparse.AuthResult{Result: "permerror", Reason: "Invalid From header"}
		return auth
	}
	from, err := mail.ParseAddressList(message.Header.Get("From"))
	if err != nil || len(from) != 1 || len(message.Header["From"]) != 1 {
		auth.DMARC = mailparse.AuthResult{Result: "permerror", Reason: "From must contain one address"}
		return auth
	}
	fromDomain := addressDomain(from[0].Address)
	policyDomain := fromDomain
	record, status, reason := lookupPolicy(ctx, resolver, fromDomain)
	if status == "none" {
		org, e := publicsuffix.EffectiveTLDPlusOne(fromDomain)
		if e == nil && org != fromDomain {
			policyDomain = org
			record, status, reason = lookupPolicy(ctx, resolver, org)
		}
	}
	auth.DMARC = mailparse.AuthResult{Result: status, Domain: fromDomain, Reason: reason}
	if record == nil {
		return auth
	}
	auth.Policy = string(record.Policy)
	if policyDomain != fromDomain && record.SubdomainPolicy != "" {
		auth.Policy = string(record.SubdomainPolicy)
	}
	auth.DMARC.Result = "fail"
	alignedPass := spfResult == spf.Pass && aligned(domain, fromDomain, record.SPFAlignment)
	temporary := spfResult == spf.TempError
	for _, r := range auth.DKIM {
		if r.Result == "pass" && aligned(r.Domain, fromDomain, record.DKIMAlignment) {
			alignedPass = true
		}
		if r.Result == "temperror" {
			temporary = true
		}
	}
	if alignedPass {
		auth.DMARC.Result = "pass"
	} else if temporary {
		auth.DMARC.Result = "temperror"
	}
	return auth
}

func lookupPolicy(ctx context.Context, resolver spf.DNSResolver, domain string) (*dmarc.Record, string, string) {
	txts, err := resolver.LookupTXT(ctx, "_dmarc."+domain)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return nil, "none", ""
		}
		return nil, "temperror", "DMARC DNS lookup failed"
	}
	var records []string
	for _, txt := range txts {
		if strings.HasPrefix(strings.TrimSpace(txt), "v=DMARC1") {
			records = append(records, txt)
		}
	}
	if len(records) == 0 {
		return nil, "none", ""
	}
	if len(records) > 1 {
		return nil, "permerror", "Multiple DMARC records"
	}
	record, err := dmarc.Parse(records[0])
	if err != nil {
		return nil, "permerror", err.Error()
	}
	return record, "fail", ""
}
func aligned(a, b string, mode dmarc.AlignmentMode) bool {
	a, b = strings.ToLower(strings.TrimSuffix(a, ".")), strings.ToLower(strings.TrimSuffix(b, "."))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	if mode == dmarc.AlignmentStrict {
		return false
	}
	x, e := publicsuffix.EffectiveTLDPlusOne(a)
	y, f := publicsuffix.EffectiveTLDPlusOne(b)
	return e == nil && f == nil && x == y
}
func addressDomain(s string) string {
	_, d, ok := strings.Cut(s, "@")
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(d, "."))
}
