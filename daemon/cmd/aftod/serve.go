package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gataky/afto/daemon/internal/config"
	"github.com/gataky/afto/daemon/internal/ipc"
	"github.com/gataky/afto/daemon/internal/plugin"
	"github.com/gataky/afto/daemon/internal/project"
	"github.com/gataky/afto/daemon/internal/provider"
	"github.com/gataky/afto/daemon/internal/store"
)

// serveOpts parameterizes runServe so the integration test can run a real
// daemon in-process on temp paths.
type serveOpts struct {
	socket  string
	data    string
	config  string
	logFile string // "" → stderr (foreground/testing)
	version string
}

func defaultServeOpts() serveOpts {
	return serveOpts{
		socket:  socketPath(),
		data:    dataDir(),
		config:  configPath(),
		logFile: logPath(),
		version: version,
	}
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	daemonize := fs.Bool("daemonize", false, "detach and log to file")
	foreground := fs.Bool("foreground", false, "log to stderr instead of the log file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *daemonize {
		return respawnDetached()
	}
	o := defaultServeOpts()
	if *foreground {
		o.logFile = ""
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runServe(ctx, o)
}

// respawnDetached re-executes "aftod serve" in its own session with stdio
// pointed at the log file, then returns so the parent exits. This is the
// no-launchd daemonization the zsh client relies on: it can run
// `aftod serve --daemonize` and get its prompt back immediately.
func respawnDetached() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := openLogFile(logPath())
	if err != nil {
		return err
	}
	defer logf.Close()

	cmd := exec.Command(exe, "serve")
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

// openLogFile opens for append, rotating once to .old past 10 MB — enough
// to keep debug sessions from growing unbounded without a rotation dep.
func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > 10<<20 {
		_ = os.Rename(path, path+".old")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

// runServe is the daemon main loop. Lifecycle (plans/phase-1.md §8):
// acquire the single-instance lock (losing silently: the client spawns
// speculatively, so two racing spawns are normal, not an error), replace
// any stale socket, auto-import history once, serve until signalled or
// idle, clean up.
func runServe(ctx context.Context, o serveOpts) error {
	level := new(slog.LevelVar) // defaults to Info; retuned from config below
	log, closeLog, err := newLogger(o.logFile, level)
	if err != nil {
		return err
	}
	defer closeLog()

	if err := os.MkdirAll(filepath.Dir(o.socket), 0o700); err != nil {
		return err
	}
	unlock, ok, err := acquireLock(o.socket + ".lock")
	if err != nil {
		return err
	}
	if !ok {
		log.Debug("another aftod holds the lock; exiting quietly")
		return nil
	}
	defer unlock()

	// We hold the lock, so an existing socket file is a leftover from a
	// dead daemon — remove it or Listen fails with "address already in use".
	_ = os.Remove(o.socket)

	mgr := config.NewManager(o.config, log)
	setLevel(level, mgr.Get().LogLevel)

	st, err := store.Open(o.data)
	if err != nil {
		return err
	}
	defer st.Close()

	applyRedact := func(c config.Config) {
		r, err := store.NewRedactor(c.Redact.ExtraPatterns)
		if err != nil {
			log.Warn("bad extra redact pattern; keeping previous rules", "err", err)
			return
		}
		st.SetRedactor(r)
	}
	applyRedact(mgr.Get())
	mgr.OnReload(func(c config.Config) {
		setLevel(level, c.LogLevel)
		applyRedact(c)
	})

	autoImport(ctx, st, log)

	var providers []provider.Provider
	pc := mgr.Get().Providers
	if pc.History {
		providers = append(providers, provider.NewHistory(st))
	}
	if pc.Frecency {
		providers = append(providers, provider.NewFrecency(st))
	}
	if pc.Transition {
		providers = append(providers, provider.NewTransition(st))
	}
	// External plugins are providers like any other — the race, the budget
	// and the merge already know how to contain a slow source, so a plugin
	// needs no special case here. Wired at start, like the built-in toggles.
	var hosts []*plugin.Host
	for _, pcfg := range mgr.Get().Plugins {
		if !pcfg.On() || pcfg.Name == "" || pcfg.Command == "" {
			continue
		}
		timeout := time.Duration(pcfg.TimeoutMS) * time.Millisecond
		if timeout <= 0 {
			timeout = time.Duration(mgr.Get().LatencyBudgetMS) * time.Millisecond
		}
		h := plugin.New(plugin.Config{
			Name: pcfg.Name, Command: pcfg.Command, Args: pcfg.Args, Timeout: timeout,
		}, log)
		hosts = append(hosts, h)
		providers = append(providers, h)
		// Warm up off the serving path: spawning here would delay the socket
		// coming up, and the first suggestion should not have to race a cold
		// exec against the latency budget.
		go h.Start()
		log.Info("plugin configured", "name", pcfg.Name, "command", pcfg.Command, "timeout", timeout)
	}
	defer func() {
		for _, h := range hosts {
			h.Close()
		}
	}()
	budget := func() time.Duration {
		return time.Duration(mgr.Get().LatencyBudgetMS) * time.Millisecond
	}
	engine := provider.NewEngine(log, budget, providers...)
	var aliases *provider.AliasNote
	if pc.AliasNote {
		aliases = provider.NewAliasNote()
		engine.Use(aliases)
	}

	l, err := net.Listen("unix", o.socket)
	if err != nil {
		return err
	}
	srv := ipc.NewServer(&core{
		engine:   engine,
		st:       st,
		projects: project.New(mgr.Get().Project.Markers),
		aliases:  aliases,
		log:      log,
		version:  o.version,
	}, log)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go mgr.Watch(ctx)
	go reloadOnSIGHUP(ctx, mgr)
	go idleLoop(ctx, cancel, srv, mgr, log)
	go func() {
		<-ctx.Done()
		_ = l.Close() // unblocks Serve
	}()

	log.Info("aftod serving", "socket", o.socket, "data", o.data, "version", o.version)
	err = srv.Serve(l)
	_ = os.Remove(o.socket)
	log.Info("aftod stopped")
	return err
}

// core implements ipc.Handler: the seam where transport meets providers and
// storage.
type core struct {
	engine   *provider.Engine
	st       *store.Store
	projects *project.Resolver
	aliases  *provider.AliasNote // nil when the alias_note decorator is off
	log      *slog.Logger
	version  string
}

// Suggest resolves the query's project before ranking. Clients send only a
// cwd — "which project is that in" is a filesystem question, so the daemon
// answers it (cached; see the project package) rather than making every
// client implement the walk.
func (c *core) Suggest(ctx context.Context, q provider.Query) []provider.Candidate {
	q.ProjectRoot = c.projects.Root(q.CWD)
	return c.engine.Suggest(ctx, q)
}

// Record ingests synchronously: two SQLite writes, well under a millisecond,
// and record messages have no response — the only thing waiting is the same
// shell's next message on this connection, which is fine at human speed.
func (c *core) Record(r ipc.Request) {
	ts := r.TS
	if ts == 0 {
		ts = time.Now().Unix()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	ok, err := c.st.Ingest(ctx, store.Event{
		Cmd: r.Cmd, CWD: r.CWD, Session: r.Session, Exit: r.Exit, TS: ts,
	})
	if err != nil {
		c.log.Warn("ingest failed", "err", err)
		return
	}
	if !ok {
		c.log.Debug("command skipped by redaction")
	}
}

// SetAliases stores a shell's alias table for annotation. Aliases are
// session-scoped in-memory state: they are user configuration rather than
// command history, so nothing here touches the store or the log's info
// level (an alias body can be as private as a command line).
func (c *core) SetAliases(r ipc.Request) {
	if c.aliases == nil {
		return
	}
	c.aliases.Set(r.Session, r.Map)
	c.log.Debug("alias table updated", "session", r.Session, "entries", len(r.Map))
}

func (c *core) Version() string { return c.version }

// autoImport bootstraps the store from the user's existing HISTFILE exactly
// once (plans/phase-1.md §6) so suggestions are useful in minute one.
func autoImport(ctx context.Context, st *store.Store, log *slog.Logger) {
	done, err := st.MetaGet(ctx, "import_done")
	if err != nil || done == "1" {
		return
	}
	hist := histfilePath()
	if hist == "" {
		return // nothing to import yet; retry check on next daemon start
	}
	stats, err := st.ImportHistfile(ctx, hist)
	if err != nil {
		log.Warn("history auto-import failed", "histfile", hist, "err", err)
		return
	}
	_ = st.MetaSet(ctx, "import_done", "1")
	log.Info("history imported", "histfile", hist,
		"commands", stats.Commands, "imported", stats.Imported, "redacted", stats.Redacted)
}

// idleLoop shuts the daemon down after the configured idle window with no
// client connections. A shell keeps its connection open for its lifetime,
// so this only triggers once every terminal is gone — the daemon then gets
// out of memory until the next shell lazily respawns it.
func idleLoop(ctx context.Context, cancel context.CancelFunc, srv *ipc.Server,
	mgr *config.Manager, log *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			idle := time.Duration(mgr.Get().IdleShutdownMin) * time.Minute
			if srv.ActiveConns() == 0 && time.Since(srv.LastActivity()) > idle {
				log.Info("idle shutdown", "idle", idle)
				cancel()
				return
			}
		}
	}
}

func reloadOnSIGHUP(ctx context.Context, mgr *config.Manager) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			mgr.Reload()
		}
	}
}

// acquireLock takes a non-blocking flock; ok=false means another daemon
// holds it. The lock file (not the socket) is the arbiter because a socket
// file's existence says nothing about whether its owner is alive.
func acquireLock(path string) (unlock func(), ok bool, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() { _ = f.Close() }, true, nil
}

func newLogger(path string, level *slog.LevelVar) (*slog.Logger, func(), error) {
	opts := &slog.HandlerOptions{Level: level}
	if path == "" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), func() {}, nil
	}
	f, err := openLogFile(path)
	if err != nil {
		return nil, nil, err
	}
	return slog.New(slog.NewTextHandler(f, opts)), func() { _ = f.Close() }, nil
}

func setLevel(v *slog.LevelVar, s string) {
	switch s {
	case "debug":
		v.Set(slog.LevelDebug)
	case "warn":
		v.Set(slog.LevelWarn)
	case "error":
		v.Set(slog.LevelError)
	default:
		v.Set(slog.LevelInfo)
	}
}
