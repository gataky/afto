package provider

import (
	"context"
	"testing"
	"time"
)

func decorated(a *AliasNote, session string, texts ...string) []Candidate {
	cs := make([]Candidate, 0, len(texts))
	for _, t := range texts {
		cs = append(cs, Candidate{Text: t})
	}
	return a.Decorate(Query{Session: session}, cs)
}

func TestAliasNoteAnnotatesFirstWord(t *testing.T) {
	a := NewAliasNote()
	a.Set("s1", map[string]string{"gco": "git checkout", "ll": "ls -la"})

	got := decorated(a, "s1", "gco main", "ll", "git status", "  gco -b x")
	want := []string{
		"gco = git checkout",
		"ll = ls -la",
		"",                   // not an alias
		"gco = git checkout", // leading whitespace is not part of the word
	}
	for i := range want {
		if got[i].Note != want[i] {
			t.Errorf("candidate %q: note = %q, want %q", got[i].Text, got[i].Note, want[i])
		}
	}
	// Annotation must never touch the text that can reach the buffer.
	if got[0].Text != "gco main" {
		t.Fatalf("decorator rewrote the candidate: %q", got[0].Text)
	}
}

func TestAliasNoteIsPerSession(t *testing.T) {
	a := NewAliasNote()
	a.Set("s1", map[string]string{"gco": "git checkout"})

	if got := decorated(a, "s2", "gco main"); got[0].Note != "" {
		t.Fatalf("session s2 saw s1's aliases: %q", got[0].Note)
	}
	if got := decorated(a, "", "gco main"); got[0].Note != "" {
		t.Fatalf("sessionless query got a note: %q", got[0].Note)
	}
	// A later table replaces the earlier one wholesale.
	a.Set("s1", map[string]string{"gs": "git status"})
	if got := decorated(a, "s1", "gco main"); got[0].Note != "" {
		t.Fatalf("stale alias survived a table replacement: %q", got[0].Note)
	}
	if got := decorated(a, "s1", "gs"); got[0].Note != "gs = git status" {
		t.Fatalf("new table not applied: %q", got[0].Note)
	}
}

func TestAliasNoteClearAndForget(t *testing.T) {
	a := NewAliasNote()
	a.Set("s1", map[string]string{"gco": "git checkout"})
	a.Set("s1", nil) // shell unset its last alias
	if got := decorated(a, "s1", "gco main"); got[0].Note != "" {
		t.Fatalf("cleared table still annotating: %q", got[0].Note)
	}

	a.Set("s2", map[string]string{"gco": "git checkout"})
	a.Forget("s2")
	if got := decorated(a, "s2", "gco main"); got[0].Note != "" {
		t.Fatalf("forgotten session still annotating: %q", got[0].Note)
	}
}

func TestAliasNoteKeepsProviderNotes(t *testing.T) {
	a := NewAliasNote()
	a.Set("s1", map[string]string{"gco": "git checkout"})
	cs := a.Decorate(Query{Session: "s1"}, []Candidate{{Text: "gco main", Note: "from a plugin"}})
	if cs[0].Note != "from a plugin" {
		t.Fatalf("overwrote a provider's own note: %q", cs[0].Note)
	}
}

func TestEngineRunsDecorators(t *testing.T) {
	a := NewAliasNote()
	a.Set("s1", map[string]string{"gco": "git checkout"})
	e := NewEngine(testLog(), budget(time.Second), &stubProvider{
		name: "fake", cs: []Candidate{{Text: "gco main", Score: 1, Source: "fake"}},
	})
	e.Use(a)

	got := e.Suggest(context.Background(), Query{Buffer: "gco", Session: "s1"})
	if len(got) != 1 || got[0].Note != "gco = git checkout" {
		t.Fatalf("engine did not decorate: %+v", got)
	}
}
