package store

import (
	"context"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustIngest(t *testing.T, s *Store, e Event) {
	t.Helper()
	ok, err := s.Ingest(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("ingest of %q unexpectedly skipped", e.Cmd)
	}
}

func TestIngestMaintainsStatsAndRollup(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	mustIngest(t, s, Event{Cmd: "git status", CWD: "/a", TS: 100})
	mustIngest(t, s, Event{Cmd: "git status", CWD: "/a", TS: 200})
	mustIngest(t, s, Event{Cmd: "git status", CWD: "/b", TS: 300})

	rows, err := s.PrefixStats(ctx, "git", "/a", 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]StatRow{}
	for _, r := range rows {
		got[r.CWD] = r
	}
	if r := got[""]; r.Count != 3 || r.LastTS != 300 {
		t.Fatalf("rollup row = %+v, want count 3 last_ts 300", r)
	}
	if r := got["/a"]; r.Count != 2 || r.LastTS != 200 {
		t.Fatalf("/a row = %+v, want count 2 last_ts 200", r)
	}
	if _, ok := got["/b"]; ok {
		t.Fatal("query for cwd /a must not return /b rows")
	}
}

func TestPrefixRange(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for _, cmd := range []string{"git checkout main", "git cherry-pick x", "git diff", "go test"} {
		mustIngest(t, s, Event{Cmd: cmd, TS: 1})
	}

	rows, err := s.PrefixStats(ctx, "git c", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 matches for 'git c', got %d: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.Cmd != "git checkout main" && r.Cmd != "git cherry-pick x" {
			t.Fatalf("unexpected match %q", r.Cmd)
		}
	}
}

func TestMostRecentPrefixOrder(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	mustIngest(t, s, Event{Cmd: "make build", TS: 100})
	mustIngest(t, s, Event{Cmd: "make bench", TS: 300})
	mustIngest(t, s, Event{Cmd: "make bootstrap", TS: 200})

	rows, err := s.MostRecentPrefix(ctx, "make b", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Cmd != "make bench" || rows[1].Cmd != "make bootstrap" {
		t.Fatalf("unexpected order/limit: %+v", rows)
	}
}

func TestIngestSkipsRedacted(t *testing.T) {
	s := open(t)
	ok, err := s.Ingest(context.Background(), Event{Cmd: "export API_TOKEN=abc123", TS: 1})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("secret-shaped command must be skipped")
	}
	rows, err := s.PrefixStats(context.Background(), "export", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("skipped command leaked into stats: %+v", rows)
	}
}

func TestPrefixUpperBound(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abc", "abd"},
		{"a", "b"},
		{"ab\xff", "ac"},
		{"\xff\xff", ""},
		{"git c", "git d"},
	}
	for _, c := range cases {
		if got := prefixUpperBound(c.in); got != c.want {
			t.Errorf("prefixUpperBound(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMeta(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if v, _ := s.MetaGet(ctx, "import_done"); v != "" {
		t.Fatalf("unset key should be empty, got %q", v)
	}
	if err := s.MetaSet(ctx, "import_done", "1"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.MetaGet(ctx, "import_done"); v != "1" {
		t.Fatalf("got %q", v)
	}
}
