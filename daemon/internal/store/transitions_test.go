package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestIngestRecordsTransitions(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	mustIngest(t, s, Event{Cmd: "git add -p", Session: "s1", TS: 100})
	mustIngest(t, s, Event{Cmd: "git commit", Session: "s1", TS: 200})
	mustIngest(t, s, Event{Cmd: "git add -p", Session: "s1", TS: 300})
	mustIngest(t, s, Event{Cmd: "git commit", Session: "s1", TS: 400})
	// A different shell must not chain onto s1's last command.
	mustIngest(t, s, Event{Cmd: "make test", Session: "s2", TS: 500})

	rows, err := s.TopNext(ctx, "git add -p", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Next != "git commit" || rows[0].Count != 2 || rows[0].LastTS != 400 {
		t.Fatalf("unexpected transitions: %+v", rows)
	}
	// "git commit" → "git add -p" happened once; the cross-session pair
	// "git commit" → "make test" must not exist.
	rows, err = s.TopNext(ctx, "git commit", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Next != "git add -p" {
		t.Fatalf("cross-session pair leaked: %+v", rows)
	}
}

func TestTopNextOrdersByCount(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		mustIngest(t, s, Event{Cmd: "make build", Session: "s", TS: int64(10 * (2*i + 1))})
		mustIngest(t, s, Event{Cmd: "make test", Session: "s", TS: int64(10 * (2*i + 2))})
	}
	mustIngest(t, s, Event{Cmd: "make build", Session: "s", TS: 100})
	mustIngest(t, s, Event{Cmd: "make run", Session: "s", TS: 110})

	rows, err := s.TopNext(ctx, "make build", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Next != "make test" || rows[1].Next != "make run" {
		t.Fatalf("want test(3) before run(1), got %+v", rows)
	}
}

func TestRedactedCommandBreaksTheChainWithoutPoisoningIt(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	mustIngest(t, s, Event{Cmd: "vault login", Session: "s", TS: 100})
	// Skipped by redaction: must appear on neither side of a pair.
	if ok, err := s.Ingest(ctx, Event{Cmd: "export API_TOKEN=abc", Session: "s", TS: 200}); err != nil || ok {
		t.Fatalf("expected redaction skip, ok=%v err=%v", ok, err)
	}
	mustIngest(t, s, Event{Cmd: "make deploy", Session: "s", TS: 300})

	rows, err := s.TopNext(ctx, "vault login", 10)
	if err != nil {
		t.Fatal(err)
	}
	// The chain closes over the secret: login → deploy, and no row anywhere
	// mentions the redacted command.
	if len(rows) != 1 || rows[0].Next != "make deploy" {
		t.Fatalf("unexpected transitions: %+v", rows)
	}
	if rows, _ := s.TopNext(ctx, "export API_TOKEN=abc", 10); len(rows) != 0 {
		t.Fatalf("redacted command used as prev: %+v", rows)
	}
}

func TestLastCommand(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	mustIngest(t, s, Event{Cmd: "first", Session: "s", TS: 1})
	mustIngest(t, s, Event{Cmd: "second", Session: "s", TS: 2})

	if got, _ := s.LastCommand(ctx, "s"); got != "second" {
		t.Fatalf("got %q, want %q", got, "second")
	}
	if got, _ := s.LastCommand(ctx, "unknown"); got != "" {
		t.Fatalf("unknown session should be empty, got %q", got)
	}
	if got, _ := s.LastCommand(ctx, importSession); got != "" {
		t.Fatalf("import session must never be a transition source, got %q", got)
	}
}

// v1SchemaSQL is the Phase 1/2 schema verbatim: the migration must upgrade a
// database of this shape in place, since real ones hold years of history.
const v1SchemaSQL = `
CREATE TABLE events (
  id INTEGER PRIMARY KEY, cmd TEXT NOT NULL, cwd TEXT NOT NULL DEFAULT '',
  session TEXT NOT NULL DEFAULT '', exit_code INTEGER NOT NULL DEFAULT 0,
  ts INTEGER NOT NULL);
CREATE TABLE stats (
  cmd TEXT NOT NULL, cwd TEXT NOT NULL DEFAULT '', count INTEGER NOT NULL,
  last_ts INTEGER NOT NULL, PRIMARY KEY (cmd, cwd));
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);
INSERT INTO meta(key,value) VALUES('schema_version','1');
`

func TestMigrateV1BackfillsTransitions(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "afto.db")

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(v1SchemaSQL); err != nil {
		t.Fatal(err)
	}
	// Two real sessions plus imported history. Only the real ones may
	// produce pairs, and pairs must not span the session boundary.
	seed := []struct {
		cmd, session string
		ts           int64
	}{
		{"git add -p", "s1", 10},
		{"git commit", "s1", 20},
		{"git add -p", "s1", 30},
		{"git commit", "s1", 40},
		{"make test", "s2", 50}, // new session: no pair with s1's tail
		{"make build", "s2", 60},
		{"ls", importSession, 70}, // imported: never a transition source
		{"cd /tmp", importSession, 80},
	}
	for _, e := range seed {
		if _, err := db.Exec(
			`INSERT INTO events(cmd,cwd,session,exit_code,ts) VALUES(?,'',?,0,?)`,
			e.cmd, e.session, e.ts); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir) // migrate runs here
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	if v, _ := s.MetaGet(ctx, "schema_version"); v != "2" {
		t.Fatalf("schema_version = %q, want 2", v)
	}
	rows, err := s.TopNext(ctx, "git add -p", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Next != "git commit" || rows[0].Count != 2 {
		t.Fatalf("backfill missed the repeated pair: %+v", rows)
	}
	if rows, _ := s.TopNext(ctx, "git commit", 10); len(rows) != 1 || rows[0].Next != "git add -p" {
		t.Fatalf("unexpected pairs for 'git commit': %+v", rows)
	}
	if rows, _ := s.TopNext(ctx, "ls", 10); len(rows) != 0 {
		t.Fatalf("imported history produced pairs: %+v", rows)
	}
	if rows, _ := s.TopNext(ctx, "make build", 10); len(rows) != 0 {
		t.Fatalf("pair spans the session boundary: %+v", rows)
	}

	// Reopening is idempotent: no double-counting on a second migrate.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if rows, _ := s2.TopNext(ctx, "git add -p", 10); len(rows) != 1 || rows[0].Count != 2 {
		t.Fatalf("re-migration double-counted: %+v", rows)
	}
}

func TestPrefixStatsProjectScope(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	mustIngest(t, s, Event{Cmd: "make test", CWD: "/w/proj/api", TS: 100})
	mustIngest(t, s, Event{Cmd: "make test", CWD: "/w/proj/web", TS: 200})
	// A sibling that merely shares a name prefix must NOT count as inside
	// the project (the classic string-range bug this query avoids).
	mustIngest(t, s, Event{Cmd: "make test", CWD: "/w/proj-old", TS: 300})
	mustIngest(t, s, Event{Cmd: "make test", CWD: "/elsewhere", TS: 400})

	rows, err := s.PrefixStats(ctx, "make", "/w/proj/api", "/w/proj", 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.CWD] = true
	}
	for _, want := range []string{"", "/w/proj/api", "/w/proj/web"} {
		if !got[want] {
			t.Fatalf("missing cwd %q in %+v", want, rows)
		}
	}
	if got["/w/proj-old"] {
		t.Fatal("sibling directory /w/proj-old leaked into the project scope")
	}
	if got["/elsewhere"] {
		t.Fatal("unrelated directory leaked into the project scope")
	}

	// Without a root, behavior is exactly Phase 1's: rollup + exact cwd.
	rows, err = s.PrefixStats(ctx, "make", "/w/proj/api", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.CWD != "" && r.CWD != "/w/proj/api" {
			t.Fatalf("root=\"\" must not widen the scan, got %+v", r)
		}
	}
}
