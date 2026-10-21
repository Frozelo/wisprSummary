// Package storage persists processed meetings and small service state in SQLite.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no CGO
)

const schema = `
CREATE TABLE IF NOT EXISTS processed_meetings (
	id           TEXT PRIMARY KEY,
	title        TEXT NOT NULL,
	processed_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS claims (
	id         TEXT PRIMARY KEY,
	claimed_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS kv (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);`

const watermarkKey = "watermark"

type SQLite struct{ db *sql.DB }

func Open(path string) (*SQLite, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single writer; the service is sequential
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLite{db: db}, nil
}

func (s *SQLite) Close() error { return s.db.Close() }

func (s *SQLite) IsProcessed(ctx context.Context, id string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM processed_meetings WHERE id = ?`, id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *SQLite) MarkProcessed(ctx context.Context, id, title string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO processed_meetings (id, title, processed_at) VALUES (?, ?, ?)`,
		id, title, at.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return s.Release(ctx, id)
}

// Claim atomically reserves a meeting for sending so two processes sharing the
// database cannot both deliver it. A claim older than ttl is considered
// abandoned (crashed process) and can be taken over.
func (s *SQLite) Claim(ctx context.Context, id string, now time.Time, ttl time.Duration) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO claims (id, claimed_at) VALUES (?, ?)
		 ON CONFLICT(id) DO UPDATE SET claimed_at = excluded.claimed_at WHERE claims.claimed_at <= ?`,
		id, now.UnixNano(), now.Add(-ttl).UnixNano())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *SQLite) Release(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM claims WHERE id = ?`, id)
	return err
}

// Watermark returns the lower bound (by modified_at) for the next scan.
func (s *SQLite) Watermark(ctx context.Context) (time.Time, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, watermarkKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	return t, err == nil, err
}

func (s *SQLite) SetWatermark(ctx context.Context, t time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		watermarkKey, t.UTC().Format(time.RFC3339Nano))
	return err
}

// GetKV reads a small persisted value.
func (s *SQLite) GetKV(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *SQLite) SetKV(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *SQLite) DeleteKV(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, key)
	return err
}
