package provider

import (
	"context"
	"time"

	"github.com/gataky/afto/daemon/internal/scoring"
	"github.com/gataky/afto/daemon/internal/store"
)

// transitionReader is the slice of the store this provider needs (same
// consumer-side interface pattern as statsReader).
type transitionReader interface {
	TopNext(ctx context.Context, prev string, limit int) ([]store.TransitionRow, error)
	LastCommand(ctx context.Context, session string) (string, error)
}

// Transition predicts the NEXT command rather than completing the current
// one: after `git add -p`, you probably want `git commit`. It is the only
// provider that answers an empty buffer, and the only one whose candidates
// do not extend what the user typed — which is why the shell shows them as
// menu rows only, never as ghost text that an accept key could take
// (DESIGN.md §2.4.3).
//
// The signal comes from the transitions table, which counts how often B
// followed A within one shell session. Imported history contributes nothing
// to it by design (a HISTFILE interleaves terminals), so this provider is
// quiet on a fresh install and gets better with a day of use.
type Transition struct {
	trans transitionReader
	now   func() time.Time
}

func NewTransition(t transitionReader) *Transition {
	return &Transition{trans: t, now: time.Now}
}

func (t *Transition) Name() string { return "transition" }

func (t *Transition) Suggest(ctx context.Context, q Query) ([]Candidate, error) {
	// A non-empty buffer is the other providers' job: what the user has
	// started typing is a better signal than what usually follows.
	if q.Buffer != "" {
		return nil, nil
	}

	// The shell sends its last command as Recent[0]; asking the store is
	// the fallback for clients that send no context (aftod query, tests).
	prev := ""
	if len(q.Recent) > 0 {
		prev = q.Recent[0]
	}
	if prev == "" {
		var err error
		if prev, err = t.trans.LastCommand(ctx, q.Session); err != nil {
			return nil, err
		}
	}
	if prev == "" {
		return nil, nil // first command of a session: nothing to predict from
	}

	rows, err := t.trans.TopNext(ctx, prev, CandidateLimit)
	if err != nil {
		return nil, err
	}
	now := t.now()
	out := make([]Candidate, 0, len(rows))
	for _, r := range rows {
		age := now.Sub(time.Unix(r.LastTS, 0)).Hours()
		out = append(out, Candidate{
			Text: r.Next,
			// Same shape and scale as the other providers' global term, so
			// a merged set stays comparable.
			Score:  scoring.Frecency(scoring.Term{Count: r.Count, AgeHours: age}, scoring.Term{}, scoring.Term{}),
			Source: "transition",
		})
	}
	return out, nil
}
