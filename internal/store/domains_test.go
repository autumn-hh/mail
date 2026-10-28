package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNormalizeDomain(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{" Example.COM. ", "example.com"}, {"LOCALHOST.", "localhost"}, {"例子.测试", "xn--fsqu00a.xn--0zwm56d"}, {"BÜCHER.DE", "xn--bcher-kva.de"}, {"xn--bcher-kva.de", "xn--bcher-kva.de"}, {"a-b.example", "a-b.example"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := NormalizeDomain(tc.input)
			if err != nil || got != tc.want {
				t.Fatal(got, err)
			}
		})
	}
	for _, value := range []string{"", " ", "example", ".", "a..test", "example.com..", "-a.test", "a-.test", "a_b.test", "https://a.test", "a.test:25", "a@test.com", "127.0.0.1", "127.0.0.1.", "::1", "[::1]", "bad\n.test", strings.Repeat("a", 64) + ".test", strings.Repeat(strings.Repeat("a", 63)+".", 4) + "a"} {
		if _, err := NormalizeDomain(value); !errors.Is(err, ErrInvalidDomain) {
			t.Errorf("accepted %q: %v", value, err)
		}
	}
	max := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	if got, err := NormalizeDomain(max); err != nil || len(got) != 253 {
		t.Fatal("253 bytes", len(got), err)
	}
}
func TestDomainsCRUDAndSeeding(t *testing.T) {
	s := testStore(t)
	now := time.Unix(1800000000, 0)
	s.now = func() time.Time { return now }
	if err := s.SeedDomainsIfEmpty([]string{" Example.test. ", "example.test", "localhost", "例子.测试"}); err != nil {
		t.Fatal(err)
	}
	domains, err := s.ListDomains()
	if err != nil || len(domains) != 3 {
		t.Fatal(domains, err)
	}
	if domains[0].Domain != "example.test" || domains[1].Domain != "localhost" || domains[2].Domain != "xn--fsqu00a.xn--0zwm56d" {
		t.Fatal("order", domains)
	}
	if err := s.SeedDomainsIfEmpty([]string{"ignored.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddDomain("EXAMPLE.TEST."); !errors.Is(err, ErrDomainExists) {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	added, err := s.AddDomain("new.test")
	if err != nil || !added.Enabled || added.CreatedAt != now.Unix() {
		t.Fatal(added, err)
	}
	if err := s.SetDomainEnabled("new.test", false); err != nil {
		t.Fatal(err)
	}
	enabled, err := s.ListEnabledDomains()
	if err != nil || len(enabled) != 3 {
		t.Fatal(enabled, err)
	}
	if err := s.DeleteDomain("new.test"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDomainEnabled("example.test", false); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDomain("localhost"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDomain("例子.测试"); !errors.Is(err, ErrLastEnabledDomain) {
		t.Fatal(err)
	}
	if err := s.SetDomainEnabled("例子.测试", false); !errors.Is(err, ErrLastEnabledDomain) {
		t.Fatal(err)
	}
	if err := s.DeleteDomain("example.test"); err != nil {
		t.Fatal("delete disabled", err)
	}
	if err := s.SetDomainEnabled("missing.test", false); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.DeleteDomain("missing.test"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.SetDomainEnabled("例子.测试", true); err != nil {
		t.Fatal("enable last idempotently", err)
	}
}
func TestSeedingRollback(t *testing.T) {
	s := testStore(t)
	if err := s.SeedDomainsIfEmpty([]string{"good.test", "invalid"}); !errors.Is(err, ErrInvalidDomain) {
		t.Fatal(err)
	}
	if err := s.SeedDomainsIfEmpty(nil); !errors.Is(err, ErrInvalidDomain) {
		t.Fatal(err)
	}
	domains, _ := s.ListDomains()
	if len(domains) != 0 {
		t.Fatal("partial seed persisted")
	}
}

// 播种在同一秒完成，列表必须保持 DOMAINS 的书写顺序，首个域名才是默认域名。
func TestSeedingKeepsConfiguredOrder(t *testing.T) {
	s := testStore(t)
	if err := s.SeedDomainsIfEmpty([]string{"zeta.test", "alpha.test", "mid.test"}); err != nil {
		t.Fatal(err)
	}
	for name, list := range map[string]func() ([]Domain, error){"all": s.ListDomains, "enabled": s.ListEnabledDomains} {
		domains, err := list()
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0, len(domains))
		for _, d := range domains {
			got = append(got, d.Domain)
		}
		if strings.Join(got, ",") != "zeta.test,alpha.test,mid.test" {
			t.Fatalf("%s order = %v", name, got)
		}
	}
}

func TestDomainGuardConcurrent(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(fmt.Sprint(remove), func(t *testing.T) {
			s := testStore(t)
			const count = 20
			for i := range count {
				if _, err := s.AddDomain(fmt.Sprintf("%d.test", i)); err != nil {
					t.Fatal(err)
				}
			}
			var wg sync.WaitGroup
			errs := make(chan error, count)
			for i := range count {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					domain := fmt.Sprintf("%d.test", i)
					if remove {
						errs <- s.DeleteDomain(domain)
					} else {
						errs <- s.SetDomainEnabled(domain, false)
					}
				}(i)
			}
			wg.Wait()
			close(errs)
			guarded := 0
			for err := range errs {
				if errors.Is(err, ErrLastEnabledDomain) {
					guarded++
				} else if err != nil {
					t.Error(err)
				}
			}
			enabled, err := s.ListEnabledDomains()
			if err != nil || len(enabled) != 1 || guarded != 1 {
				t.Fatal(enabled, guarded, err)
			}
		})
	}
}
func TestAddDomainsOrderNormalizationAndSkips(t *testing.T) {
	s := testStore(t)
	now := time.Unix(1800000000, 0)
	s.now = func() time.Time { return now }
	existing, err := s.AddDomain("existing.test")
	if err != nil {
		t.Fatal(err)
	}
	added, skipped, err := s.AddDomains([]string{
		" ZETA.TEST. ", " EXISTING.TEST. ", "bad/domain", "zeta.test", "B\u00dcCHER.DE.",
		"xn--bcher-kva.de", "alpha.test", "existing.test", " \u4f8b\u5b50.\u6d4b\u8bd5 ",
	})
	want := []Domain{
		{Domain: "zeta.test", Enabled: true, CreatedAt: now.Unix(), UpdatedAt: now.Unix()},
		{Domain: "xn--bcher-kva.de", Enabled: true, CreatedAt: now.Unix(), UpdatedAt: now.Unix()},
		{Domain: "alpha.test", Enabled: true, CreatedAt: now.Unix(), UpdatedAt: now.Unix()},
		{Domain: "xn--fsqu00a.xn--0zwm56d", Enabled: true, CreatedAt: now.Unix(), UpdatedAt: now.Unix()},
	}
	wantSkipped := []DomainSkip{
		{Domain: "EXISTING.TEST.", Reason: "域名已存在"},
		{Domain: "bad/domain", Reason: "域名格式不正确"},
		{Domain: "zeta.test", Reason: "重复输入"},
		{Domain: "xn--bcher-kva.de", Reason: "重复输入"},
		{Domain: "existing.test", Reason: "重复输入"},
	}
	if err != nil || !reflect.DeepEqual(added, want) || !reflect.DeepEqual(skipped, wantSkipped) {
		t.Fatalf("add = %#v, skips = %#v, error = %v", added, skipped, err)
	}
	all, err := s.ListDomains()
	if err != nil || !reflect.DeepEqual(all, append([]Domain{*existing}, want...)) {
		t.Fatalf("persisted order = %#v, error = %v", all, err)
	}
	added, skipped, err = s.AddDomains([]string{" ZETA.TEST. ", "invalid"})
	if err != nil || added == nil || len(added) != 0 || len(skipped) != 2 {
		t.Fatalf("all skipped = %#v, %#v, %v", added, skipped, err)
	}
	added, skipped, err = s.AddDomains([]string{"fresh.test"})
	if err != nil || len(added) != 1 || skipped == nil || len(skipped) != 0 {
		t.Fatalf("all added = %#v, %#v, %v", added, skipped, err)
	}
	added, skipped, err = s.AddDomains(nil)
	if err != nil || added == nil || skipped == nil || len(added) != 0 || len(skipped) != 0 {
		t.Fatalf("empty add = %#v, %#v, %v", added, skipped, err)
	}
}

func TestAddDomainsRollbackOnDatabaseError(t *testing.T) {
	s := testStore(t)
	if _, err := s.AddDomain("keeper.test"); err != nil {
		t.Fatal(err)
	}
	before, err := s.ListDomains()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_batch_add BEFORE INSERT ON domains
		WHEN NEW.domain = 'reject.test' BEGIN SELECT RAISE(ABORT, 'insert unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	added, skipped, err := s.AddDomains([]string{"first.test", "reject.test", "last.test"})
	if err == nil || added != nil || skipped != nil {
		t.Fatalf("failed add = %#v, %#v, %v", added, skipped, err)
	}
	after, err := s.ListDomains()
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("failed insert changed domains: %#v, %v", after, err)
	}
}

func TestDeleteDomainsOrderNormalizationAndFailures(t *testing.T) {
	s := testStore(t)
	if err := s.SeedDomainsIfEmpty([]string{"keeper.test", "alpha.test", "xn--bcher-kva.de", "disabled.test", "zeta.test"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDomainEnabled("disabled.test", false); err != nil {
		t.Fatal(err)
	}
	deleted, failed, err := s.DeleteDomains([]string{
		" ZETA.TEST. ", " missing.test ", "bad/domain", "zeta.test", " B\u00dcCHER.DE. ",
		"MISSING.TEST.", "xn--bcher-kva.de", "alpha.test", "disabled.test",
	})
	wantFailed := []DomainSkip{
		{Domain: " missing.test ", Reason: "域名不存在"},
		{Domain: "bad/domain", Reason: "域名不存在"},
	}
	if err != nil || !reflect.DeepEqual(deleted, []string{"zeta.test", "xn--bcher-kva.de", "alpha.test", "disabled.test"}) || !reflect.DeepEqual(failed, wantFailed) {
		t.Fatalf("delete = %#v, failed = %#v, error = %v", deleted, failed, err)
	}
	all, err := s.ListDomains()
	if err != nil || len(all) != 1 || all[0].Domain != "keeper.test" || !all[0].Enabled {
		t.Fatalf("remaining domains = %#v, %v", all, err)
	}
	deleted, failed, err = s.DeleteDomains([]string{"missing.test", "MISSING.TEST.", "invalid"})
	if err != nil || deleted == nil || len(deleted) != 0 || len(failed) != 2 {
		t.Fatalf("all missing = %#v, %#v, %v", deleted, failed, err)
	}
	deleted, failed, err = s.DeleteDomains(nil)
	if err != nil || deleted == nil || failed == nil || len(deleted) != 0 || len(failed) != 0 {
		t.Fatalf("empty delete = %#v, %#v, %v", deleted, failed, err)
	}
	if _, err := s.AddDomain("fresh.test"); err != nil {
		t.Fatal(err)
	}
	deleted, failed, err = s.DeleteDomains([]string{" FRESH.TEST. ", "fresh.test"})
	if err != nil || !reflect.DeepEqual(deleted, []string{"fresh.test"}) || failed == nil || len(failed) != 0 {
		t.Fatalf("all found = %#v, %#v, %v", deleted, failed, err)
	}
}

func TestDeleteDomainsGuardRejectsWholeBatch(t *testing.T) {
	s := testStore(t)
	if err := s.SeedDomainsIfEmpty([]string{"first.test", "second.test", "disabled.test"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDomainEnabled("disabled.test", false); err != nil {
		t.Fatal(err)
	}
	before, err := s.ListDomains()
	if err != nil {
		t.Fatal(err)
	}
	deleted, failed, err := s.DeleteDomains([]string{"disabled.test", " FIRST.TEST. ", "missing.test", "second.test", "first.test"})
	if !errors.Is(err, ErrLastEnabledDomain) || deleted != nil || failed != nil {
		t.Fatalf("guard = %#v, %#v, %v", deleted, failed, err)
	}
	after, err := s.ListDomains()
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("guard changed domains: %#v, %v", after, err)
	}
	deleted, failed, err = s.DeleteDomains([]string{"first.test", "FIRST.TEST."})
	if err != nil || !reflect.DeepEqual(deleted, []string{"first.test"}) || failed == nil || len(failed) != 0 {
		t.Fatalf("duplicates counted against guard = %#v, %#v, %v", deleted, failed, err)
	}
}

func TestDeleteDomainsRollbackOnDatabaseError(t *testing.T) {
	s := testStore(t)
	if err := s.SeedDomainsIfEmpty([]string{"keeper.test", "first.test", "reject.test", "last.test"}); err != nil {
		t.Fatal(err)
	}
	before, err := s.ListDomains()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_batch_delete BEFORE DELETE ON domains
		WHEN OLD.domain = 'reject.test' BEGIN SELECT RAISE(ABORT, 'delete unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	deleted, failed, err := s.DeleteDomains([]string{"first.test", "reject.test", "last.test"})
	if err == nil || deleted != nil || failed != nil {
		t.Fatalf("failed delete = %#v, %#v, %v", deleted, failed, err)
	}
	after, err := s.ListDomains()
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("failed delete changed domains: %#v, %v", after, err)
	}
}
