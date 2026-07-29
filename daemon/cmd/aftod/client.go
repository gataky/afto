package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/gataky/afto/daemon/internal/ipc"
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
	buffer := fs.String("buffer", "", "command-line prefix to suggest for (required)")
	cwd := fs.String("cwd", "", "working directory context")
	limit := fs.Int("limit", 0, "max candidates (0 = all, capped at 10)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *buffer == "" {
		return fmt.Errorf("query: --buffer is required")
	}
	if *cwd == "" {
		*cwd, _ = os.Getwd()
	}
	line, err := roundTrip(*socket, ipc.Request{
		V: ipc.V, Type: ipc.TypeSuggest, ID: 1,
		Buffer: *buffer, Cursor: len(*buffer), CWD: *cwd, Limit: *limit,
	})
	if err != nil {
		return err
	}
	fmt.Print(line)
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
