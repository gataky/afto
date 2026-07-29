// Command afto-make-targets is a sample afto plugin: it suggests `make`
// targets read from the Makefile in the querying shell's directory.
//
// It exists to demonstrate the thing built-in providers structurally
// cannot do. history and frecency can only suggest commands you have
// already run; this suggests targets from a Makefile you may have cloned
// five minutes ago, in a directory afto has never seen. That is the case
// external plugins are for — knowledge that lives outside your history.
//
// Protocol: docs/plugins.md. Read one JSON request per line from stdin,
// write one JSON response per line to stdout, echo the id. Diagnostics go
// to stderr, which the daemon logs at debug level.
//
// Usage:
//
//	[[plugin]]
//	name    = "make-targets"
//	command = "/path/to/afto-make-targets"
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type request struct {
	V      int    `json:"v"`
	ID     int64  `json:"id"`
	Buffer string `json:"buffer"`
	CWD    string `json:"cwd"`
}

type candidate struct {
	Text  string  `json:"text"`
	Score float64 `json:"score,omitempty"`
	Note  string  `json:"note,omitempty"`
}

type response struct {
	V          int         `json:"v"`
	ID         int64       `json:"id"`
	Candidates []candidate `json:"candidates"`
}

// trigger is the buffer shape this plugin answers: "make" plus at least one
// space, then an optional partial target. Anything else gets an empty
// answer — a plugin that stays quiet outside its niche is a good citizen,
// since every candidate it returns competes for the same few rows.
var trigger = regexp.MustCompile(`^make\s+(\S*)$`)

// targetLine matches a rule like `build: deps` but not a variable
// assignment, a pattern rule, or a special target like .PHONY.
var targetLine = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._/-]*)\s*:{1,2}([^=]|$)`)

func main() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 4096), 1<<20)
	out := bufio.NewWriter(os.Stdout)

	for in.Scan() {
		var req request
		if err := json.Unmarshal(in.Bytes(), &req); err != nil {
			fmt.Fprintf(os.Stderr, "afto-make-targets: bad request: %v\n", err)
			continue
		}
		resp := response{V: 1, ID: req.ID, Candidates: suggest(req)}
		b, err := json.Marshal(resp)
		if err != nil {
			fmt.Fprintf(os.Stderr, "afto-make-targets: marshal: %v\n", err)
			continue
		}
		// Flush every line: the daemon is waiting on this one, and a
		// buffered plugin looks exactly like a slow one.
		out.Write(append(b, '\n'))
		if err := out.Flush(); err != nil {
			return // daemon went away
		}
	}
}

func suggest(req request) []candidate {
	m := trigger.FindStringSubmatch(req.Buffer)
	if m == nil {
		return nil
	}
	partial := m[1]

	targets, err := targets(req.CWD)
	if err != nil {
		fmt.Fprintf(os.Stderr, "afto-make-targets: %v\n", err)
		return nil
	}
	var out []candidate
	for _, t := range targets {
		if !strings.HasPrefix(t, partial) {
			continue
		}
		// Candidates must extend the whole buffer, not just the word: the
		// client displays only strict extensions of what was typed.
		out = append(out, candidate{
			Text:  strings.TrimSuffix(req.Buffer, partial) + t,
			Score: 1.0,
			Note:  "make target",
		})
	}
	return out
}

// targets parses the Makefile in dir. Deliberately shallow: no `include`
// following, no variable expansion, no recursion into submakes. A plugin on
// the keystroke path should do the cheap 90% and stay quiet about the rest.
func targets(dir string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}
	var f *os.File
	var err error
	for _, name := range []string{"Makefile", "makefile", "GNUmakefile"} {
		f, err = os.Open(filepath.Join(dir, name))
		if err == nil {
			break
		}
	}
	if f == nil {
		return nil, nil // no Makefile here: not an error, just nothing to say
	}
	defer f.Close()

	seen := map[string]bool{}
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "#") {
			continue // recipe body or comment
		}
		m := targetLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}
