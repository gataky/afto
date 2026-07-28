package store

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ImportStats summarizes a HISTFILE import.
type ImportStats struct {
	Lines    int // physical lines read
	Commands int // logical commands parsed
	Imported int // commands persisted
	Redacted int // commands skipped by redaction
}

// extendedRe matches a zsh EXTENDED_HISTORY entry: ": <start>:<elapsed>;cmd".
var extendedRe = regexp.MustCompile(`^: (\d+):(\d+);(.*)$`)

// ImportHistfile ingests an existing zsh history file so afto is useful in
// minute one (plans/phase-1.md §6). It handles:
//
//   - EXTENDED_HISTORY format (": <ts>:<elapsed>;cmd") with real timestamps.
//     A line that doesn't match the entry pattern is a continuation of the
//     previous command (multiline commands are stored with literal
//     newlines), so it is appended.
//
//   - Plain format (one command per line). Timestamps are synthesized to
//     preserve relative recency: the last line gets "now", earlier lines
//     one second older each — enough for sane frecency, without pretending
//     we know real times. Multiline commands cannot be distinguished from
//     consecutive commands in this format; each line imports separately
//     (zsh itself has the same ambiguity reading such files).
//
//   - Metafied bytes: zsh escapes bytes >= 0x80 in the histfile as
//     0x83 followed by (byte XOR 0x20). Decoded before anything else, or
//     every multibyte command would import corrupted.
//
// Redaction applies to every command. Events rows get session='import' and
// cwd='' (the original directories are unknowable). Aggregation happens in
// memory first, then one transaction writes everything — importing a
// 100k-line histfile stays in the low seconds.
func (s *Store) ImportHistfile(ctx context.Context, path string) (ImportStats, error) {
	var st ImportStats
	raw, err := os.ReadFile(path)
	if err != nil {
		return st, fmt.Errorf("store: import: %w", err)
	}
	lines := strings.Split(string(unmetafy(raw)), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // trailing newline
	}
	st.Lines = len(lines)

	type entry struct {
		cmd string
		ts  int64
	}
	var entries []entry
	extendedMode := len(lines) > 0 && extendedRe.MatchString(lines[0])

	if extendedMode {
		var cur *entry
		for _, line := range lines {
			if m := extendedRe.FindStringSubmatch(line); m != nil {
				if cur != nil {
					entries = append(entries, *cur)
				}
				ts, _ := strconv.ParseInt(m[1], 10, 64)
				cur = &entry{cmd: m[3], ts: ts}
			} else if cur != nil {
				cur.cmd += "\n" + line
			}
		}
		if cur != nil {
			entries = append(entries, *cur)
		}
	} else {
		now := time.Now().Unix()
		for i, line := range lines {
			if line == "" {
				continue
			}
			entries = append(entries, entry{cmd: line, ts: now - int64(len(lines)-1-i)})
		}
	}
	st.Commands = len(entries)

	// Aggregate before touching the database.
	type agg struct {
		count  int64
		lastTS int64
	}
	stats := make(map[string]*agg)
	var kept []entry
	for _, e := range entries {
		if e.cmd == "" || s.redactor.Skip(e.cmd) {
			st.Redacted++
			continue
		}
		kept = append(kept, e)
		a := stats[e.cmd]
		if a == nil {
			a = &agg{}
			stats[e.cmd] = a
		}
		a.count++
		if e.ts > a.lastTS {
			a.lastTS = e.ts
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return st, fmt.Errorf("store: import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	insEvent, err := tx.PrepareContext(ctx,
		`INSERT INTO events(cmd,cwd,session,exit_code,ts) VALUES(?,?,?,?,?)`)
	if err != nil {
		return st, fmt.Errorf("store: import: %w", err)
	}
	defer insEvent.Close()
	for _, e := range kept {
		if _, err := insEvent.ExecContext(ctx, e.cmd, "", "import", 0, e.ts); err != nil {
			return st, fmt.Errorf("store: import: %w", err)
		}
	}
	for cmd, a := range stats {
		if err := upsertStat(ctx, tx, cmd, "", a.lastTS, a.count); err != nil {
			return st, err
		}
	}
	if err := tx.Commit(); err != nil {
		return st, fmt.Errorf("store: import: %w", err)
	}
	st.Imported = len(kept)
	return st, nil
}

// unmetafy reverses zsh's histfile metafication: 0x83 (Meta) followed by b
// decodes to b^0x20. See zsh's utils.c:unmetafy.
func unmetafy(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == 0x83 && i+1 < len(b) {
			i++
			out = append(out, b[i]^0x20)
			continue
		}
		out = append(out, b[i])
	}
	return out
}
