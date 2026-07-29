package provider

import (
	"strings"
	"sync"
)

// Decorator annotates candidates after the merge, without producing any of
// its own. It is a separate stage from Provider on purpose: a provider owns
// a source of suggestions, while a decorator adds information to whatever
// the sources produced. (DESIGN.md §3.1 lists alias-note among the
// providers; the interface shape is wrong for it — see plans/phase-3.md §5.)
//
// Decorators run inline after the race, so they must be cheap and must
// never block: they are inside the latency budget's shadow, not protected
// by it.
type Decorator interface {
	Name() string
	Decorate(q Query, cs []Candidate) []Candidate
}

// AliasNote annotates candidates whose first word is an alias with what
// that alias expands to: `gco main` gets the note "gco = git checkout".
//
// This is deliberately informational only. Knowing that `gco` means
// `git checkout` is useful; rewriting the user's line to say so is the
// disruptive IRIS behavior this project rejects (DESIGN.md §3.1) — the note
// is display text that never enters $BUFFER.
//
// The alias table belongs to the shell, so the shell ships it (one
// fire-and-forget message per session, resent when it changes). It is held
// per session in memory and never persisted: alias definitions are user
// configuration, not command history, and the store is for the latter.
type AliasNote struct {
	mu        sync.RWMutex
	bySession map[string]map[string]string
}

func NewAliasNote() *AliasNote {
	return &AliasNote{bySession: map[string]map[string]string{}}
}

func (a *AliasNote) Name() string { return "alias-note" }

// Set installs (or replaces) a session's alias table. An empty map clears
// it, which is what a shell that unset its last alias would send.
func (a *AliasNote) Set(session string, m map[string]string) {
	if session == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(m) == 0 {
		delete(a.bySession, session)
		return
	}
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	a.bySession[session] = cp
}

// Forget drops a session's table (called when its connection closes).
func (a *AliasNote) Forget(session string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.bySession, session)
}

func (a *AliasNote) Decorate(q Query, cs []Candidate) []Candidate {
	a.mu.RLock()
	table := a.bySession[q.Session]
	a.mu.RUnlock()
	if len(table) == 0 {
		return cs
	}
	for i := range cs {
		if cs[i].Note != "" {
			continue // a provider's own note wins; don't overwrite it
		}
		word := firstWord(cs[i].Text)
		if word == "" {
			continue
		}
		if exp, ok := table[word]; ok && exp != "" {
			cs[i].Note = word + " = " + exp
		}
	}
	return cs
}

// firstWord returns the command word of a line — what the shell would
// look up in its alias table. Only a leading bare word can be an alias, so
// anything quoted, assigned, or path-like simply won't match a table entry
// and needs no special handling here.
func firstWord(s string) string {
	s = strings.TrimLeft(s, " \t")
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}
