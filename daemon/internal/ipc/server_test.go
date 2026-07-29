package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gataky/afto/daemon/internal/provider"
)

type fakeHandler struct {
	recorded atomic.Int64
	lastCmd  atomic.Value
}

func (f *fakeHandler) Suggest(_ context.Context, q provider.Query) []provider.Candidate {
	if q.Buffer == "none" {
		return nil
	}
	return []provider.Candidate{
		{Text: q.Buffer + " --verbose", Score: 2, Source: "fake"},
		{Text: q.Buffer + " --help", Score: 1, Source: "fake"},
	}
}

func (f *fakeHandler) Record(r Request) {
	f.recorded.Add(1)
	f.lastCmd.Store(r.Cmd)
}

func (f *fakeHandler) Version() string { return "test-1" }

// startServer returns a connected client and the handler.
func startServer(t *testing.T) (net.Conn, *fakeHandler) {
	t.Helper()
	dir, err := os.MkdirTemp("", "afto-ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	h := &fakeHandler{}
	srv := NewServer(h, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, h
}

func send(t *testing.T, conn net.Conn, line string) {
	t.Helper()
	if _, err := conn.Write([]byte(line + "\n")); err != nil {
		t.Fatal(err)
	}
}

func readLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(line, "\n")
}

func TestSuggestTSV(t *testing.T) {
	conn, _ := startServer(t)
	r := bufio.NewReader(conn)

	send(t, conn, `{"v":1,"type":"suggest","id":7,"fmt":"tsv","buffer":"git ch"}`)
	if got, want := readLine(t, r), "7\tgit ch --verbose"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// empty result → id + empty suggestion, still framed
	send(t, conn, `{"v":1,"type":"suggest","id":8,"fmt":"tsv","buffer":"none"}`)
	if got, want := readLine(t, r), "8\t"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSuggestTSVLimit(t *testing.T) {
	conn, _ := startServer(t)
	r := bufio.NewReader(conn)

	// limit > 1 → multiple tab-separated candidates on one line
	send(t, conn, `{"v":1,"type":"suggest","id":11,"fmt":"tsv","buffer":"git ch","limit":4}`)
	if got, want := readLine(t, r), "11\tgit ch --verbose\tgit ch --help"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// limit larger than the engine cap is clamped, not an error
	send(t, conn, `{"v":1,"type":"suggest","id":12,"fmt":"tsv","buffer":"git ch","limit":999}`)
	if got, want := readLine(t, r), "12\tgit ch --verbose\tgit ch --help"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// limit with zero candidates keeps the Phase 1 empty shape
	send(t, conn, `{"v":1,"type":"suggest","id":13,"fmt":"tsv","buffer":"none","limit":4}`)
	if got, want := readLine(t, r), "13\t"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSuggestJSONLimit(t *testing.T) {
	conn, _ := startServer(t)
	r := bufio.NewReader(conn)

	send(t, conn, `{"v":1,"type":"suggest","id":14,"buffer":"git ch","limit":1}`)
	var resp SuggestResponse
	if err := json.Unmarshal([]byte(readLine(t, r)), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != 14 || len(resp.Candidates) != 1 || resp.Candidates[0].Text != "git ch --verbose" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestSuggestJSON(t *testing.T) {
	conn, _ := startServer(t)
	r := bufio.NewReader(conn)

	send(t, conn, `{"v":1,"type":"suggest","id":9,"buffer":"git ch"}`)
	var resp SuggestResponse
	if err := json.Unmarshal([]byte(readLine(t, r)), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != 9 || len(resp.Candidates) != 2 || resp.Candidates[0].Text != "git ch --verbose" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	// empty candidates must serialize as [], not null
	send(t, conn, `{"v":1,"type":"suggest","id":10,"buffer":"none"}`)
	raw := readLine(t, r)
	if !strings.Contains(raw, `"candidates":[]`) {
		t.Fatalf("want empty array, got %s", raw)
	}
}

func TestMalformedAndUnknownKeepConnection(t *testing.T) {
	conn, h := startServer(t)
	r := bufio.NewReader(conn)

	send(t, conn, `{not json`)
	send(t, conn, `{"v":1,"type":"mystery"}`)
	send(t, conn, `{"v":1,"type":"record","cmd":"ls -la","exit":0,"ts":1722180042}`)
	send(t, conn, `{"v":1,"type":"ping"}`)

	var pong PingResponse
	if err := json.Unmarshal([]byte(readLine(t, r)), &pong); err != nil {
		t.Fatal(err)
	}
	if !pong.OK || pong.Version != "test-1" {
		t.Fatalf("unexpected pong: %+v", pong)
	}
	deadline := time.Now().Add(time.Second)
	for h.recorded.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h.recorded.Load() != 1 || h.lastCmd.Load() != "ls -la" {
		t.Fatalf("record not handled: n=%d cmd=%v", h.recorded.Load(), h.lastCmd.Load())
	}
}

func TestConcurrentConnections(t *testing.T) {
	conn1, _ := startServer(t)
	// second client on the same socket
	sock := conn1.RemoteAddr().String()
	conn2, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()

	r1, r2 := bufio.NewReader(conn1), bufio.NewReader(conn2)
	for i := 0; i < 50; i++ {
		send(t, conn1, fmt.Sprintf(`{"v":1,"type":"suggest","id":%d,"fmt":"tsv","buffer":"a"}`, i))
		send(t, conn2, fmt.Sprintf(`{"v":1,"type":"suggest","id":%d,"fmt":"tsv","buffer":"b"}`, i))
		if got, want := readLine(t, r1), fmt.Sprintf("%d\ta --verbose", i); got != want {
			t.Fatalf("conn1: got %q want %q", got, want)
		}
		if got, want := readLine(t, r2), fmt.Sprintf("%d\tb --verbose", i); got != want {
			t.Fatalf("conn2: got %q want %q", got, want)
		}
	}
}
