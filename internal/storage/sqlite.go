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
CREATE TABLE IF NOT EXISTS digests (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	meeting_id TEXT NOT NULL UNIQUE,
	title      TEXT NOT NULL,
	start_at   TEXT NOT NULL,
	share_link TEXT NOT NULL,
	summary    TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS digest_messages (
	digest_id  INTEGER NOT NULL,
	part       INTEGER NOT NULL,
	message_id INTEGER NOT NULL,
	PRIMARY KEY (digest_id, part)
);
CREATE TABLE IF NOT EXISTS task_done (
	digest_id INTEGER NOT NULL,
	task      INTEGER NOT NULL,
	PRIMARY KEY (digest_id, task)
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

// Digest is a delivered notification kept so its task checkboxes can be
// re-rendered when a button is pressed.
type Digest struct {
	ID        int64
	MeetingID string
	Title     string
	Start     time.Time
	ShareLink string
	Summary   string
}

// SaveDigest stores a digest once per meeting and returns its id.
func (s *SQLite) SaveDigest(ctx context.Context, d Digest, now time.Time) (int64, error) {
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO digests (meeting_id, title, start_at, share_link, summary, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		d.MeetingID, d.Title, d.Start.UTC().Format(time.RFC3339Nano), d.ShareLink, d.Summary,
		now.UTC().Format(time.RFC3339Nano)); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM digests WHERE meeting_id = ?`, d.MeetingID).Scan(&id)
	return id, err
}

// DigestByID returns false when it does not exist (never saved, or pruned).
func (s *SQLite) DigestByID(ctx context.Context, id int64) (Digest, bool, error) {
	d := Digest{ID: id}
	var start string
	err := s.db.QueryRowContext(ctx,
		`SELECT meeting_id, title, start_at, share_link, summary FROM digests WHERE id = ?`, id).
		Scan(&d.MeetingID, &d.Title, &start, &d.ShareLink, &d.Summary)
	if errors.Is(err, sql.ErrNoRows) {
		return Digest{}, false, nil
	}
	if err != nil {
		return Digest{}, false, err
	}
	d.Start, _ = time.Parse(time.RFC3339Nano, start)
	return d, true, nil
}

func (s *SQLite) SaveDigestMessage(ctx context.Context, digestID int64, part int, messageID int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO digest_messages (digest_id, part, message_id) VALUES (?, ?, ?)
		 ON CONFLICT(digest_id, part) DO UPDATE SET message_id = excluded.message_id`,
		digestID, part, messageID)
	return err
}

// DigestMessages maps message part (0-based) to Telegram message id.
func (s *SQLite) DigestMessages(ctx context.Context, digestID int64) (map[int]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT part, message_id FROM digest_messages WHERE digest_id = ?`, digestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]int64{}
	for rows.Next() {
		var part int
		var id int64
		if err := rows.Scan(&part, &id); err != nil {
			return nil, err
		}
		out[part] = id
	}
	return out, rows.Err()
}

// DoneTasks returns the set of completed task numbers.
func (s *SQLite) DoneTasks(ctx context.Context, digestID int64) (map[int]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task FROM task_done WHERE digest_id = ?`, digestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// ToggleTask flips a task and reports its new state (true = done).
func (s *SQLite) ToggleTask(ctx context.Context, digestID int64, task int) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM task_done WHERE digest_id = ? AND task = ?`, digestID, task)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	done := n == 0
	if done {
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_done (digest_id, task) VALUES (?, ?)`, digestID, task); err != nil {
			return false, err
		}
	}
	return done, tx.Commit()
}

// PruneDigests deletes digests (and their messages and marks) created before cutoff.
func (s *SQLite) PruneDigests(ctx context.Context, cutoff time.Time) error {
	c := cutoff.UTC().Format(time.RFC3339Nano)
	for _, q := range []string{
		`DELETE FROM digest_messages WHERE digest_id IN (SELECT id FROM digests WHERE created_at < ?)`,
		`DELETE FROM task_done WHERE digest_id IN (SELECT id FROM digests WHERE created_at < ?)`,
		`DELETE FROM digests WHERE created_at < ?`,
	} {
		if _, err := s.db.ExecContext(ctx, q, c); err != nil {
			return err
		}
	}
	return nil
}
