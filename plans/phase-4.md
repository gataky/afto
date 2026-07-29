# Phase 4 Implementation Plan — plugin host + fzf integration

**Audience:** an implementing agent/engineer with access to this repository and no
other context. Read `DESIGN.md` (authoritative architecture, esp. §2.3 and §3.2),
`docs/protocol.md`, and the phase reports (`plans/phase-*-report.md`) before
writing code — the reports record ZLE and zpty behaviors that are not
discoverable from the code.

## 1. Mission

Build Phase 4 from `DESIGN.md §5`:

> **Plugin host + fzf widget:** subprocess plugin runtime with circuit breaker +
> a sample plugin; optional `aftod query --list | fzf` widget and frecency-fed
> `^R` alternative.

Phases 1–3 built every suggestion source into the daemon. Phase 4 opens that
door: anything that can read a line of JSON and write one back becomes a
suggestion source, without being written in Go and without being linked into
`aftod`. The second half makes afto's ranking available to fzf for users who
prefer a picker over ghost text.

## 2. Non-negotiable constraints

All prior constraints hold (`plans/phase-1.md §2`, `plans/phase-2.md §2`,
`plans/phase-3.md §2`). The ones this phase can newly violate:

1. **A plugin can never block the prompt.** It is raced by the same engine
   deadline as every built-in provider, and additionally bounded by its own
   `timeout_ms`. A plugin that hangs, crashes, floods, or never answers must
   degrade only itself. This is the phase's whole risk surface: we are
   introducing untrusted, user-supplied processes into a latency-critical path.
2. **A plugin cannot impersonate a built-in.** The host stamps
   `Candidate.Source` with the plugin's configured name, overwriting whatever
   the subprocess claimed. Provenance is the host's to assign.
3. **A plugin cannot rewrite the user's line.** The prefix invariant is already
   client-enforced and applies to plugin candidates identically — this is
   exactly the "misbehaving provider" case `DESIGN.md §2.4.3` was written for.
   No new client-side work; the guarantee must simply not be weakened.
4. **afto still never binds `^T`, `^R`, or `Alt+C`** (`DESIGN.md §2.3`). The
   fzf widget ships **unbound**; the user opts in with their own `bindkey`.
5. **The fzf widget may fork** — it is an explicit user action, not the
   keystroke path. Nothing in `line-pre-redraw` gains a subprocess.
6. **Privacy holds:** plugins are local subprocesses. The daemon sends them the
   same `Query` it gives built-ins, and that is the user's command line — so
   plugin configuration is a trust decision, and the docs must say so plainly.

## 3. Plugin protocol (`docs/plugins.md`, new)

Stdio, JSON-lines, one object per `\n`-terminated line — the same shapes as the
socket protocol, which is the point: "the shell client is effectively plugin #0"
(`DESIGN.md §3.2`).

Daemon → plugin (stdin):
```json
{"v":1,"id":7,"buffer":"kubectl ","cursor":8,"cwd":"/w/proj","last_exit":0,"session":"host.1234.…","recent":["git status"]}
```

Plugin → daemon (stdout):
```json
{"v":1,"id":7,"candidates":[{"text":"kubectl get pods -n prod","score":1.5,"note":"ctx: prod"}]}
```

- **`id` must be echoed.** The host correlates on it and discards mismatches, so
  a late answer to an abandoned request can never be attributed to the next one.
- **`score` is optional** (default 1.0). Scores share the built-ins' scale
  (`ln(1+count)·decay`), so a plugin claiming a huge score outranks history —
  documented as the plugin author's responsibility, bounded by the merge cap.
- **`source` is ignored if sent**; the host stamps the configured plugin name.
- **stderr goes to the daemon log at debug level**, never to the terminal.
- Malformed lines are dropped (counted as a failure); unknown fields ignored.

## 4. Host design (`daemon/internal/plugin`)

One `Host` per configured plugin, registered with the engine as an ordinary
`Provider` — so the race, the budget, and the merge apply unchanged.

**Process model.** Long-lived subprocess, started lazily on first use. A reader
goroutine owns stdout and delivers lines on a channel; requests select on that
channel against their deadline. Requests to one plugin are serialized (a
line-oriented script cannot be assumed concurrency-safe); a caller that cannot
acquire the plugin before its deadline gives up rather than queueing.

**Failure handling** — three layers, because they fail differently:

| Failure | Response |
|---|---|
| Slow answer | Abandon at `timeout_ms`; the late line is drained and dropped by id |
| Crash / EOF | Restart on next use with exponential backoff (1 s → 60 s cap) |
| Repeated failure | Circuit breaker opens: plugin benched, process killed |

A **failure** is a timeout, a write/read error, a malformed response, or a
non-echoing id. Zero candidates is a valid answer, not a failure. Three
consecutive failures open the breaker for a 30 s cooldown; the next request
after cooldown is a half-open probe that closes the breaker on success and
re-opens it on failure. An open breaker returns nil immediately — no process,
no syscall, no latency.

Killing the process when the breaker opens is deliberate: it reclaims a wedged
plugin's resources, and respawning on probe is the only recovery path that fixes
a process stuck mid-line.

