// Package store owns afto's on-disk state: a single SQLite database of
// executed commands and the aggregate statistics providers rank against.
//
// Two tables carry the data (plans/phase-1.md §6):
//
//   - events: an append-only ledger of every executed command with its cwd,
//     session, exit code and timestamp. Nothing reads it on the suggestion
//     hot path today; it exists so Phase 3 (command-transition prediction)
//     can be built from data collected since day one.
//
//   - stats: the aggregate the providers actually query — one row per
//     (cmd, cwd) pair plus a "rollup" row per cmd with cwd=” that
//     accumulates across all directories. The rollup means "how often has
//     this command run anywhere" is a primary-key lookup instead of a SUM,
//     at the cost of one extra upsert per ingest. Storage is command lines,
//     so the dataset is small (tens of MB for years of history).
//
// Privacy invariant: redaction happens HERE, before persistence, not at
// display time. A command matching a secret pattern is skipped entirely —
// never stored masked, because a masked command would resurface as a broken
// suggestion. See redact.go.
//
// Concurrency: the daemon is effectively the only writer; WAL mode plus a
// busy_timeout lets `aftod import` (a second process) coexist with a
// running daemon without either erroring out.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database. Create with Open; safe for concurrent
// use (database/sql serializes access; SQLite WAL handles the rest).
type Store struct {
	db       *sql.DB
	redactor *Redactor
}

// Open creates/opens dir/afto.db (dir is created 0700 — it holds command
// history, which is sensitive) and applies migrations. The default Redactor
// is installed; the daemon swaps in a config-extended one via SetRedactor.
func Open(dir string) (*Store, error) {
	var err error
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	dsn := "file:" + filepath.Join(dir, "afto.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(250)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err = migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	r, err := NewRedactor(nil)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, redactor: r}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// SetRedactor replaces the redaction rules (used for config hot reload).
func (s *Store) SetRedactor(r *Redactor) { s.redactor = r }

const schema = `
CREATE TABLE IF NOT EXISTS events (
  id        INTEGER PRIMARY KEY,
  cmd       TEXT NOT NULL,
  cwd       TEXT NOT NULL DEFAULT '',
  session   TEXT NOT NULL DEFAULT '',
  exit_code INTEGER NOT NULL DEFAULT 0,
  ts        INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS stats (
  cmd     TEXT NOT NULL,
  cwd     TEXT NOT NULL DEFAULT '',
  count   INTEGER NOT NULL,
  last_ts INTEGER NOT NULL,
  PRIMARY KEY (cmd, cwd)
);
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT
);
`

func migrate(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	if _, err := db.Exec(
		`INSERT INTO meta(key,value) VALUES('schema_version','1')
		 ON CONFLICT(key) DO NOTHING`); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// Event is one executed command as reported by a shell (or the importer).
type Event struct {
	Cmd     string
	CWD     string
	Session string
	Exit    int
	TS      int64
}

// Ingest records one executed command: an events row plus stats upserts for
// (cmd, cwd) and the cwd=” rollup. Returns false (and no error) when the
// command was skipped by redaction — a skip is a success, not a failure.
func (s *Store) Ingest(ctx context.Context, e Event) (bool, error) {
	if e.Cmd == "" || s.redactor.Skip(e.Cmd) {
		return false, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: ingest: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(cmd,cwd,session,exit_code,ts) VALUES(?,?,?,?,?)`,
		e.Cmd, e.CWD, e.Session, e.Exit, e.TS); err != nil {
		return false, fmt.Errorf("store: ingest: %w", err)
	}
	// Two distinct stats rows per execution: the (cmd,'') rollup feeds the
	// frecency score's global term ("how often anywhere"); the (cmd,cwd) row
	// feeds its cwd-affinity term ("how often in this directory"). When the
	// event has no cwd (importer), the per-directory key would collide with
	// the rollup key and double-count — hence the guard.
	if err := upsertStat(ctx, tx, e.Cmd, "", e.TS, 1); err != nil {
		return false, err
	}
	if e.CWD != "" {
		if err := upsertStat(ctx, tx, e.Cmd, e.CWD, e.TS, 1); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: ingest: %w", err)
	}
	return true, nil
}

func upsertStat(ctx context.Context, tx *sql.Tx, cmd, cwd string, ts int64, n int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO stats(cmd,cwd,count,last_ts) VALUES(?,?,?,?)
		 ON CONFLICT(cmd,cwd) DO UPDATE SET
		   count = count + excluded.count,
		   last_ts = MAX(last_ts, excluded.last_ts)`,
		cmd, cwd, n, ts)
	if err != nil {
		return fmt.Errorf("store: upsert stat: %w", err)
	}
	return nil
}

// StatRow is one stats record; CWD==” is the all-directories rollup.
type StatRow struct {
	Cmd    string
	CWD    string
	Count  int64
	LastTS int64
}

// PrefixStats returns stats rows whose cmd starts with prefix, restricted to
// the rollup rows plus rows for the given cwd (both are needed to compute
// the frecency score's global and cwd-affinity terms in one pass).
//
// The lookup is a B-tree range scan on the (cmd, cwd) primary key —
// deliberately not LIKE, which would not use the index reliably. limit
// bounds the scan for very short prefixes.
func (s *Store) PrefixStats(ctx context.Context, prefix, cwd string, limit int) ([]StatRow, error) {
	q := `SELECT cmd, cwd, count, last_ts FROM stats WHERE cwd IN ('', ?)`
	args := []any{cwd}
	if prefix != "" {
		q += ` AND cmd >= ?`
		args = append(args, prefix)
		if ub := prefixUpperBound(prefix); ub != "" {
			q += ` AND cmd < ?`
			args = append(args, ub)
		}
	}
	q += ` LIMIT ?`
	args = append(args, limit)
	return s.statRows(ctx, q, args...)
}

// MostRecentPrefix returns rollup rows matching prefix ordered by recency,
// newest first. Backs the history provider (pure recency, PoC parity).
func (s *Store) MostRecentPrefix(ctx context.Context, prefix string, limit int) ([]StatRow, error) {
	q := `SELECT cmd, cwd, count, last_ts FROM stats WHERE cwd = ''`
	var args []any
	if prefix != "" {
		q += ` AND cmd >= ?`
		args = append(args, prefix)
		if ub := prefixUpperBound(prefix); ub != "" {
			q += ` AND cmd < ?`
			args = append(args, ub)
		}
	}
	q += ` ORDER BY last_ts DESC LIMIT ?`
	args = append(args, limit)
	return s.statRows(ctx, q, args...)
}

func (s *Store) statRows(ctx context.Context, q string, args ...any) ([]StatRow, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query: %w", err)
	}
	defer rows.Close()
	var out []StatRow
	for rows.Next() {
		var r StatRow
		if err := rows.Scan(&r.Cmd, &r.CWD, &r.Count, &r.LastTS); err != nil {
			return nil, fmt.Errorf("store: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// prefixUpperBound returns the smallest string greater than every string
// with the given prefix, for use as an exclusive range end. Increments the
// last byte, dropping trailing 0xFF bytes first; returns "" (no bound) for
// the degenerate all-0xFF prefix.
func prefixUpperBound(p string) string {
	b := []byte(p)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xFF {
			b[i]++
			return string(b[:i+1])
		}
	}
	return ""
}

// MetaGet returns the value for key, or "" if absent.
func (s *Store) MetaGet(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: meta get: %w", err)
	}
	return v, nil
}

// MetaSet stores key=value.
func (s *Store) MetaSet(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta(key,value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("store: meta set: %w", err)
	}
	return nil
}
