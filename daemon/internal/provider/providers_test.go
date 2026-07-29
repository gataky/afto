package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gataky/afto/daemon/internal/store"
)

// fakeStats implements statsReader without SQLite.
type fakeStats struct {
	prefix []store.StatRow // returned by PrefixStats
	recent []store.StatRow // returned by MostRecentPrefix
}

// Mirrors the real query's scope: the rollup, the exact cwd, and — when a
// root is given — anything inside it (path-boundary aware, so a sibling
// sharing a name prefix is excluded).
func (f *fakeStats) PrefixStats(_ context.Context, prefix, cwd, root string, _ int) ([]store.StatRow, error) {
	inProject := func(dir string) bool {
		return root != "" && (dir == root || strings.HasPrefix(dir, root+"/"))
	}
	var out []store.StatRow
	for _, r := range f.prefix {
		if !strings.HasPrefix(r.Cmd, prefix) {
			continue
		}
		if r.CWD == "" || r.CWD == cwd || inProject(r.CWD) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStats) MostRecentPrefix(_ context.Context, prefix string, limit int) ([]store.StatRow, error) {
	var out []store.StatRow
	for _, r := range f.recent {
		if strings.HasPrefix(r.Cmd, prefix) && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

var testNow = time.Unix(1_722_200_000, 0)

func fixedNow() time.Time { return testNow }

func ts(ageHours float64) int64 {
	return testNow.Add(-time.Duration(ageHours * float64(time.Hour))).Unix()
}

func TestHistoryProvider(t *testing.T) {
	h := NewHistory(&fakeStats{recent: []store.StatRow{
		{Cmd: "git checkout main", Count: 3, LastTS: ts(1)},
		{Cmd: "git ch", Count: 1, LastTS: ts(2)},                      // == buffer, must drop
		{Cmd: "git checkout -b x\ngit push", Count: 1, LastTS: ts(3)}, // multiline, must drop
		{Cmd: "git cherry-pick abc", Count: 1, LastTS: ts(4)},
	}})
	h.now = fixedNow

	got, err := h.Suggest(context.Background(), Query{Buffer: "git ch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Text != "git checkout main" || got[1].Text != "git cherry-pick abc" {
		t.Fatalf("unexpected candidates: %+v", got)
	}
	if got[0].Source != "history" || got[0].Score <= 0 {
		t.Fatalf("bad metadata: %+v", got[0])
	}
}

func TestFrecencyCwdAffinity(t *testing.T) {
	// "make e2e" runs occasionally but in THIS repo; "make deploy" runs more
	// overall but never here. Affinity must win.
	f := NewFrecency(&fakeStats{prefix: []store.StatRow{
		{Cmd: "make e2e", CWD: "", Count: 4, LastTS: ts(2)},
		{Cmd: "make e2e", CWD: "/repo", Count: 4, LastTS: ts(2)},
		{Cmd: "make deploy", CWD: "", Count: 9, LastTS: ts(2)},
	}})
	f.now = fixedNow

	got, err := f.Suggest(context.Background(), Query{Buffer: "make", CWD: "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Text != "make e2e" {
		t.Fatalf("cwd affinity did not win: %+v", got)
	}
}

func TestFrecencyDropsBufferAndMultiline(t *testing.T) {
	f := NewFrecency(&fakeStats{prefix: []store.StatRow{
		{Cmd: "ls", CWD: "", Count: 5, LastTS: ts(1)},
		{Cmd: "ls -la\npwd", CWD: "", Count: 5, LastTS: ts(1)},
	}})
	f.now = fixedNow

	got, err := f.Suggest(context.Background(), Query{Buffer: "ls"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want nothing, got %+v", got)
	}
}

func TestProvidersReturnNilOnEmptyBuffer(t *testing.T) {
	h := NewHistory(&fakeStats{})
	f := NewFrecency(&fakeStats{})
	if cs, _ := h.Suggest(context.Background(), Query{}); cs != nil {
		t.Fatal("history must not suggest on empty buffer")
	}
	if cs, _ := f.Suggest(context.Background(), Query{}); cs != nil {
		t.Fatal("frecency must not suggest on empty buffer")
	}
}
