package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeMakefile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const sample = `
# a comment
VERSION := 1.2.3
BIN = bin/aftod

.PHONY: build test
.DEFAULT_GOAL := build

build: $(BIN)
	go build ./...

test-e2e:: deps
	zsh tests/e2e/harness.zsh

deps:
	go mod download

%.o: %.c
	cc -c $<

build:
	@echo duplicate rule for an existing target
`

func TestTargets(t *testing.T) {
	got, err := targets(writeMakefile(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build", "deps", "test-e2e"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTargetsIgnoresNonTargets(t *testing.T) {
	// Each line here has tripped up naive "split on colon" parsers.
	got, err := targets(writeMakefile(t, `
VERSION := 1.0
LDFLAGS = -X main.version=$(VERSION)
export PATH := /usr/bin:$(PATH)
	@echo "indented: recipe line"
# commented: out
.PHONY: all
%.o: %.c
real-target: dep
`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"real-target"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTargetsMissingMakefileIsQuiet(t *testing.T) {
	got, err := targets(t.TempDir())
	if err != nil || got != nil {
		t.Fatalf("got %v err=%v — a directory without a Makefile is not an error", got, err)
	}
	if got, err := targets(""); err != nil || got != nil {
		t.Fatalf("empty cwd: got %v err=%v", got, err)
	}
}

func TestSuggestOnlyFiresOnMakeBuffers(t *testing.T) {
	dir := writeMakefile(t, sample)
	for _, buf := range []string{"", "mak", "make", "git make ", "echo make x", "make build extra "} {
		if got := suggest(request{Buffer: buf, CWD: dir}); got != nil {
			t.Errorf("buffer %q should produce nothing, got %+v", buf, got)
		}
	}
}

func TestSuggestExtendsTheWholeBuffer(t *testing.T) {
	dir := writeMakefile(t, sample)

	got := suggest(request{Buffer: "make ", CWD: dir})
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	// The client only displays strict extensions of the whole buffer, so a
	// bare target name would be silently dropped.
	if got[0].Text != "make build" || got[0].Note != "make target" {
		t.Fatalf("candidate = %+v", got[0])
	}

	got = suggest(request{Buffer: "make te", CWD: dir})
	if len(got) != 1 || got[0].Text != "make test-e2e" {
		t.Fatalf("partial target: %+v", got)
	}

	if got := suggest(request{Buffer: "make zzz", CWD: dir}); got != nil {
		t.Fatalf("no match should be silence, got %+v", got)
	}
}
