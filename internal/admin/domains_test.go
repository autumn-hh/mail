package admin_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"maildrop/internal/admin"
	"maildrop/internal/config"
	"maildrop/internal/store"
)

func newDomainTestService(t *testing.T) (*admin.Service, *store.Store) {
	t.Helper()
	cfg := config.FromEnv(func(string) string { return "" })
	db, err := store.OpenWithConfig(filepath.Join(t.TempDir(), "domains.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return admin.New(cfg, db, func() time.Time { return time.Unix(1800000000, 0) }), db
}

type domainAuditExpectation struct {
	action  string
	changes map[string]any
}

func assertDomainAudits(t *testing.T, db *store.Store, actor admin.Actor, want []domainAuditExpectation) {
	t.Helper()
	rows, err := db.DB().Query(`SELECT action, mailbox_id, changes, admin_user, ip_address, timestamp
		FROM audit_logs ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []domainAuditExpectation
	for rows.Next() {
		var action, changes string
		var mailboxID, user, ip sql.NullString
		var timestamp int64
		if err := rows.Scan(&action, &mailboxID, &changes, &user, &ip, &timestamp); err != nil {
			t.Fatal(err)
		}
		if mailboxID.Valid {
			t.Errorf("%s mailbox_id = %q, want SQL NULL", action, mailboxID.String)
		}
		if !user.Valid || user.String != actor.User || !ip.Valid || ip.String != actor.IP {
			t.Errorf("%s actor = (%v, %v), want %+v", action, user, ip, actor)
		}
		if timestamp != 1800000000 {
			t.Errorf("%s timestamp = %d, want injected clock", action, timestamp)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(changes), &decoded); err != nil {
			t.Fatal(err)
		}
		got = append(got, domainAuditExpectation{action, decoded})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("audits = %#v, want %#v", got, want)
	}
}

func TestDomainServiceCRUDAndAudit(t *testing.T) {
	svc, db := newDomainTestService(t)
	ctx := context.Background()
	actor := admin.Actor{User: "domain-admin", IP: "192.0.2.10"}
	domains, err := svc.ListDomains()
	if err != nil || domains == nil || len(domains) != 0 {
		t.Fatalf("empty list = %#v, %v", domains, err)
	}
	first, err := svc.AddDomain(ctx, " Example.TEST. ", actor)
	if err != nil || first == nil || first.Domain != "example.test" || !first.Enabled || first.CreatedAt == 0 {
		t.Fatalf("add = %#v, %v", first, err)
	}
	idn, err := svc.AddDomain(ctx, " B\u00dcCHER.DE. ", actor)
	if err != nil || idn == nil || idn.Domain != "xn--bcher-kva.de" || !idn.Enabled {
		t.Fatalf("add IDN = %#v, %v", idn, err)
	}
	domains, err = svc.ListDomains()
	if err != nil || len(domains) != 2 || domains[0] != *first || domains[1] != *idn {
		t.Fatalf("list = %#v, %v", domains, err)
	}
	disabled, err := svc.SetDomainEnabled(ctx, " B\u00dcCHER.DE. ", false, actor)
	if err != nil || disabled == nil || disabled.Domain != idn.Domain || disabled.Enabled || disabled.CreatedAt != idn.CreatedAt || disabled.UpdatedAt < idn.UpdatedAt {
		t.Fatalf("disable IDN = %#v, %v", disabled, err)
	}
	enabled, err := svc.SetDomainEnabled(ctx, " XN--BCHER-KVA.DE. ", true, actor)
	if err != nil || enabled == nil || !enabled.Enabled || enabled.Domain != idn.Domain {
		t.Fatalf("enable IDN = %#v, %v", enabled, err)
	}
	if err := svc.DeleteDomain(ctx, " B\u00dcCHER.DE. ", actor); err != nil {
		t.Fatal(err)
	}
	domains, err = svc.ListDomains()
	if err != nil || len(domains) != 1 || domains[0] != *first {
		t.Fatalf("after delete = %#v, %v", domains, err)
	}
	assertDomainAudits(t, db, actor, []domainAuditExpectation{
		{"ADD_DOMAIN", map[string]any{"domain": "example.test", "enabled": true}},
		{"ADD_DOMAIN", map[string]any{"domain": "xn--bcher-kva.de", "enabled": true}},
		{"UPDATE_DOMAIN", map[string]any{"domain": "xn--bcher-kva.de", "enabled": false}},
		{"UPDATE_DOMAIN", map[string]any{"domain": "xn--bcher-kva.de", "enabled": true}},
		{"DELETE_DOMAIN", map[string]any{"domain": "xn--bcher-kva.de"}},
	})
}

func TestDomainServiceErrorsAndLastEnabledGuard(t *testing.T) {
	svc, db := newDomainTestService(t)
	ctx := context.Background()
	actor := admin.Actor{User: "domain-admin", IP: "192.0.2.11"}
	if _, err := db.AddDomain("sole.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddDomain(ctx, " SOLE.TEST. ", actor); !errors.Is(err, store.ErrDomainExists) {
		t.Fatalf("duplicate add = %v", err)
	}
	if _, err := svc.AddDomain(ctx, "invalid", actor); !errors.Is(err, store.ErrInvalidDomain) {
		t.Fatalf("invalid add = %v", err)
	}
	for _, value := range []string{"missing.test", "", "invalid", "https://sole.test", "127.0.0.1"} {
		if domain, err := svc.SetDomainEnabled(ctx, value, false, actor); domain != nil || !errors.Is(err, store.ErrNotFound) {
			t.Errorf("update %q = %#v, %v; want not found", value, domain, err)
		}
		if err := svc.DeleteDomain(ctx, value, actor); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("delete %q = %v; want not found", value, err)
		}
	}
	if domain, err := svc.SetDomainEnabled(ctx, " SOLE.TEST. ", false, actor); domain != nil || !errors.Is(err, store.ErrLastEnabledDomain) {
		t.Fatalf("disable last = %#v, %v", domain, err)
	}
	if err := svc.DeleteDomain(ctx, " SOLE.TEST. ", actor); !errors.Is(err, store.ErrLastEnabledDomain) {
		t.Fatalf("delete last = %v", err)
	}
	assertDomainAudits(t, db, actor, nil)

	// Enabling an already enabled sole domain must still succeed and be audited.
	if domain, err := svc.SetDomainEnabled(ctx, " SOLE.TEST. ", true, actor); err != nil || domain == nil || !domain.Enabled {
		t.Fatalf("enable last = %#v, %v", domain, err)
	}
	if _, err := db.AddDomain("disabled.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetDomainEnabled(ctx, "disabled.test", false, actor); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteDomain(ctx, " DISABLED.TEST. ", actor); err != nil {
		t.Fatalf("delete disabled = %v", err)
	}
	domains, err := svc.ListDomains()
	if err != nil || len(domains) != 1 || domains[0].Domain != "sole.test" || !domains[0].Enabled {
		t.Fatalf("guarded domains = %#v, %v", domains, err)
	}
	assertDomainAudits(t, db, actor, []domainAuditExpectation{
		{"UPDATE_DOMAIN", map[string]any{"domain": "sole.test", "enabled": true}},
		{"UPDATE_DOMAIN", map[string]any{"domain": "disabled.test", "enabled": false}},
		{"DELETE_DOMAIN", map[string]any{"domain": "disabled.test"}},
	})
}

func TestDomainServiceAuditFailureLeavesCommittedChange(t *testing.T) {
	for _, operation := range []string{"add", "update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			svc, db := newDomainTestService(t)
			for _, value := range []string{"keeper.test", "target.test"} {
				if _, err := db.AddDomain(value); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.DB().Exec(`CREATE TRIGGER reject_domain_audit BEFORE INSERT ON audit_logs
				BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			actor := admin.Actor{User: "domain-admin", IP: "192.0.2.12"}
			var err error
			target, wantPresent, wantEnabled := "target.test", true, true
			switch operation {
			case "add":
				target = "added.test"
				_, err = svc.AddDomain(ctx, target, actor)
			case "update":
				wantEnabled = false
				_, err = svc.SetDomainEnabled(ctx, target, false, actor)
			case "delete":
				wantPresent = false
				err = svc.DeleteDomain(ctx, target, actor)
			}
			if err == nil {
				t.Fatal("audit failure was not returned")
			}
			domains, err := svc.ListDomains()
			if err != nil {
				t.Fatal(err)
			}
			present := false
			for _, domain := range domains {
				if domain.Domain == target {
					present = true
					if domain.Enabled != wantEnabled {
						t.Errorf("committed enabled = %v, want %v", domain.Enabled, wantEnabled)
					}
				}
			}
			if present != wantPresent {
				t.Errorf("committed presence = %v, want %v", present, wantPresent)
			}
			assertDomainAudits(t, db, actor, nil)
		})
	}
}
func TestDomainServiceBatchesAuditOncePerChange(t *testing.T) {
	svc, db := newDomainTestService(t)
	ctx := context.Background()
	actor := admin.Actor{User: "domain-admin", IP: "192.0.2.13"}
	if _, err := db.AddDomain("keeper.test"); err != nil {
		t.Fatal(err)
	}
	added, skipped, err := svc.AddDomains(ctx, []string{" A.TEST. ", "keeper.test", "invalid", "a.test", "B\u00dcCHER.DE."}, actor)
	if err != nil || len(added) != 2 || added[0].Domain != "a.test" || added[1].Domain != "xn--bcher-kva.de" || len(skipped) != 3 {
		t.Fatalf("batch add = %#v, %#v, %v", added, skipped, err)
	}
	added, skipped, err = svc.AddDomains(ctx, []string{"a.test", "invalid"}, actor)
	if err != nil || added == nil || len(added) != 0 || len(skipped) != 2 {
		t.Fatalf("unchanged add = %#v, %#v, %v", added, skipped, err)
	}
	before, err := svc.ListDomains()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.DeleteDomains(ctx, []string{"keeper.test", "a.test", "xn--bcher-kva.de"}, actor); !errors.Is(err, store.ErrLastEnabledDomain) {
		t.Fatalf("batch guard = %v", err)
	}
	after, err := svc.ListDomains()
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("guard changed domains = %#v, %v", after, err)
	}
	deleted, failed, err := svc.DeleteDomains(ctx, []string{"missing.test", "B\u00dcCHER.DE.", "a.test", "A.TEST.", "invalid"}, actor)
	if err != nil || !reflect.DeepEqual(deleted, []string{"xn--bcher-kva.de", "a.test"}) || len(failed) != 2 {
		t.Fatalf("batch delete = %#v, %#v, %v", deleted, failed, err)
	}
	deleted, failed, err = svc.DeleteDomains(ctx, []string{"missing.test"}, actor)
	if err != nil || deleted == nil || len(deleted) != 0 || len(failed) != 1 {
		t.Fatalf("unchanged delete = %#v, %#v, %v", deleted, failed, err)
	}
	assertDomainAudits(t, db, actor, []domainAuditExpectation{
		{"BATCH_ADD_DOMAINS", map[string]any{"domains": []any{"a.test", "xn--bcher-kva.de"}, "skipped": float64(3)}},
		{"BATCH_DELETE_DOMAINS", map[string]any{"domains": []any{"xn--bcher-kva.de", "a.test"}, "failed": float64(2)}},
	})
}

