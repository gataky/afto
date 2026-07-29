// Package plugin runs external suggestion sources as subprocesses that speak
// the same JSON-lines protocol as the shell client (DESIGN.md §3.2,
// docs/plugins.md).
//
// # Why subprocesses
//
// Go's stdlib `plugin` package is version-locked and brittle; gRPC plugin
// frameworks are heavy for a line of text. A subprocess reading JSON lines
// from stdin can be fifteen lines of shell, and it makes the shell client
// "plugin #0" — one protocol to learn, one to document.
//
// # What this package is actually about
//
// The protocol is trivial. Everything here exists to answer a harder
// question: how do you let an arbitrary user-supplied process into a
// latency-critical path without it ever being able to hurt the prompt?
//
// The answer is three independent layers, because processes fail in three
// different ways:
//
//   - Slow. Bounded per request by Timeout, on top of the engine's own race
//     deadline. A late answer is drained and discarded by id, so it can
//     never be mistaken for the answer to the NEXT request.
//   - Dead. A crashed process is restarted on next use with exponential
//     backoff, so a plugin that dies instantly cannot become a fork bomb.
//   - Persistently broken. A breaker counts consecutive failures and, past a
//     threshold, benches the plugin entirely for a cooldown — no process, no
//     syscall, no latency. A half-open probe lets it come back on its own.
//
// A Host is registered with the provider engine as an ordinary Provider, so
// the existing race/budget/merge machinery applies to plugins unchanged. The
// engine's deadline is the outer bound; everything here is the inner one.
package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/gataky/afto/daemon/internal/provider"
)

// Tunables. In one place, documented as adjustable; tests pin behavior
// rather than these numbers.
const (
	// maxFailures is how many consecutive failures bench a plugin. Three
	// tolerates a transient hiccup without letting a broken plugin cost the
	// latency budget on every keystroke.
	maxFailures = 3

	// cooldown is how long a benched plugin stays benched before a probe.
	cooldown = 30 * time.Second

	// Restart backoff bounds, applied after a process dies.
	minBackoff = 1 * time.Second
	maxBackoff = 60 * time.Second

	// MaxLine bounds one response line from a plugin.
	MaxLine = 64 << 10

	// maxTextLen bounds a single candidate's text. Longer is truncated:
	// a chatty plugin should degrade, not disappear.
	maxTextLen = 4 << 10
)

// errExited means the subprocess closed its stdout — it died. Distinguished
// from a timeout because the recovery differs: a dead plugin needs a restart
// (with backoff), a slow one just needs to be left alone until the breaker
// decides it is hopeless.
var errExited = errors.New("plugin exited")

// Config describes one configured plugin.
type Config struct {
	Name    string
	Command string
	Args    []string
	Timeout time.Duration
}

// request is what the daemon writes to a plugin's stdin.
type request struct {
	V        int      `json:"v"`
	ID       int64    `json:"id"`
	Buffer   string   `json:"buffer"`
	Cursor   int      `json:"cursor"`
	CWD      string   `json:"cwd,omitempty"`
	LastExit int      `json:"last_exit,omitempty"`
	Session  string   `json:"session,omitempty"`
	Recent   []string `json:"recent,omitempty"`
}

// response is what a plugin writes back.
type response struct {
	V          int         `json:"v"`
	ID         int64       `json:"id"`
	Candidates []candidate `json:"candidates"`
}

type candidate struct {
	Text  string  `json:"text"`
	Score float64 `json:"score"`
	Note  string  `json:"note"`
}

// Host owns one plugin subprocess and implements provider.Provider.
//
// Concurrency: requests to a single plugin are serialized. A line-oriented
// script cannot be assumed to handle interleaved requests, and serializing
// costs nothing real — each request is bounded by Timeout, and the engine
// abandons anything slower than its budget anyway. A caller that cannot get
// the plugin before its own deadline gives up rather than queueing.
type Host struct {
	cfg Config
	log *slog.Logger

	mu      sync.Mutex // held for the whole request/response exchange
	proc    *proc
	nextID  int64
	backoff time.Duration
	nextTry time.Time // earliest restart after a death

	failures  int
	benchedTo time.Time // breaker open until this instant
}

