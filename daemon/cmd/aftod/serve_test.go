package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gataky/afto/daemon/internal/ipc"
)

// startDaemon runs a real daemon in-process on temp paths and returns the
// socket path plus a stopper. This is the M5 integration test fixture:
// everything between the socket and SQLite is the production code path.
func startDaemon(t *testing.T, histfile string) (string, context.CancelFunc) {
	t.Helper()
	dir, err := os.MkdirTemp("", "afto-it") // NB: t.TempDir can exceed the 104-byte sun_path limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if histfile == "" {
		histfile = filepath.Join(dir, "no_histfile")
	}
	t.Setenv("HISTFILE", histfile)

	o := serveOpts{
		socket:  filepath.Join(dir, "afto.sock"),
		data:    filepath.Join(dir, "data"),
		config:  filepath.Join(dir, "config.toml"),
		logFile: filepath.Join(dir, "aftod.log"),
		version: "it-test",
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, o) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	})

	// Wait for the socket to accept.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", o.socket); err == nil {
			_ = conn.Close()
			return o.socket, cancel
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("daemon never came up")
	return "", nil
}

func connect(t *testing.T, socket string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, bufio.NewReader(conn)
}

func request(t *testing.T, conn net.Conn, req ipc.Request) {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonSuggestRecordRoundTrip(t *testing.T) {
	socket, _ := startDaemon(t, "")
	conn, r := connect(t, socket)

	// Ping proves liveness and version plumbing.
	request(t, conn, ipc.Request{V: 1, Type: "ping"})
	var pong ipc.PingResponse
	if err := json.Unmarshal(readL(t, r), &pong); err != nil {
		t.Fatal(err)
	}
	if !pong.OK || pong.Version != "it-test" {
		t.Fatalf("pong = %+v", pong)
	}

	// Record a command, then suggest from its prefix: full ingest→rank path.
	request(t, conn, ipc.Request{V: 1, Type: "record",
		Cmd: "git checkout feature/afto", CWD: "/repo", Session: "s1", TS: time.Now().Unix()})
	request(t, conn, ipc.Request{V: 1, Type: "suggest", ID: 5, Fmt: "tsv",
		Buffer: "git check", CWD: "/repo", Session: "s1"})

	line := string(readL(t, r))
	if line != "5\tgit checkout feature/afto" {
		t.Fatalf("got %q", line)
	}
}

func TestDaemonAutoImportsHistfileOnce(t *testing.T) {
	dir := t.TempDir()
	hist := filepath.Join(dir, "zsh_history")
	os.WriteFile(hist, []byte(": 1722000000:0;make deploy-prod\n"), 0o600)

	socket, _ := startDaemon(t, hist)
	conn, r := connect(t, socket)

	request(t, conn, ipc.Request{V: 1, Type: "suggest", ID: 1, Fmt: "tsv", Buffer: "make dep"})
	if got := string(readL(t, r)); got != "1\tmake deploy-prod" {
		t.Fatalf("imported history not suggested: %q", got)
	}
}

func TestDaemonSecondInstanceExitsQuietly(t *testing.T) {
	socket, _ := startDaemon(t, "")

	// Same paths → same lock: the second runServe must return nil quickly
	// without disturbing the first daemon.
	o := serveOpts{
		socket:  socket,
		data:    filepath.Join(filepath.Dir(socket), "data"),
		config:  filepath.Join(filepath.Dir(socket), "config.toml"),
		logFile: filepath.Join(filepath.Dir(socket), "aftod2.log"),
		version: "loser",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runServe(ctx, o); err != nil {
		t.Fatalf("second instance must exit nil, got %v", err)
	}

	// First daemon still answers.
	conn, r := connect(t, socket)
	request(t, conn, ipc.Request{V: 1, Type: "ping"})
	if !strings.Contains(string(readL(t, r)), `"ok":true`) {
		t.Fatal("original daemon disturbed by losing racer")
	}
}

func readL(t *testing.T, r *bufio.Reader) []byte {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return []byte(strings.TrimSuffix(line, "\n"))
}