func TestDomainServiceBatchAuditFailureLeavesCommittedChanges(t *testing.T) {
	for _, operation := range []string{"add", "delete"} {
		t.Run(operation, func(t *testing.T) {
			svc, db := newDomainTestService(t)
			if err := db.SeedDomainsIfEmpty([]string{"keeper.test", "first.test", "second.test"}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.DB().Exec(`CREATE TRIGGER reject_batch_domain_audit BEFORE INSERT ON audit_logs
				BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			actor := admin.Actor{User: "domain-admin", IP: "192.0.2.14"}
			var err error
			want := []string{"keeper.test"}
			if operation == "add" {
				_, _, err = svc.AddDomains(ctx, []string{"third.test", "fourth.test"}, actor)
				want = []string{"keeper.test", "first.test", "second.test", "third.test", "fourth.test"}
			} else {
				_, _, err = svc.DeleteDomains(ctx, []string{"first.test", "second.test"}, actor)
			}
			if err == nil {
				t.Fatal("audit failure was not returned")
			}
			domains, err := svc.ListDomains()
			if err != nil {
				t.Fatal(err)
			}
			got := []string{}
			for _, domain := range domains {
				got = append(got, domain.Domain)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("committed domains = %v, want %v", got, want)
			}
			assertDomainAudits(t, db, actor, nil)
		})
	}
}

func TestDomainServiceBatchStoreErrorDoesNotAudit(t *testing.T) {
	for _, operation := range []string{"add", "delete"} {
		t.Run(operation, func(t *testing.T) {
			svc, db := newDomainTestService(t)
			if err := db.SeedDomainsIfEmpty([]string{"keeper.test", "first.test", "reject.test"}); err != nil {
				t.Fatal(err)
			}
			before, err := svc.ListDomains()
			if err != nil {
				t.Fatal(err)
			}
			trigger := `CREATE TRIGGER reject_domain_batch BEFORE DELETE ON domains
				WHEN OLD.domain = 'reject.test' BEGIN SELECT RAISE(ABORT, 'delete unavailable'); END`
			if operation == "add" {
				trigger = `CREATE TRIGGER reject_domain_batch BEFORE INSERT ON domains
					WHEN NEW.domain = 'new-reject.test' BEGIN SELECT RAISE(ABORT, 'insert unavailable'); END`
			}
			if _, err := db.DB().Exec(trigger); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			actor := admin.Actor{User: "domain-admin", IP: "192.0.2.15"}
			if operation == "add" {
				_, _, err = svc.AddDomains(ctx, []string{"new-first.test", "new-reject.test"}, actor)
			} else {
				_, _, err = svc.DeleteDomains(ctx, []string{"first.test", "reject.test"}, actor)
			}
			if err == nil {
				t.Fatal("database failure was not returned")
			}
			after, err := svc.ListDomains()
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("failed batch changed domains = %#v, %v", after, err)
			}
			assertDomainAudits(t, db, actor, nil)
		})
	}
}
