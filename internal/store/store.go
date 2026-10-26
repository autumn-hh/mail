// Package store 提供兼容 Python 表结构的 SQLite 存储。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"maildrop/internal/config"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound          = errors.New("not found")
	ErrDomainExists      = errors.New("domain already exists")
	ErrLastEnabledDomain = errors.New("cannot remove the last enabled domain")
	ErrInvalidDomain     = errors.New("invalid domain")
)

type Store struct {
	db  *sql.DB
	cfg config.Config
	now func() time.Time
}

func Open(path string) (*Store, error) {
	return OpenWithConfig(path, config.FromLookupEnv(os.LookupEnv))
}

// OpenWithConfig 显式传入配置；Open 不会隐式读取 .env。
func OpenWithConfig(path string, cfg *config.Config) (*Store, error) {
	if cfg == nil {
		return nil, errors.New("nil config")
	}
	if path == "" {
		return nil, errors.New("empty database path")
	}
	dsn := path
	if path != ":memory:" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(absolute), 0755); err != nil {
			return nil, err
		}
		uriPath := filepath.ToSlash(absolute)
		if !strings.HasPrefix(uriPath, "/") {
			uriPath = "/" + uriPath
		}
		dsn = (&url.URL{Scheme: "file", Path: uriPath}).String()
	}
	// 单连接串行排队进程内写入；IMMEDIATE 先取写锁，避免读事务升级的 BUSY。
	// busy_timeout 同时等待其他进程/Store 的短事务，WAL 允许外部读者并行。
	dsn += "?_txlock=immediate&_pragma=busy_timeout%2810000%29&_pragma=foreign_keys%281%29"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, cfg: *cfg, now: time.Now}
	if _, err = db.Exec("PRAGMA journal_mode=WAL"); err == nil {
		err = s.migrate()
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("open store: %w", err)
	}
	return s, nil
}

func (s *Store) DB() *sql.DB  { return s.db }
func (s *Store) Close() error { return s.db.Close() }

// WithTx 回调只能使用 tx；回调内再次使用 DB 会等待当前唯一连接。
func (s *Store) WithTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func affected(result sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
