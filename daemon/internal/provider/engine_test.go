package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"
)

type stubProvider struct {
	name  string
	cs    []Candidate
	err   error
	delay time.Duration
}

func (s *stubProvider) Name() string { return s.name }

func (s *stubProvider) Suggest(ctx context.Context, _ Query) ([]Candidate, error) {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.cs, s.err
}

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(os.Stderr, nil)) }

func budget(d time.Duration) func() time.Duration { return func() time.Duration { return d } }

func TestEngineDropsSlowProvider(t *testing.T) {
	fast := &stubProvider{name: "fast", cs: []Candidate{{Text: "quick", Score: 1, Source: "fast"}}}
	slow := &stubProvider{name: "slow", delay: 2 * time.Second,
		cs: []Candidate{{Text: "late", Score: 99, Source: "slow"}}}

	e := NewEngine(testLog(), budget(30*time.Millisecond), fast, slow)
	start := time.Now()
	got := e.Suggest(context.Background(), Query{Buffer: "q"})
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("engine waited on the slow provider: %v", elapsed)
	}
	if len(got) != 1 || got[0].Text != "quick" {
		t.Fatalf("want fast provider's result only, got %+v", got)
	}
}

func TestEngineIgnoresErroringProvider(t *testing.T) {
	bad := &stubProvider{name: "bad", err: errors.New("boom")}
	ok := &stubProvider{name: "ok", cs: []Candidate{{Text: "fine", Score: 1}}}

	got := NewEngine(testLog(), budget(time.Second), bad, ok).
		Suggest(context.Background(), Query{Buffer: "f"})
	if len(got) != 1 || got[0].Text != "fine" {
		t.Fatalf("got %+v", got)
	}
}

func TestEngineMergesDedupesAndRanks(t *testing.T) {
	a := &stubProvider{name: "a", cs: []Candidate{
		{Text: "git checkout main", Score: 2.0, Source: "a"},
		{Text: "git cherry-pick x", Score: 1.0, Source: "a"},
	}}
	b := &stubProvider{name: "b", cs: []Candidate{
		{Text: "git checkout main", Score: 5.0, Source: "b"}, // dupe, higher score wins
		{Text: "git clean -fd", Score: 3.0, Source: "b"},
	}}

	got := NewEngine(testLog(), budget(time.Second), a, b).
		Suggest(context.Background(), Query{Buffer: "git c"})
	if len(got) != 3 {
		t.Fatalf("want 3 after dedupe, got %d: %+v", len(got), got)
	}
	if got[0].Text != "git checkout main" || got[0].Score != 5.0 || got[0].Source != "b" {
		t.Fatalf("dedupe must keep max score: %+v", got[0])
	}
	if got[1].Text != "git clean -fd" || got[2].Text != "git cherry-pick x" {
		t.Fatalf("ranking wrong: %+v", got)
	}
}

func TestEngineCapsMergedSet(t *testing.T) {
	var cs []Candidate
	for i := 0; i < 30; i++ {
		cs = append(cs, Candidate{Text: fmt.Sprintf("cmd-%d", i), Score: float64(i)})
	}
	got := NewEngine(testLog(), budget(time.Second), &stubProvider{name: "many", cs: cs}).
		Suggest(context.Background(), Query{Buffer: "cmd"})
	if len(got) != CandidateLimit {
		t.Fatalf("want cap %d, got %d", CandidateLimit, len(got))
	}
	if got[0].Text != "cmd-29" {
		t.Fatalf("cap must keep the best, got %+v", got[0])
	}
}

func TestEngineEmptyResultIsNotAnError(t *testing.T) {
	got := NewEngine(testLog(), budget(time.Second), &stubProvider{name: "empty"}).
		Suggest(context.Background(), Query{Buffer: "zzz"})
	if len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}
