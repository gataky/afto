package provider

import (
	"context"
	"log/slog"
	"sort"
	"time"
)

// Engine races every enabled provider against a latency budget and merges
// whatever answered in time (DESIGN.md §3.1). This is the mechanism behind
// the "failure is absence" contract: a provider that is slow, erroring, or
// deadlocked costs at most the budget — it can never hang the prompt,
// because the engine returns without it and the shell just shows what came
// back (possibly nothing).
type Engine struct {
	providers []Provider
	budget    func() time.Duration // func, not value: config hot-reload swaps it
	log       *slog.Logger
}

func NewEngine(log *slog.Logger, budget func() time.Duration, providers ...Provider) *Engine {
	return &Engine{providers: providers, budget: budget, log: log}
}

// Suggest fans out to all providers and merges. Always returns within the
// budget (plus scheduling noise); never returns an error — an empty slice
// is the wire representation of every failure mode.
func (e *Engine) Suggest(ctx context.Context, q Query) []Candidate {
	ctx, cancel := context.WithTimeout(ctx, e.budget())
	defer cancel()

	type result struct {
		name string
		cs   []Candidate
		err  error
	}
	ch := make(chan result, len(e.providers)) // buffered: stragglers must not leak goroutines
	for _, p := range e.providers {
		go func(p Provider) {
			cs, err := p.Suggest(ctx, q)
			ch <- result{p.Name(), cs, err}
		}(p)
	}

	var all []Candidate
	for range e.providers {
		select {
		case r := <-ch:
			if r.err != nil {
				e.log.Debug("provider error", "provider", r.name, "err", r.err)
				continue
			}
			all = append(all, r.cs...)
		case <-ctx.Done():
			e.log.Debug("latency budget expired; merging partial results",
				"got", len(all), "budget", e.budget())
			return merge(all)
		}
	}
	return merge(all)
}

// merge dedupes identical texts (keeping the max score — providers score on
// a shared scale, see History's doc), orders by score descending with a
// stable sort, and caps the set. Capping here rather than per-provider
// keeps the TSV top-1 and the future JSON consumers consistent.
func merge(cs []Candidate) []Candidate {
	best := make(map[string]int, len(cs)) // text → index in out
	out := make([]Candidate, 0, len(cs))
	for _, c := range cs {
		if i, seen := best[c.Text]; seen {
			if c.Score > out[i].Score {
				out[i] = c
			}
			continue
		}
		best[c.Text] = len(out)
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > CandidateLimit {
		out = out[:CandidateLimit]
	}
	return out
}
