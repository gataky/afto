package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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
	return startDaemonCfg(t, histfile, "")
}

// startDaemonCfg additionally writes a config.toml before the daemon starts,
// for tests that need configured plugins.
func startDaemonCfg(t *testing.T, histfile, configBody string) (string, context.CancelFunc) {
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
	if configBody != "" {
		if err := os.WriteFile(o.config, []byte(configBody), 0o600); err != nil {
			t.Fatal(err)
		}
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

// pluginScript writes an executable sh plugin and returns its path.
func pluginScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "plug.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDaemonServesPluginCandidates(t *testing.T) {
	plug := pluginScript(t, `
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed 's/.*"id":\([0-9]*\).*/\1/')
  printf '{"v":1,"id":%s,"candidates":[{"text":"deploy from-plugin","score":99}]}\n' "$id"
done
`)
	// A generous budget on purpose: the engine's deadline covers the whole
	// race, and a cold `sh` spawn plus a `sed` fork does not reliably fit in
	// the 40ms production default. Containment under a tight budget is what
	// TestDaemonSurvivesABrokenPlugin asserts; this test is about the
	// candidate reaching the client at all.
	socket, _ := startDaemonCfg(t, "", `
latency_budget_ms = 3000
[[plugin]]
name = "demo"
command = "`+plug+`"
timeout_ms = 2000
`)
	conn, r := connect(t, socket)

	// JSON format so the response carries provenance, not just text.
	request(t, conn, ipc.Request{V: 1, Type: "suggest", ID: 1, Buffer: "deploy", CWD: "/x"})
	var resp ipc.SuggestResponse
	if err := json.Unmarshal(readL(t, r), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Candidates) != 1 {
		t.Fatalf("plugin candidate missing: %+v", resp.Candidates)
	}
	if resp.Candidates[0].Text != "deploy from-plugin" {
		t.Fatalf("unexpected text: %+v", resp.Candidates[0])
	}
	if resp.Candidates[0].Source != "demo" {
		t.Fatalf("source = %q, want the configured plugin name", resp.Candidates[0].Source)
	}
}

func TestDaemonSurvivesABrokenPlugin(t *testing.T) {
	// One plugin hangs forever, one exits instantly. Neither may stop the
	// daemon from answering from its own history — this is the phase's core
	// promise, asserted end to end rather than only in the host's unit tests.
	hang := pluginScript(t, `while IFS= read -r line; do sleep 30; done`)
	dead := pluginScript(t, `exit 1`)
	socket, _ := startDaemonCfg(t, "", `
latency_budget_ms = 60
[[plugin]]
name = "hangs"
command = "`+hang+`"
[[plugin]]
name = "dies"
command = "`+dead+`"
`)
	conn, r := connect(t, socket)

	request(t, conn, ipc.Request{V: 1, Type: "record",
		Cmd: "make release", CWD: "/repo", Session: "s1", TS: time.Now().Unix()})

	for i := 0; i < 3; i++ {
		start := time.Now()
		request(t, conn, ipc.Request{V: 1, Type: "suggest", ID: int64(i + 1), Fmt: "tsv",
			Buffer: "make rel", CWD: "/repo", Session: "s1"})
		got := string(readL(t, r))
		elapsed := time.Since(start)

		if want := fmt.Sprintf("%d\tmake release", i+1); got != want {
			t.Fatalf("request %d: got %q, want %q", i+1, got, want)
		}
		// The hanging plugin must cost at most the budget, never its own
		// 30-second sleep.
		if elapsed > 2*time.Second {
			t.Fatalf("request %d took %v — a broken plugin delayed the answer", i+1, elapsed)
		}
	}
}

func TestDaemonSkipsDisabledAndIncompletePlugins(t *testing.T) {
	plug := pluginScript(t, `
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed 's/.*"id":\([0-9]*\).*/\1/')
  printf '{"v":1,"id":%s,"candidates":[{"text":"should not appear"}]}\n' "$id"
done
`)
	socket, _ := startDaemonCfg(t, "", `
[[plugin]]
name = "off"
command = "`+plug+`"
enabled = false
[[plugin]]
name = "nameless-command"
[[plugin]]
command = "`+plug+`"
`)
	conn, r := connect(t, socket)

	request(t, conn, ipc.Request{V: 1, Type: "suggest", ID: 1, Buffer: "should", CWD: "/x"})
	var resp ipc.SuggestResponse
	if err := json.Unmarshal(readL(t, r), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Candidates) != 0 {
		t.Fatalf("disabled/incomplete plugins produced candidates: %+v", resp.Candidates)
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
