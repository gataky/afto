// Package config loads and hot-reloads aftod's TOML configuration.
//
// Philosophy (plans/phase-1.md §8): every key is optional and a missing
// file is the normal case — the daemon must run usefully with zero setup.
// Reload can never take the daemon down: an invalid edit keeps the previous
// config and logs the problem, so a typo while the daemon runs degrades
// nothing.
//
// Hot reload matters here because of how afto is used: the daemon is spawned
// lazily by the shell and lives for days. "Edit config, restart daemon,
// restart every shell" (the IRIS reload dance) is exactly the workflow this
// design rejects — a config edit takes effect on the next keystroke.
package config

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/fsnotify/fsnotify"
)

// Config mirrors config.toml. See plans/phase-1.md §8 for the annotated
// example.
type Config struct {
	// LatencyBudgetMS is the per-request provider race deadline. The prompt
	// never waits longer than this for suggestions.
	LatencyBudgetMS int `toml:"latency_budget_ms"`
	// IdleShutdownMin: the daemon exits after this long with no client
	// connections (a shell holding its socket open counts as a connection,
	// so this only fires once every terminal is closed).
	IdleShutdownMin int       `toml:"idle_shutdown_min"`
	LogLevel        string    `toml:"log_level"`
	Providers       Providers `toml:"providers"`
	Redact          Redact    `toml:"redact"`
}

// Providers toggles built-in suggestion sources. Toggles are read at daemon
// start; unlike the other keys they do not hot-reload (the provider set is
// wired once — restarting the daemon is `kill` + next keystroke respawn).
type Providers struct {
	History  bool `toml:"history"`
	Frecency bool `toml:"frecency"`
}

// Redact extends (never replaces) the built-in secret patterns in
// store/redact.go.
type Redact struct {
	ExtraPatterns []string `toml:"extra_patterns"`
}

// Default is the zero-setup configuration.
func Default() Config {
	return Config{
		LatencyBudgetMS: 40,
		IdleShutdownMin: 30,
		LogLevel:        "info",
		Providers:       Providers{History: true, Frecency: true},
	}
}

// Load reads path over the defaults. A missing file returns pure defaults
// and no error; a present-but-invalid file returns defaults AND the error
// so callers can log it without dying.
func Load(path string) (Config, error) {
	c := Default()
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return Default(), err
	}
	// Nonsense values fall back rather than error: config can never make
	// the daemon unusable.
	if c.LatencyBudgetMS <= 0 {
		c.LatencyBudgetMS = Default().LatencyBudgetMS
	}
	if c.IdleShutdownMin <= 0 {
		c.IdleShutdownMin = Default().IdleShutdownMin
	}
	return c, nil
}

// Manager holds the live config and swaps it atomically on reload.
// Readers call Get() per use (e.g. the engine reads the budget on every
// request) — that is what makes reload take effect without coordination.
type Manager struct {
	path string
	log  *slog.Logger
	cur  atomic.Pointer[Config]

	mu       sync.Mutex
	onReload []func(Config)
}

func NewManager(path string, log *slog.Logger) *Manager {
	m := &Manager{path: path, log: log}
	c, err := Load(path)
	if err != nil {
		log.Warn("config invalid; using defaults", "path", path, "err", err)
	}
	m.cur.Store(&c)
	return m
}

func (m *Manager) Get() Config { return *m.cur.Load() }

// OnReload registers a callback invoked (on the watcher goroutine) after a
// successful reload — used to rebuild the redactor and retune log level.
func (m *Manager) OnReload(f func(Config)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onReload = append(m.onReload, f)
}

// Reload re-reads the file. On error the previous config stays live.
// Also called directly on SIGHUP (the fsnotify-less fallback).
func (m *Manager) Reload() {
	c, err := Load(m.path)
	if err != nil {
		m.log.Warn("config reload failed; keeping previous", "err", err)
		return
	}
	m.cur.Store(&c)
	m.log.Info("config reloaded", "path", m.path)
	m.mu.Lock()
	cbs := append([]func(Config){}, m.onReload...)
	m.mu.Unlock()
	for _, f := range cbs {
		f(c)
	}
}

// Watch reloads on file changes until ctx is done. It watches the parent
// directory rather than the file: the file may not exist yet, and editors
// typically replace files by rename (which unwatches a direct file watch).
// Events are debounced — a save can emit several within milliseconds.
func (m *Manager) Watch(ctx interface{ Done() <-chan struct{} }) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		m.log.Warn("config watch unavailable; SIGHUP still reloads", "err", err)
		return
	}
	defer w.Close()

	dir := filepath.Dir(m.path)
	_ = os.MkdirAll(dir, 0o755)
	if err := w.Add(dir); err != nil {
		m.log.Warn("config watch unavailable; SIGHUP still reloads", "dir", dir, "err", err)
		return
	}

	var timer *time.Timer
	debounced := make(chan struct{}, 1)
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-w.Events:
			if filepath.Clean(ev.Name) != filepath.Clean(m.path) {
				continue
			}
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(100*time.Millisecond, func() {
				select {
				case debounced <- struct{}{}:
				default:
				}
			})
		case <-debounced:
			m.Reload()
		case err := <-w.Errors:
			m.log.Debug("config watcher error", "err", err)
		}
	}
}
