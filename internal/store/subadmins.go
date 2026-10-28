package store

import (
	"database/sql"
	"strings"

	"github.com/google/uuid"
)

func (s *Store) CreateSubAdmin(token string, domains []string, options CreateSubAdminOptions) (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	days := 30
	if options.MaxRetentionDays != nil {
		days = *options.MaxRetentionDays
	}
	var whitelist any
	if len(options.SenderWhitelist) > 0 {
		whitelist = strings.Join(options.SenderWhitelist, ",")
	}
	now := s.now().Unix()
	_, err = s.db.Exec(`INSERT INTO sub_admins (id, token, domains, sender_whitelist, max_retention_days, created_at, updated_at, created_by, notes)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, id.String(), token, strings.Join(domains, ","), whitelist, days, now, now, options.CreatedBy, options.Notes)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

const subAdminSelect = `SELECT id, token, domains, sender_whitelist, max_retention_days, created_at, updated_at, is_active, created_by, notes FROM sub_admins`

func scanSubAdmin(row scanner) (*SubAdmin, error) {
	var a SubAdmin
	var domains string
	var whitelist sql.NullString
	if err := row.Scan(&a.ID, &a.Token, &domains, &whitelist, &a.MaxRetentionDays, &a.CreatedAt, &a.UpdatedAt, &a.IsActive, &a.CreatedBy, &a.Notes); err != nil {
		return nil, notFound(err)
	}
	a.Domains, a.SenderWhitelist = []string{}, []string{}
	if domains != "" {
		a.Domains = strings.Split(domains, ",")
	}
	if whitelist.String != "" {
		a.SenderWhitelist = strings.Split(whitelist.String, ",")
	}
	return &a, nil
}
func (s *Store) GetSubAdminByToken(token string) (*SubAdmin, error) {
	return scanSubAdmin(s.db.QueryRow(subAdminSelect+" WHERE token = ? AND is_active = 1", token))
}
func (s *Store) GetAllSubAdmins() ([]SubAdmin, error) {
	rows, err := s.db.Query(subAdminSelect + " ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	admins := []SubAdmin{}
	for rows.Next() {
		a, err := scanSubAdmin(rows)
		if err != nil {
			return nil, err
		}
		admins = append(admins, *a)
	}
	return admins, rows.Err()
}
func (s *Store) UpdateSubAdmin(id string, update SubAdminUpdate) error {
	fields := []string{}
	args := []any{}
	if update.Domains != nil {
		fields = append(fields, "domains = ?")
		args = append(args, strings.Join(update.Domains, ","))
	}
	if update.SenderWhitelist != nil {
		fields = append(fields, "sender_whitelist = ?")
		var value any
		if len(update.SenderWhitelist) > 0 {
			value = strings.Join(update.SenderWhitelist, ",")
		}
		args = append(args, value)
	}
	if update.MaxRetentionDays != nil {
		fields = append(fields, "max_retention_days = ?")
		args = append(args, *update.MaxRetentionDays)
	}
	if update.IsActive != nil {
		fields = append(fields, "is_active = ?")
		args = append(args, *update.IsActive)
	}
	if update.Notes != nil {
		fields = append(fields, "notes = ?")
		args = append(args, *update.Notes)
	}
	fields = append(fields, "updated_at = ?")
	args = append(args, s.now().Unix(), id)
	_, err := s.db.Exec("UPDATE sub_admins SET "+strings.Join(fields, ", ")+" WHERE id = ?", args...)
	return err
}
func (s *Store) DeleteSubAdmin(id string) error {
	_, err := s.db.Exec("DELETE FROM sub_admins WHERE id = ?", id)
	return err
}
