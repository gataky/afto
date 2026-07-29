package provider

import (
	"context"
	"testing"

	"github.com/gataky/afto/daemon/internal/store"
)

// fakeTransitions implements transitionReader without SQLite.
type fakeTransitions struct {
	next    map[string][]store.TransitionRow
	last    map[string]string // session → last command
	lastHit int               // how often the store fallback was consulted
}

func (f *fakeTransitions) TopNext(_ context.Context, prev string, limit int) ([]store.TransitionRow, error) {
	rows := f.next[prev]
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (f *fakeTransitions) LastCommand(_ context.Context, session string) (string, error) {
	f.lastHit++
	return f.last[session], nil
}

func newFakeTransitions() *fakeTransitions {
	return &fakeTransitions{
		next: map[string][]store.TransitionRow{
			"git add -p": {
				{Next: "git commit", Count: 8, LastTS: ts(2)},
				{Next: "git diff --cached", Count: 2, LastTS: ts(30)},
			},
		},
		last: map[string]string{"s1": "git add -p"},
	}
}

func TestTransitionPredictsFromRecent(t *testing.T) {
	f := newFakeTransitions()
	p := NewTransition(f)
	p.now = fixedNow

	got, err := p.Suggest(context.Background(), Query{Recent: []string{"git add -p"}, Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Text != "git commit" || got[1].Text != "git diff --cached" {
		t.Fatalf("unexpected predictions: %+v", got)
	}
	if got[0].Source != "transition" || got[0].Score <= 0 {
		t.Fatalf("bad metadata: %+v", got[0])
	}
	if got[0].Score <= got[1].Score {
		t.Fatalf("frequent+fresh pair must outrank rare+stale: %+v", got)
	}
	// The shell's context is authoritative; no store round trip needed.
	if f.lastHit != 0 {
		t.Fatalf("store consulted despite Recent being present (%d times)", f.lastHit)
	}
}

func TestTransitionFallsBackToStoredSessionTail(t *testing.T) {
	f := newFakeTransitions()
	p := NewTransition(f)
	p.now = fixedNow

	// No Recent (e.g. `aftod query`): the session's last recorded command
	// stands in.
	got, err := p.Suggest(context.Background(), Query{Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Text != "git commit" {
		t.Fatalf("fallback failed: %+v", got)
	}
	if f.lastHit != 1 {
		t.Fatalf("expected exactly one store lookup, got %d", f.lastHit)
	}
}

func TestTransitionSilentWhenNothingToPredictFrom(t *testing.T) {
	f := newFakeTransitions()
	p := NewTransition(f)
	p.now = fixedNow
	ctx := context.Background()

	// Unknown session and no Recent: first command of a fresh shell.
	if got, _ := p.Suggest(ctx, Query{Session: "brand-new"}); len(got) != 0 {
		t.Fatalf("want nothing for a session with no history, got %+v", got)
	}
	// A previous command nobody has ever followed.
	if got, _ := p.Suggest(ctx, Query{Recent: []string{"cowsay moo"}}); len(got) != 0 {
		t.Fatalf("want nothing for an unseen prev, got %+v", got)
	}
}

func TestTransitionIgnoresNonEmptyBuffer(t *testing.T) {
	f := newFakeTransitions()
	p := NewTransition(f)
	p.now = fixedNow

	// Once the user types, prefix providers own the answer — a prediction
	// that ignored the buffer could not be displayed anyway.
	got, err := p.Suggest(context.Background(), Query{Buffer: "gi", Recent: []string{"git add -p"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want nothing for a non-empty buffer, got %+v", got)
	}
}
