// Package store owns afto's on-disk state: a single SQLite database of
// executed commands and the aggregate statistics providers rank against.
//
// Two tables carry the data (plans/phase-1.md §6):
//
//   - events: an append-only ledger of every executed command with its cwd,
//     session, exit code and timestamp. It is what let Phase 3's transition
//     table be backfilled from data collected since day one.
//
//   - stats: the aggregate the providers actually query — one row per
//     (cmd, cwd) pair plus a "rollup" row per cmd with cwd=” that
//     accumulates across all directories. The rollup means "how often has
//     this command run anywhere" is a primary-key lookup instead of a SUM,
//     at the cost of one extra upsert per ingest. Storage is command lines,
//     so the dataset is small (tens of MB for years of history).
//
//   - transitions (schema v2): how often command B followed command A in the
//     same shell session, feeding the next-command prediction shown on an
//     empty prompt. Maintained on ingest and backfilled from events.
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
CREATE TABLE IF NOT EXISTS transitions (
  prev    TEXT NOT NULL,
  next    TEXT NOT NULL,
  count   INTEGER NOT NULL,
  last_ts INTEGER NOT NULL,
  PRIMARY KEY (prev, next)
);
CREATE INDEX IF NOT EXISTS events_session ON events(session, id);
`

// schemaVersion is the version this code writes. Bump it and add an upgrade
// step below when the shape changes.
const schemaVersion = 2

// migrate creates missing objects and upgrades an older database in place.
// Databases are long-lived (they hold years of history), so an upgrade must
// never require the user to delete anything: v1 stores get the v2 tables
// created empty and then backfilled from the events ledger they already have.
func migrate(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}

	var have string
	err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&have)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: migrate: %w", err)
	}
	// No version row at all means a database this code just created: the
	// schema above is already current and there is nothing to migrate.
	if errors.Is(err, sql.ErrNoRows) {
		return setSchemaVersion(db, schemaVersion)
	}
	if have == "1" {
		if err := backfillTransitions(db); err != nil {
			return err
		}
	}
	return setSchemaVersion(db, schemaVersion)
}

func setSchemaVersion(db *sql.DB, v int) error {
	if _, err := db.Exec(
		`INSERT INTO meta(key,value) VALUES('schema_version',?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, v); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// backfillTransitions reconstructs the transition table from the events
// ledger: within each session, every command pairs with the one before it.
//
// Imported history is deliberately excluded (session 'import'). A HISTFILE
// interleaves every terminal that was open, so consecutive lines in it are
// not causally related — pairing them would teach the predictor noise. The
// practical consequence, worth knowing: prediction becomes useful after a
// day of real use, not at install time.
func backfillTransitions(db *sql.DB) error {
	rows, err := db.Query(
		`SELECT session, cmd, ts FROM events
		 WHERE session <> '' AND session <> 'import'
		 ORDER BY session, id`)
	if err != nil {
		return fmt.Errorf("store: backfill: %w", err)
	}
	defer rows.Close()

	type pair struct{ prev, next string }
	agg := map[pair]*struct {
		count  int64
		lastTS int64
	}{}
	var curSession, prevCmd string
	for rows.Next() {
		var session, cmd string
		var ts int64
		if err := rows.Scan(&session, &cmd, &ts); err != nil {
			return fmt.Errorf("store: backfill: %w", err)
		}
		if session != curSession { // session boundary: no pair spans shells
			curSession, prevCmd = session, cmd
			continue
		}
		if prevCmd != "" {
			k := pair{prevCmd, cmd}
			a := agg[k]
			if a == nil {
				a = &struct {
					count  int64
					lastTS int64
				}{}
				agg[k] = a
			}
			a.count++
			if ts > a.lastTS {
				a.lastTS = ts
			}
		}
		prevCmd = cmd
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: backfill: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: backfill: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for k, a := range agg {
		if err := upsertTransition(context.Background(), tx, k.prev, k.next, a.lastTS, a.count); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: backfill: %w", err)
	}
	return nil
}

// importSession marks events that came from a HISTFILE import rather than
// from a live shell. Transition learning excludes them: a history file
// interleaves every terminal that was open, so "the next line" is not "what
// the user did next" (see backfillTransitions).
const importSession = "import"

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

	// The session's previous command, read BEFORE this one is appended, is
	// the "prev" of a transition pair. Redacted commands never reach here,
	// so a secret-shaped command doesn't just stay unstored — it also can't
	// appear on either side of a pair; the chain simply closes over it.
	prev, err := lastCommandTx(ctx, tx, e.Session)
	if err != nil {
		return false, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(cmd,cwd,session,exit_code,ts) VALUES(?,?,?,?,?)`,
		e.Cmd, e.CWD, e.Session, e.Exit, e.TS); err != nil {
		return false, fmt.Errorf("store: ingest: %w", err)
	}
	if prev != "" {
		if err := upsertTransition(ctx, tx, prev, e.Cmd, e.TS, 1); err != nil {
			return false, err
		}
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

func upsertTransition(ctx context.Context, tx *sql.Tx, prev, next string, ts int64, n int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO transitions(prev,next,count,last_ts) VALUES(?,?,?,?)
		 ON CONFLICT(prev,next) DO UPDATE SET
		   count = count + excluded.count,
		   last_ts = MAX(last_ts, excluded.last_ts)`,
		prev, next, n, ts)
	if err != nil {
		return fmt.Errorf("store: upsert transition: %w", err)
	}
	return nil
}

