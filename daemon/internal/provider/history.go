package provider

import (
	"context"
	"strings"
	"time"

	"github.com/gataky/afto/daemon/internal/scoring"
	"github.com/gataky/afto/daemon/internal/store"
)

// statsReader is the slice of *store.Store the providers consume. Defined
// here (consumer side) so provider tests can fake the data layer without
// SQLite, and so this package never grows a dependency on storage details
// beyond these two queries.
type statsReader interface {
	PrefixStats(ctx context.Context, prefix, cwd string, limit int) ([]store.StatRow, error)
	MostRecentPrefix(ctx context.Context, prefix string, limit int) ([]store.StatRow, error)
}

// History suggests the most recently used commands matching the buffer —
// pure recency, no frequency weighting. It exists for two reasons:
//
//  1. PoC parity: poc/afto.plugin.zsh suggested the most recent matching
//     history entry, and that behavior must survive the daemon swap.
//  2. A safety net for Frecency's scan cap: for very short prefixes,
//     Frecency bounds its row scan and can truncate away the most recent
//     match; History's recency-ordered query guarantees it stays in the
//     candidate set.
//
// Scores use the frecency formula's global term (count + recency decay), so
// they land on the same scale as Frecency's and merge sanely.
type History struct {
	stats statsReader
	now   func() time.Time // injectable clock for tests
}

func NewHistory(s statsReader) *History {
	return &History{stats: s, now: time.Now}
}

func (h *History) Name() string { return "history" }

func (h *History) Suggest(ctx context.Context, q Query) ([]Candidate, error) {
	if q.Buffer == "" {
		return nil, nil
	}
	rows, err := h.stats.MostRecentPrefix(ctx, q.Buffer, CandidateLimit)
	if err != nil {
		return nil, err
	}
	now := h.now()
	var out []Candidate
	for _, r := range rows {
		if !usable(r.Cmd, q.Buffer) {
			continue
		}
		age := now.Sub(time.Unix(r.LastTS, 0)).Hours()
		out = append(out, Candidate{
			Text:   r.Cmd,
			Score:  scoring.Frecency(r.Count, age, 0, 0),
			Source: "history",
		})
	}
	return out, nil
}

// usable filters candidates no provider should emit: the buffer itself
// (a "suggestion" equal to what's typed ghosts nothing) and multiline
// commands (ghost text is single-line in Phase 1; a multiline command CAN
// prefix-extend a single-line buffer, so the client-side prefix invariant
// alone would not reject it).
func usable(cmd, buffer string) bool {
	return cmd != buffer && !strings.Contains(cmd, "\n")
}
