package provider

import (
	"context"
	"sort"
	"time"

	"github.com/gataky/afto/daemon/internal/scoring"
	"github.com/gataky/afto/daemon/internal/store"
)

// scanLimit bounds the stats range scan for very short prefixes (typing "g"
// can match thousands of distinct commands). Truncation risk is covered by
// the History provider; see its doc comment.
const scanLimit = 2000

// Frecency ranks prefix matches by frequency × recency × cwd affinity
// (scoring package). This is the provider that makes suggestions feel
// personal: "make e2e" in this repo outranks "make -C ~/other build"
// globally, because the (cmd, cwd) stats row adds the affinity term.
type Frecency struct {
	stats statsReader
	now   func() time.Time
}

func NewFrecency(s statsReader) *Frecency {
	return &Frecency{stats: s, now: time.Now}
}

func (f *Frecency) Name() string { return "frecency" }

func (f *Frecency) Suggest(ctx context.Context, q Query) ([]Candidate, error) {
	if q.Buffer == "" {
		return nil, nil
	}
	rows, err := f.stats.PrefixStats(ctx, q.Buffer, q.CWD, scanLimit)
	if err != nil {
		return nil, err
	}

	// Fold the two row kinds per command: the cwd='' rollup carries the
	// global term, the cwd-specific row the affinity term.
	type terms struct {
		all, cwd store.StatRow
	}
	byCmd := make(map[string]*terms)
	for _, r := range rows {
		if !usable(r.Cmd, q.Buffer) {
			continue
		}
		tr := byCmd[r.Cmd]
		if tr == nil {
			tr = &terms{}
			byCmd[r.Cmd] = tr
		}
		if r.CWD == "" {
			tr.all = r
		} else {
			tr.cwd = r
		}
	}

	now := f.now()
	age := func(ts int64) float64 { return now.Sub(time.Unix(ts, 0)).Hours() }
	out := make([]Candidate, 0, len(byCmd))
	for cmd, tr := range byCmd {
		out = append(out, Candidate{
			Text:   cmd,
			Score:  scoring.Frecency(tr.all.Count, age(tr.all.LastTS), tr.cwd.Count, age(tr.cwd.LastTS)),
			Source: "frecency",
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > candidateLimit {
		out = out[:candidateLimit]
	}
	return out, nil
}