func New(cfg Config, log *slog.Logger) *Host {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 40 * time.Millisecond
	}
	return &Host{cfg: cfg, log: log.With("plugin", cfg.Name)}
}

func (h *Host) Name() string { return h.cfg.Name }

// Suggest asks the plugin. It returns (nil, nil) for every failure mode:
// an error here would only be logged by the engine anyway, and "no
// candidates" is the honest representation of "this source had nothing for
// you", whatever the reason.
func (h *Host) Suggest(ctx context.Context, q provider.Query) ([]provider.Candidate, error) {
	// Serialize, but never wait past the caller's deadline.
	if !h.lock(ctx) {
		return nil, nil
	}
	defer h.mu.Unlock()

	if time.Now().Before(h.benchedTo) {
		return nil, nil // breaker open: not even a syscall
	}

	p, err := h.ensureProc()
	if err != nil {
		h.fail("start", err)
		return nil, nil
	}

	h.nextID++
	id := h.nextID
	req := request{
		V: 1, ID: id, Buffer: q.Buffer, Cursor: q.Cursor, CWD: q.CWD,
		LastExit: q.LastExit, Session: q.Session, Recent: q.Recent,
	}
	line, err := json.Marshal(req)
	if err != nil {
		h.fail("marshal", err)
		return nil, nil
	}
	if _, err := p.stdin.Write(append(line, '\n')); err != nil {
		h.kill()
		h.fail("write", err)
		return nil, nil
	}

	resp, err := h.await(ctx, p, id)
	if err != nil {
		// A dead process must be cleared here, not left for the next
		// request to discover by writing into a broken pipe: kill() is what
		// engages the restart backoff, and without it a plugin that exits on
		// every request would be respawned on every keystroke.
		if errors.Is(err, errExited) {
			h.kill()
		}
		h.fail("read", err)
		return nil, nil
	}

	h.succeed()
	return h.candidates(resp), nil
}

// lock acquires the request mutex unless ctx expires first.
func (h *Host) lock(ctx context.Context) bool {
	done := make(chan struct{})
	go func() { h.mu.Lock(); close(done) }()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		// The goroutine still holds/acquires the lock; release it when it
		// lands so the mutex is not leaked.
		go func() { <-done; h.mu.Unlock() }()
		return false
	}
}

// await waits for the response with the matching id, discarding anything
// else. Draining by id is what keeps a late answer from being attributed to
// the request that follows it.
func (h *Host) await(ctx context.Context, p *proc, id int64) (*response, error) {
	timer := time.NewTimer(h.cfg.Timeout)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-p.lines:
			if !ok {
				return nil, errExited
			}
			var r response
			if err := json.Unmarshal(line, &r); err != nil {
				return nil, fmt.Errorf("malformed response: %w", err)
			}
			if r.ID != id {
				h.log.Debug("dropping stale plugin response", "want", id, "got", r.ID)
				continue
			}
			return &r, nil
		case <-timer.C:
			return nil, fmt.Errorf("timeout after %s", h.cfg.Timeout)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// candidates converts, caps, and stamps provenance. Source is assigned here,
// never taken from the plugin: a plugin must not be able to claim it is
// "history".
func (h *Host) candidates(r *response) []provider.Candidate {
	out := make([]provider.Candidate, 0, len(r.Candidates))
	for _, c := range r.Candidates {
		if c.Text == "" {
			continue
		}
		if len(c.Text) > maxTextLen {
			c.Text = c.Text[:maxTextLen]
		}
		if len(c.Note) > maxTextLen {
			c.Note = c.Note[:maxTextLen]
		}
		score := c.Score
		if score == 0 {
			score = 1.0
		}
		out = append(out, provider.Candidate{
			Text: c.Text, Score: score, Source: h.cfg.Name, Note: c.Note,
		})
		if len(out) == provider.CandidateLimit {
			break
		}
	}
	return out
}

// --- breaker ------------------------------------------------------------

func (h *Host) succeed() {
	h.failures = 0
	h.backoff = 0
}

func (h *Host) fail(stage string, err error) {
	h.failures++
	h.log.Debug("plugin failure", "stage", stage, "err", err, "consecutive", h.failures)
	if h.failures >= maxFailures {
		h.benchedTo = time.Now().Add(cooldown)
		// Killing on bench is deliberate: respawning on the probe is the
		// only recovery path that fixes a process wedged mid-line.
		h.kill()
		h.failures = 0 // the probe after cooldown starts with a clean count
		h.log.Warn("plugin benched", "cooldown", cooldown)
	}
}

// --- process lifecycle --------------------------------------------------

type proc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan []byte
	closed sync.Once
}

