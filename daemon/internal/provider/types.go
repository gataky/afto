// Package provider defines the suggestion domain types and the Provider
// interface every suggestion source implements (DESIGN.md §3.1).
//
// The provider model is afto's single extension point. Built-in sources
// (history, frecency — and in later phases transitions, alias notes, an
// opt-in AI backend, and external subprocess plugins) all implement the same
// three-method interface and are raced concurrently against a latency
// budget by the engine. Whatever has answered when the budget expires gets
// merged and ranked; stragglers are abandoned for that request. This is how
// a slow or broken source degrades itself instead of the user's prompt.
//
// Nothing in this package touches the wire: ipc converts requests into a
// Query, providers consult whatever backend they own, and the daemon
// composes the two. provider must never import ipc (the dependency points
// the other way). The built-in providers do read the store, but only
// through the narrow statsReader interface defined here, so tests fake the
// data layer instead of standing up SQLite.
package provider

import "context"

// CandidateLimit caps ranked candidates at every layer: what a single
// provider returns, what the engine's merge emits, and the largest "limit" a
// client may request over the wire (ipc clamps to it).
const CandidateLimit = 10

// Query is the context a suggestion is computed from. It already carries
// everything a future AI provider would need; extending it is additive.
type Query struct {
	Buffer   string
	Cursor   int
	CWD      string
	LastExit int
	Session  string
	Recent   []string
}

// Candidate is one ranked suggestion. Text should extend Query.Buffer by
// construction; the shell client re-verifies regardless (prefix invariant).
type Candidate struct {
	Text   string
	Score  float64
	Source string
	Note   string
}

// Provider is a suggestion source. Implementations must respect ctx: the
// engine races providers against a latency budget and abandons stragglers.
type Provider interface {
	Name() string
	Suggest(ctx context.Context, q Query) ([]Candidate, error)
}