**Caps** (a plugin is untrusted input): response lines bounded at 64 KiB,
candidates truncated to `provider.CandidateLimit`, candidate text bounded at
4 KiB. Exceeding a cap truncates rather than fails — a chatty plugin should
degrade, not disappear.

**Config:**
```toml
[[plugin]]
name       = "make-targets"
command    = "/usr/local/bin/afto-make-targets"
args       = []          # optional; no shell is involved, so no quoting rules
timeout_ms = 40          # optional; defaults to latency_budget_ms
enabled    = true        # optional; default true
```
Wired at daemon start, like the built-in provider toggles. `command` is executed
directly (no shell) — there is no word-splitting or injection surface.

## 5. Sample plugin

`afto-make-targets` (Go, `daemon/cmd/afto-make-targets`, built by `make build`):
when the buffer starts with `make `, it parses the `Makefile` in the query's cwd
and suggests targets. It demonstrates the thing built-ins structurally cannot do
— suggesting from *the current directory's contents* rather than from history —
and it is genuinely useful.

`examples/plugins/afto-echo.sh` (POSIX sh, ~15 lines) demonstrates the
language-agnostic claim in the smallest possible form and is exercised by a real
test. (A Python example was considered; `python3` is broken on the dev machine
and a skipped test is not a gate. `sh` is always present.)

## 6. fzf integration (`DESIGN.md §2.3`, additive opt-ins)

Two CLI entry points, because the two use cases want different things:

- **`aftod query --list`** — through the daemon: the live provider stack,
  candidate texts one per line. For scripting against a running daemon.
- **`aftod list [--prefix P] [--cwd D] [--limit N]`** — frecency-ranked history
  read **directly from the store**, like `aftod import` already does (WAL makes
  it safe alongside a running daemon). This is the `^R` replacement's data
  source: it works with no daemon, and with an empty prefix it ranks the whole
  history, which `query` deliberately does not (an empty buffer there means
  next-command prediction).

The zsh side adds one widget, `afto-fzf`, **bound to nothing**: it runs
`aftod list --prefix "$LBUFFER" | fzf` and replaces the buffer with the pick.
Users opt in with `bindkey '^R' afto-fzf`. Documented in `docs/fzf.md` alongside
the reminder that plain fzf coexistence needs no configuration at all.

## 7. Milestones (each = one commit, tests green before moving on)

- **M17 — Host mechanics:** `daemon/internal/plugin` — process lifecycle,
  JSON-lines codec, id correlation, timeout, reader goroutine, restart backoff,
  circuit breaker, caps. Tests drive real subprocesses (shell scripts written
  by the test) covering: normal answer, slow answer, crash mid-session,
  never-answers, malformed line, wrong id, oversized response, breaker
  open→cooldown→probe→close.
- **M18 — Wiring:** `[[plugin]]` config, engine registration, source stamping,
  `serve.go`. Integration test: a real daemon with a configured plugin returns
  its candidates over the socket, and a broken plugin leaves suggestions
  otherwise intact.
- **M19 — Sample plugin + docs:** `afto-make-targets`, `examples/plugins/afto-echo.sh`,
  `docs/plugins.md` (protocol, config, failure semantics, trust note).
- **M20 — fzf integration:** `aftod list`, `aftod query --list`, the `afto-fzf`
  widget (unbound), `docs/fzf.md`.
- **M21 — E2E + gates + report:** harness scenarios per §8, full gates,
  `plans/phase-4-report.md`, `DESIGN.md`/`CLAUDE.md` updates.

## 8. E2E additions (`tests/e2e/harness.zsh`)

- **Plugin candidates reach the ghost:** configure `afto-make-targets` in the
  harness's `config.toml`, create a `Makefile` with a distinctive target in the
  test cwd, type `make ` + prefix → the target appears as a suggestion and
  accepting it puts exactly that text in the buffer.
- **A broken plugin harms nothing:** configure a second plugin that exits
  immediately (or sleeps forever); assert history/frecency suggestions still
  render, no error bytes appear, and typing latency is unaffected (the existing
  no-error-noise scenario covers the terminal side).
- **The fzf widget is not bound:** assert `bindkey '^R'` still reports fzf's
  widget (already covered by S12) and that `zle -l` contains `afto-fzf` — it
  exists but claims nothing.
- Every existing scenario must stay green unchanged.

## 9. Acceptance gates

1. `make test` green; `go vet ./...` clean; isolation grep returns nothing.
2. `make e2e` green — all existing scenarios plus §8.
3. `make bench` p99 keystroke→ghost < 50 ms **with a deliberately hanging
   plugin configured** — the strongest statement of the phase's core promise.
4. A plugin that crashes on every request is benched and the daemon stays
   healthy: pinned by a host test, and visible in the e2e no-noise scenario.
5. `docs/plugins.md` and `docs/fzf.md` exist and match the implementation;
   `DESIGN.md §3.2` updated if anything deviates.
6. Deviations listed in `plans/phase-4-report.md`.

## 10. Left to implementer's judgment (decide and document)

Exact breaker thresholds and backoff curve; whether a timed-out plugin is killed
immediately or only when the breaker opens; whether `aftod list` dedupes across
cwds; the sample plugin's Makefile parsing depth (include `.PHONY`? included
files?); whether `afto-fzf` replaces the whole buffer or only the left part.
