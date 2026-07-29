package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gataky/afto/daemon/internal/ipc"
	"github.com/gataky/afto/daemon/internal/project"
	"github.com/gataky/afto/daemon/internal/scoring"
	"github.com/gataky/afto/daemon/internal/store"
)

// This file is aftod acting as a CLIENT of a running daemon (or, for
// import, of the store directly): one-shot commands for humans, tests and
// the shell's `afto status`. The interactive client is the zsh plugin —
// see docs/protocol.md for who talks to whom.

func dialDaemon(socket string) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", socket, 500*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("daemon not reachable at %s (start one with `aftod serve`): %w", socket, err)
	}
	return conn, nil
}

// roundTrip sends one request and returns one response line.
func roundTrip(socket string, req ipc.Request) (string, error) {
	conn, err := dialDaemon(socket)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	b, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return "", err
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("no response: %w", err)
	}
	return line, nil
}

func cmdPing(args []string) error {
	fs := flag.NewFlagSet("ping", flag.ExitOnError)
	socket := fs.String("socket", socketPath(), "daemon socket path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	line, err := roundTrip(*socket, ipc.Request{V: ipc.V, Type: ipc.TypePing})
	if err != nil {
		return err
	}
	fmt.Print(line)
	return nil
}

func cmdQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	socket := fs.String("socket", socketPath(), "daemon socket path")
	buffer := fs.String("buffer", "", "command-line prefix to suggest for")
	cwd := fs.String("cwd", "", "working directory context")
	limit := fs.Int("limit", 0, "max candidates (0 = all, capped at 10)")
	// An empty buffer is a real query, not a mistake: it asks what usually
	// comes NEXT, which needs a session to know what came last.
	session := fs.String("session", "", "session id, for empty-buffer next-command prediction")
	recent := fs.String("recent", "", "the command just executed (overrides the session's stored tail)")
	list := fs.Bool("list", false, "print candidate texts one per line instead of JSON (for piping to fzf)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cwd == "" {
		*cwd, _ = os.Getwd()
	}
	var recents []string
	if *recent != "" {
		recents = []string{*recent}
	}
	line, err := roundTrip(*socket, ipc.Request{
		V: ipc.V, Type: ipc.TypeSuggest, ID: 1,
		Buffer: *buffer, Cursor: len(*buffer), CWD: *cwd, Limit: *limit,
		Session: *session, Recent: recents,
	})
	if err != nil {
		return err
	}
	if !*list {
		fmt.Print(line)
		return nil
	}
	var resp ipc.SuggestResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return fmt.Errorf("query: bad response: %w", err)
	}
	for _, c := range resp.Candidates {
		fmt.Println(c.Text)
	}
	return nil
}

// cmdList ranks history straight from the store rather than through the
// daemon — the same direct-access pattern cmdImport uses, and safe for the
// same reason (WAL plus busy_timeout).
//
// It exists alongside `query --list` because the picker use case wants
// something `query` deliberately does not provide: with no prefix, the
// WHOLE history ranked by frecency. An empty buffer means "predict what
// comes next" to the suggestion path, which is the right answer for ghost
// text and the wrong one for a fuzzy finder. Reading the store directly
// also means the ^R replacement works when no daemon is running.
func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	data := fs.String("data-dir", dataDir(), "store directory")
	prefix := fs.String("prefix", "", "only commands starting with this")
	cwd := fs.String("cwd", "", "working directory context for ranking (default: current)")
	limit := fs.Int("limit", 1000, "maximum commands to print")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cwd == "" {
		*cwd, _ = os.Getwd()
	}

	st, err := store.Open(*data)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	root := project.New(nil).Root(*cwd)
	// Scan wider than the output limit: ranking picks the best of what it
	// sees, so a scan capped at the print limit would bias toward whatever
	// SQLite happened to return first.
	scan := *limit * 4
	if scan < 4000 {
		scan = 4000
	}
	rows, err := st.PrefixStats(ctx, *prefix, *cwd, root, scan)
	if err != nil {
		return err
	}

	type agg struct {
		all, cwd, proj scoring.Term
	}
	now := time.Now()
	age := func(ts int64) float64 { return now.Sub(time.Unix(ts, 0)).Hours() }
	byCmd := map[string]*agg{}
	for _, r := range rows {
		a := byCmd[r.Cmd]
		if a == nil {
			a = &agg{}
			byCmd[r.Cmd] = a
		}
		switch {
		case r.CWD == "":
			a.all = scoring.Term{Count: r.Count, AgeHours: age(r.LastTS)}
			continue
		case r.CWD == *cwd:
			a.cwd = scoring.Term{Count: r.Count, AgeHours: age(r.LastTS)}
		}
		if root != "" {
			a.proj.Count += r.Count
			if h := age(r.LastTS); a.proj.Count == r.Count || h < a.proj.AgeHours {
				a.proj.AgeHours = h
			}
		}
	}

	type scored struct {
		cmd   string
		score float64
	}
	out := make([]scored, 0, len(byCmd))
	for cmd, a := range byCmd {
		out = append(out, scored{cmd, scoring.Frecency(a.all, a.cwd, a.proj)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	if len(out) > *limit {
		out = out[:*limit]
	}

	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	for _, s := range out {
		// A command containing a newline would corrupt a line-oriented
		// picker's idea of where entries end.
		if strings.ContainsAny(s.cmd, "\n\r") {
			continue
		}
		fmt.Fprintln(w, s.cmd)
	}
	return nil
}

// cmdImport operates on the store directly rather than through a daemon —
// WAL mode plus busy_timeout make that safe alongside a running one. It
// also marks import_done so a later daemon start doesn't re-import on top.
func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	histfile := fs.String("histfile", histfilePath(), "history file to import")
	data := fs.String("data-dir", dataDir(), "store directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *histfile == "" {
		return fmt.Errorf("import: no history file found; pass --histfile")
	}
	st, err := store.Open(*data)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	stats, err := st.ImportHistfile(ctx, *histfile)
	if err != nil {
		return err
	}
	if err := st.MetaSet(ctx, "import_done", "1"); err != nil {
		return err
	}
	fmt.Printf("imported %s: %d commands read, %d imported, %d skipped by redaction\n",
		*histfile, stats.Commands, stats.Imported, stats.Redacted)
	return nil
}
