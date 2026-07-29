package plugin

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gataky/afto/daemon/internal/provider"
)

// Tests drive REAL subprocesses. A fake in-process "plugin" would test the
// codec and nothing else — the behaviors that matter here (a process that
// dies mid-line, one that never answers, one that answers late) only exist
// when there is a process.

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// script writes an executable sh script and returns its path.
func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "plugin.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func host(t *testing.T, path string, timeout time.Duration) *Host {
	t.Helper()
	h := New(Config{Name: "test-plugin", Command: path, Timeout: timeout}, testLog())
	t.Cleanup(h.Close)
	return h
}

// echoPlugin answers every request with one candidate built from the buffer.
// It is deliberately the smallest thing that could work — the same shape the
// docs promise a plugin can be.
const echoPlugin = `
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed 's/.*"id":\([0-9]*\).*/\1/')
  printf '{"v":1,"id":%s,"candidates":[{"text":"from-plugin","score":2.5,"note":"n"}]}\n' "$id"
done
`

func TestSuggestRoundTrip(t *testing.T) {
	h := host(t, script(t, echoPlugin), time.Second)

	got, err := h.Suggest(context.Background(), provider.Query{Buffer: "fro"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "from-plugin" || got[0].Note != "n" {
		t.Fatalf("unexpected candidates: %+v", got)
	}
	if got[0].Score != 2.5 {
		t.Fatalf("score not carried: %+v", got[0])
	}
	// Provenance is the host's to assign.
	if got[0].Source != "test-plugin" {
		t.Fatalf("source = %q, want the configured plugin name", got[0].Source)
	}

	// The process is long-lived: a second request reuses it.
	pid := h.proc.cmd.Process.Pid
	if _, err := h.Suggest(context.Background(), provider.Query{Buffer: "x"}); err != nil {
		t.Fatal(err)
	}
	if h.proc == nil || h.proc.cmd.Process.Pid != pid {
		t.Fatal("plugin was respawned between requests")
	}
}

func TestSourceCannotBeSpoofed(t *testing.T) {
	h := host(t, script(t, `
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed 's/.*"id":\([0-9]*\).*/\1/')
  printf '{"v":1,"id":%s,"candidates":[{"text":"t","source":"history"}]}\n' "$id"
done
`), time.Second)

	got, _ := h.Suggest(context.Background(), provider.Query{Buffer: "t"})
	if len(got) != 1 || got[0].Source != "test-plugin" {
		t.Fatalf("plugin impersonated a built-in: %+v", got)
	}
	// An omitted score defaults rather than sorting the candidate last.
	if got[0].Score != 1.0 {
		t.Fatalf("default score = %v, want 1", got[0].Score)
	}
}

func TestSlowPluginTimesOutWithoutBlocking(t *testing.T) {
	h := host(t, script(t, `
while IFS= read -r line; do
  sleep 5
done
`), 50*time.Millisecond)

	start := time.Now()
	got, err := h.Suggest(context.Background(), provider.Query{Buffer: "x"})
	elapsed := time.Since(start)
	if err != nil || got != nil {
		t.Fatalf("want silence, got %+v err=%v", got, err)
	}
	if elapsed > time.Second {
		t.Fatalf("Suggest waited %v on a hanging plugin", elapsed)
	}
}

func TestLateAnswerIsNotAttributedToTheNextRequest(t *testing.T) {
	// Answers request 1 too late to be used, then answers request 2 promptly.
	// The stale line must be discarded by id, not handed to request 2.
	h := host(t, script(t, `
first=1
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed 's/.*"id":\([0-9]*\).*/\1/')
  if [ "$first" = 1 ]; then
    first=0
    sleep 0.3
    printf '{"v":1,"id":%s,"candidates":[{"text":"STALE"}]}\n' "$id"
  else
    printf '{"v":1,"id":%s,"candidates":[{"text":"FRESH"}]}\n' "$id"
  fi
done
`), 80*time.Millisecond)

	if got, _ := h.Suggest(context.Background(), provider.Query{Buffer: "a"}); got != nil {
		t.Fatalf("first request should have timed out, got %+v", got)
	}
	time.Sleep(400 * time.Millisecond) // let the stale answer arrive

	// Generous timeout for the second request: this test is about id
	// correlation, so scheduling noise must not be able to fail it.
	h.mu.Lock()
	h.cfg.Timeout = 2 * time.Second
	h.mu.Unlock()

	got, _ := h.Suggest(context.Background(), provider.Query{Buffer: "b"})
	if len(got) != 1 || got[0].Text != "FRESH" {
		t.Fatalf("stale answer leaked into the next request: %+v", got)
	}
}

func TestCallerDeadlineIsRespected(t *testing.T) {
	h := host(t, script(t, `while IFS= read -r line; do sleep 5; done`), 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if got, _ := h.Suggest(ctx, provider.Query{Buffer: "x"}); got != nil {
		t.Fatalf("got %+v", got)
	}
	// The engine's budget must win over a generous per-plugin timeout.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("ignored the caller's deadline: waited %v", elapsed)
	}
}

func TestMalformedAndMismatchedResponses(t *testing.T) {
	h := host(t, script(t, `
while IFS= read -r line; do
  printf 'not json at all\n'
done
`), 200*time.Millisecond)

	if got, err := h.Suggest(context.Background(), provider.Query{Buffer: "x"}); got != nil || err != nil {
		t.Fatalf("malformed output must yield silence, got %+v err=%v", got, err)
	}
	if h.failures == 0 {
		t.Fatal("malformed response should count as a failure")
	}
}

func TestCrashedPluginIsRestartedNotResurrectedInstantly(t *testing.T) {
	// Exits as soon as it reads anything.
	h := host(t, script(t, `read -r line; exit 1`), time.Second)

	if got, _ := h.Suggest(context.Background(), provider.Query{Buffer: "x"}); got != nil {
		t.Fatalf("got %+v", got)
	}
	// A dead process must not be respawned on the very next keystroke: that
	// is how a broken plugin becomes a fork bomb.
	if h.backoff < minBackoff {
		t.Fatalf("no restart backoff applied: %v", h.backoff)
	}
	before := h.backoff
	if got, _ := h.Suggest(context.Background(), provider.Query{Buffer: "y"}); got != nil {
		t.Fatalf("got %+v", got)
	}
	if h.proc != nil {
		t.Fatal("respawned inside the backoff window")
	}
	if h.backoff != before {
		t.Fatalf("backoff advanced without a spawn attempt: %v → %v", before, h.backoff)
	}
}

func TestBreakerBenchesAndRecovers(t *testing.T) {
	h := host(t, script(t, `while IFS= read -r line; do printf 'garbage\n'; done`), 200*time.Millisecond)

	for i := 0; i < maxFailures; i++ {
		if _, err := h.Suggest(context.Background(), provider.Query{Buffer: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if h.benchedTo.IsZero() || time.Now().After(h.benchedTo) {
		t.Fatal("breaker did not open after repeated failures")
	}
	if h.proc != nil {
		t.Fatal("benching should stop the process")
	}

	// While benched, a request must cost nothing at all.
	start := time.Now()
	if got, _ := h.Suggest(context.Background(), provider.Query{Buffer: "x"}); got != nil {
		t.Fatalf("benched plugin answered: %+v", got)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Fatalf("benched plugin still cost %v", elapsed)
	}

	// After the cooldown a probe is allowed through, and a plugin that has
	// started behaving closes the breaker again.
	h.mu.Lock()
	h.benchedTo = time.Now().Add(-time.Second)
	h.nextTry = time.Time{}
	h.cfg.Command = script(t, echoPlugin)
	h.mu.Unlock()

	got, _ := h.Suggest(context.Background(), provider.Query{Buffer: "x"})
	if len(got) != 1 || got[0].Text != "from-plugin" {
		t.Fatalf("probe after cooldown did not recover: %+v", got)
	}
	if h.failures != 0 {
		t.Fatalf("failure count not reset after success: %d", h.failures)
	}
}

func TestOversizedLineIsDroppedAsAFailure(t *testing.T) {
	// A line past MaxLine cannot be truncated into valid JSON, so unlike the
	// per-candidate caps it is a failure, not a degradation. Pinning the
	// distinction because it is the one place "cap" means "discard".
	h := host(t, script(t, `
long=$(printf 'x%.0s' $(seq 1 70000))
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed 's/.*"id":\([0-9]*\).*/\1/')
  printf '{"v":1,"id":%s,"candidates":[{"text":"%s"}]}\n' "$id" "$long"
done
`), 300*time.Millisecond)

	got, err := h.Suggest(context.Background(), provider.Query{Buffer: "x"})
	if got != nil || err != nil {
		t.Fatalf("want silence, got %+v err=%v", got, err)
	}
	if h.failures == 0 {
		t.Fatal("an unreadable response should count as a failure")
	}
}

func TestManyCandidatesAndLongTextAreCapped(t *testing.T) {
	// 12 candidates of 4500 bytes each: both the count and each text are
	// capped, and the plugin still counts as working.
	h := host(t, script(t, `
long=$(printf 'x%.0s' $(seq 1 4500))
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed 's/.*"id":\([0-9]*\).*/\1/')
  printf '{"v":1,"id":%s,"candidates":[' "$id"
  i=1
  while [ $i -le 12 ]; do
    [ $i -gt 1 ] && printf ','
    printf '{"text":"%s"}' "$long"
    i=$((i+1))
  done
  printf ']}\n'
done
`), 2*time.Second)

	got, err := h.Suggest(context.Background(), provider.Query{Buffer: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != provider.CandidateLimit {
		t.Fatalf("candidate count not capped: %d", len(got))
	}
	if len(got[0].Text) != maxTextLen {
		t.Fatalf("text not capped: %d bytes", len(got[0].Text))
	}
	if h.failures != 0 {
		t.Fatal("a chatty plugin should degrade, not be treated as broken")
	}
}

func TestStartWarmsTheProcess(t *testing.T) {
	h := host(t, script(t, echoPlugin), time.Second)
	h.Start()
	if h.proc == nil {
		t.Fatal("Start did not spawn the plugin")
	}
	pid := h.proc.cmd.Process.Pid

	// The warmed process is the one that serves the first request — that is
	// the whole point: no cold exec inside the latency budget.
	if got, _ := h.Suggest(context.Background(), provider.Query{Buffer: "x"}); len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if h.proc.cmd.Process.Pid != pid {
		t.Fatal("first request respawned instead of reusing the warmed process")
	}
}

func TestStartOnBrokenPluginIsHarmless(t *testing.T) {
	h := host(t, filepath.Join(t.TempDir(), "missing"), time.Second)
	h.Start() // must not panic, must not block
	if h.proc != nil {
		t.Fatal("a missing binary must not leave a process behind")
	}
}

func TestMissingBinaryIsSilentAndBackedOff(t *testing.T) {
	h := host(t, filepath.Join(t.TempDir(), "does-not-exist"), time.Second)

	got, err := h.Suggest(context.Background(), provider.Query{Buffer: "x"})
	if got != nil || err != nil {
		t.Fatalf("want silence, got %+v err=%v", got, err)
	}
	if h.backoff < minBackoff {
		t.Fatal("a missing binary should engage restart backoff")
	}
}

func TestRequestCarriesTheQueryContext(t *testing.T) {
	// Echo the request back as a candidate so the test can inspect what the
	// plugin actually received.
	h := host(t, script(t, `
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed 's/.*"id":\([0-9]*\).*/\1/')
  esc=$(printf '%s' "$line" | sed 's/"/\x27/g')
  printf '{"v":1,"id":%s,"candidates":[{"text":"%s"}]}\n' "$id" "$esc"
done
`), 2*time.Second)

	got, err := h.Suggest(context.Background(), provider.Query{
		Buffer: "kubectl ", Cursor: 8, CWD: "/w/proj", Session: "s1",
		LastExit: 3, Recent: []string{"git status"},
	})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v err=%v", got, err)
	}
	seen := got[0].Text
	for _, want := range []string{"kubectl ", "/w/proj", "s1", "git status", "'cursor':8", "'last_exit':3", "'v':1"} {
		if !strings.Contains(seen, want) {
			t.Errorf("request missing %q; plugin saw: %s", want, seen)
		}
	}
}