// lastCommandTx returns the most recent command recorded for a session, or
// "" when the session is unknown, empty, or the importer's. The
// events(session, id) index makes this a one-row index seek, which is why
// it is affordable inside every ingest.
func lastCommandTx(ctx context.Context, tx *sql.Tx, session string) (string, error) {
	if session == "" || session == importSession {
		return "", nil
	}
	var cmd string
	err := tx.QueryRowContext(ctx,
		`SELECT cmd FROM events WHERE session = ? ORDER BY id DESC LIMIT 1`, session).Scan(&cmd)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: last command: %w", err)
	}
	return cmd, nil
}

// LastCommand reports the session's most recent command. The transition
// provider prefers the Recent context the shell sends, and falls back to
// this for clients that send none (aftod query, tests).
func (s *Store) LastCommand(ctx context.Context, session string) (string, error) {
	if session == "" || session == importSession {
		return "", nil
	}
	var cmd string
	err := s.db.QueryRowContext(ctx,
		`SELECT cmd FROM events WHERE session = ? ORDER BY id DESC LIMIT 1`, session).Scan(&cmd)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: last command: %w", err)
	}
	return cmd, nil
}

// TransitionRow is one "what followed prev" aggregate.
type TransitionRow struct {
	Next   string
	Count  int64
	LastTS int64
}

// TopNext returns the commands most often run after prev, most frequent
// first. A primary-key range seek on (prev, next).
func (s *Store) TopNext(ctx context.Context, prev string, limit int) ([]TransitionRow, error) {
	if prev == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT next, count, last_ts FROM transitions WHERE prev = ?
		 ORDER BY count DESC, last_ts DESC LIMIT ?`, prev, limit)
	if err != nil {
		return nil, fmt.Errorf("store: transitions: %w", err)
	}
	defer rows.Close()
	var out []TransitionRow
	for rows.Next() {
		var r TransitionRow
		if err := rows.Scan(&r.Next, &r.Count, &r.LastTS); err != nil {
			return nil, fmt.Errorf("store: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
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
// the rows the frecency score needs: the cwd=” rollup (global term), the
// exact cwd (directory-affinity term), and — when root is non-empty —
// everything inside that project root (project-affinity term). One scan
// serves all three terms; the provider folds the row kinds.
//
// The project predicate is written against path boundaries, not a plain
// string range: a naive `cwd >= root AND cwd < upperBound(root)` would sweep
// in sibling directories that merely share a name prefix (/x/proj-old for
// root /x/proj). Matching `cwd = root OR cwd BETWEEN root+"/" AND …` is
// exact because a child path always continues with the separator.
//
// The lookup is a B-tree range scan on the (cmd, cwd) primary key —
// deliberately not LIKE, which would not use the index reliably. limit
// bounds the scan for very short prefixes.
func (s *Store) PrefixStats(ctx context.Context, prefix, cwd, root string, limit int) ([]StatRow, error) {
	q := `SELECT cmd, cwd, count, last_ts FROM stats WHERE (cwd = '' OR cwd = ?`
	args := []any{cwd}
	if root != "" {
		q += ` OR cwd = ? OR (cwd >= ? AND cwd < ?)`
		args = append(args, root, root+"/", prefixUpperBound(root+"/"))
	}
	q += `)`
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
