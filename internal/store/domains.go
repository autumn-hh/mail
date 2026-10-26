package store

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strings"

	"golang.org/x/net/idna"
	"modernc.org/sqlite"
)

func NormalizeDomain(value string) (string, error) {
	value = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
	ascii, err := idna.Lookup.ToASCII(value)
	if err != nil {
		return "", ErrInvalidDomain
	}
	ascii = strings.ToLower(ascii)
	if ascii == "" || len(ascii) > 253 || net.ParseIP(ascii) != nil {
		return "", ErrInvalidDomain
	}
	labels := strings.Split(ascii, ".")
	if len(labels) == 1 && ascii != "localhost" {
		return "", ErrInvalidDomain
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrInvalidDomain
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return "", ErrInvalidDomain
			}
		}
	}
	return ascii, nil
}

func (s *Store) listDomains(enabledOnly bool) ([]Domain, error) {
	query := "SELECT domain, enabled, created_at, updated_at FROM domains"
	if enabledOnly {
		query += " WHERE enabled = 1"
	}
	// 同一秒写入（如首次从 DOMAINS 播种）按插入顺序排列，保证 DOMAINS 第一个域名仍是默认域名。
	query += " ORDER BY created_at, rowid"
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	domains := []Domain{}
	for rows.Next() {
		var d Domain
		if err := rows.Scan(&d.Domain, &d.Enabled, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		domains = append(domains, d)
	}
	return domains, rows.Err()
}
func (s *Store) ListDomains() ([]Domain, error)        { return s.listDomains(false) }
func (s *Store) ListEnabledDomains() ([]Domain, error) { return s.listDomains(true) }
func (s *Store) AddDomain(value string) (*Domain, error) {
	domain, err := NormalizeDomain(value)
	if err != nil {
		return nil, err
	}
	now := s.now().Unix()
	_, err = s.db.Exec("INSERT INTO domains (domain, created_at, updated_at) VALUES (?, ?, ?)", domain, now, now)
	if err != nil {
		var sqliteErr *sqlite.Error
		if errors.As(err, &sqliteErr) && (sqliteErr.Code() == 1555 || sqliteErr.Code() == 2067) {
			return nil, ErrDomainExists
		}
		return nil, err
	}
	return &Domain{Domain: domain, Enabled: true, CreatedAt: now, UpdatedAt: now}, nil
}
func (s *Store) changeDomain(value string, enabled bool, remove bool) error {
	domain, err := NormalizeDomain(value)
	if err != nil {
		return err
	}
	return s.WithTx(context.Background(), func(tx *sql.Tx) error {
		var current bool
		if err := tx.QueryRow("SELECT enabled FROM domains WHERE domain = ?", domain).Scan(&current); err != nil {
			return notFound(err)
		}
		if current && (remove || !enabled) {
			var count int
			if err := tx.QueryRow("SELECT COUNT(*) FROM domains WHERE enabled = 1").Scan(&count); err != nil {
				return err
			}
			if count <= 1 {
				return ErrLastEnabledDomain
			}
		}
		if remove {
			_, err := tx.Exec("DELETE FROM domains WHERE domain = ?", domain)
			return err
		}
		_, err := tx.Exec("UPDATE domains SET enabled = ?, updated_at = ? WHERE domain = ?", enabled, s.now().Unix(), domain)
		return err
	})
}
func (s *Store) SetDomainEnabled(domain string, enabled bool) error {
	return s.changeDomain(domain, enabled, false)
}
func (s *Store) DeleteDomain(domain string) error { return s.changeDomain(domain, false, true) }

// SeedDomainsIfEmpty 仅空表时初始化；禁用的已有域名也阻止重播种。
func (s *Store) SeedDomainsIfEmpty(values []string) error {
	return s.WithTx(context.Background(), func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRow("SELECT COUNT(*) FROM domains").Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return nil
		}
		if len(values) == 0 {
			return ErrInvalidDomain
		}
		now := s.now().Unix()
		for _, value := range values {
			domain, err := NormalizeDomain(value)
			if err != nil {
				return err
			}
			if _, err := tx.Exec("INSERT OR IGNORE INTO domains (domain, created_at, updated_at) VALUES (?, ?, ?)", domain, now, now); err != nil {
				return err
			}
		}
		return nil
	})
}