func (h *Host) ensureProc() (*proc, error) {
	if h.proc != nil {
		return h.proc, nil
	}
	if now := time.Now(); now.Before(h.nextTry) {
		return nil, fmt.Errorf("restart backoff for another %s", h.nextTry.Sub(now).Round(time.Millisecond))
	}

	cmd := exec.Command(h.cfg.Command, h.cfg.Args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		h.penalize()
		return nil, err
	}

	p := &proc{cmd: cmd, stdin: stdin, lines: make(chan []byte, 4)}
	go h.readLoop(p, stdout)
	go h.drainStderr(stderr)
	h.proc = p
	h.log.Debug("plugin started", "command", h.cfg.Command, "pid", cmd.Process.Pid)
	return p, nil
}

// readLoop owns stdout. It closes p.lines on EOF, which is how a waiting
// request learns the process died rather than waiting out its timeout.
func (h *Host) readLoop(p *proc, stdout io.ReadCloser) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 4096), MaxLine)
	for sc.Scan() {
		// Scanner reuses its buffer; the value must be copied before it
		// crosses the channel.
		line := make([]byte, len(sc.Bytes()))
		copy(line, sc.Bytes())
		select {
		case p.lines <- line:
		default:
			// Nobody is waiting (an abandoned request) and the buffer is
			// full: drop rather than block the reader forever.
			h.log.Debug("dropping unread plugin line")
		}
	}
	if err := sc.Err(); err != nil {
		h.log.Debug("plugin read ended", "err", err)
	}
	p.closed.Do(func() { close(p.lines) })
}

// drainStderr keeps a plugin's diagnostics out of the user's terminal and in
// the daemon log, where they belong.
func (h *Host) drainStderr(stderr io.ReadCloser) {
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 4096), MaxLine)
	for sc.Scan() {
		h.log.Debug("plugin stderr", "line", sc.Text())
	}
}

// kill stops the process and schedules the next restart with backoff.
func (h *Host) kill() {
	if h.proc == nil {
		return
	}
	_ = h.proc.stdin.Close()
	if h.proc.cmd.Process != nil {
		_ = h.proc.cmd.Process.Kill()
	}
	go func(c *exec.Cmd) { _ = c.Wait() }(h.proc.cmd) // reap without blocking
	h.proc = nil
	h.penalize()
}

// penalize advances the restart backoff. Reset by a successful exchange, so
// a plugin that works after one bad start is not punished for it.
func (h *Host) penalize() {
	if h.backoff == 0 {
		h.backoff = minBackoff
	} else if h.backoff < maxBackoff {
		h.backoff *= 2
		if h.backoff > maxBackoff {
			h.backoff = maxBackoff
		}
	}
	h.nextTry = time.Now().Add(h.backoff)
}

// Close stops the subprocess. Called on daemon shutdown.
func (h *Host) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.proc == nil {
		return
	}
	_ = h.proc.stdin.Close()
	if h.proc.cmd.Process != nil {
		_ = h.proc.cmd.Process.Kill()
	}
	go func(c *exec.Cmd) { _ = c.Wait() }(h.proc.cmd)
	h.proc = nil
}
