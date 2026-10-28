package store

import (
	"errors"
	"testing"
	"time"
)

func TestSubAdminCRUD(t *testing.T) {
	s := testStore(t)
	now := time.Unix(1800000000, 0)
	s.now = func() time.Time { return now }
	id, err := s.CreateSubAdmin("token", []string{"a.test", "b.test"}, CreateSubAdminOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.GetSubAdminByToken("token")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != id || a.IsActive != 1 || a.MaxRetentionDays != 30 || len(a.Domains) != 2 || a.SenderWhitelist == nil || len(a.SenderWhitelist) != 0 {
		t.Fatalf("admin: %+v", a)
	}
	value := jsonObject(t, a)
	if len(value) != 10 || value["is_active"] != float64(1) || value["created_by"] != nil || value["notes"] != nil {
		t.Fatal(value)
	}
	if _, err := s.CreateSubAdmin("token", nil, CreateSubAdminOptions{}); err == nil {
		t.Fatal("duplicate token")
	}
	now = now.Add(time.Second)
	days := 99
	notes := "updated"
	if err := s.UpdateSubAdmin(id, SubAdminUpdate{Domains: []string{"c.test"}, SenderWhitelist: []string{"@ok.test"}, MaxRetentionDays: &days, Notes: &notes}); err != nil {
		t.Fatal(err)
	}
	a, _ = s.GetSubAdminByToken("token")
	if a.MaxRetentionDays != 99 || len(a.Domains) != 1 || a.Domains[0] != "c.test" || len(a.SenderWhitelist) != 1 || a.UpdatedAt != now.Unix() {
		t.Fatal(a)
	}
	if err := s.UpdateSubAdmin(id, SubAdminUpdate{SenderWhitelist: []string{}}); err != nil {
		t.Fatal(err)
	}
	a, _ = s.GetSubAdminByToken("token")
	if len(a.SenderWhitelist) != 0 {
		t.Fatal("clear whitelist")
	}
	inactive := 0
	if err := s.UpdateSubAdmin(id, SubAdminUpdate{IsActive: &inactive}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSubAdminByToken("token"); !errors.Is(err, ErrNotFound) {
		t.Fatal("inactive visible", err)
	}
	if _, err := s.CreateSubAdmin("later", nil, CreateSubAdminOptions{}); err != nil {
		t.Fatal(err)
	}
	all, err := s.GetAllSubAdmins()
	if err != nil || len(all) != 2 || all[0].Token != "later" {
		t.Fatal(all, err)
	}
	if err := s.DeleteSubAdmin(id); err != nil {
		t.Fatal(err)
	}
	all, err = s.GetAllSubAdmins()
	if err != nil || len(all) != 1 {
		t.Fatal(all, err)
	}
}
