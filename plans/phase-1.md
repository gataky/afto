# Phase 1 Implementation Plan — `aftod` daemon + async zsh client

**Audience:** an implementing agent/engineer with access to this repository and no
other context. Read `DESIGN.md` (authoritative architecture) and `poc/afto.plugin.zsh`
(the validated UI contract) before writing code. This plan is scoped, ordered, and
has explicit acceptance gates.

## 1. Mission

Build the Phase 1 system from `DESIGN.md §5`:

> `aftod` (Go daemon) with `history` + `frecency` providers, HISTFILE import,
> secrets redaction, `precmd` ingestion; production zsh plugin with async socket
> fetch (`zle -F`) and staleness checks. **Ghost text only** — no dropdown/menu UI
> (that is Phase 2), no plugins host (Phase 4), no AI.

The PoC (`poc/afto.plugin.zsh`) proved the UI mechanics with a synchronous in-shell
history lookup. Phase 1 replaces that engine with a daemon over a unix socket while
preserving every guarantee the PoC validated.

## 2. Non-negotiable constraints (violating any of these fails the phase)

1. **TAB is never bound, wrapped, or observed.** No `bindkey '\t'`, no wrapping of
   `expand-or-complete`/`complete-word`/`menu-select`, no `compdef`, no `zstyle`.
   Gate: `grep -nE 'bindkey.*\\t|expand-or-complete|complete-word|menu-select|compdef|zstyle' shell/zsh/afto.plugin.zsh` must return nothing.
2. **Display-only rendering.** Suggestions render exclusively via `$POSTDISPLAY` +
   `region_highlight`. `$BUFFER` is written only in accept widgets, only with text
   already displayed as ghost.
3. **Prefix invariant, enforced client-side.** A suggestion is displayed only if it
   strictly extends `$BUFFER` (`candidate == $BUFFER + nonempty suffix`), regardless
   of what the daemon returns.
4. **Failure mode is absence, never latency or noise.** Daemon down/slow/crashed →
   no ghost text, zero error output to the terminal, typing latency unaffected.
   Nothing in the keystroke path may block: no synchronous socket reads, no
   subprocess spawns (`$(...)`) in hooks, socket writes must be small enough to
   never fill the buffer (<8 KB) and write errors silently disable until reconnect.
5. **Suggest only when `$CONTEXT == start`**, buffer is single-line, and non-empty.
6. **Kill switch:** `AFTO_DISABLE=1` before sourcing → plugin is a no-op;
   `afto off` at runtime unhooks everything.
7. **Privacy:** local-only storage, no network calls, no telemetry. Redaction
   happens before persistence.
8. **Go:** no cgo (SQLite via `modernc.org/sqlite`). Dependencies limited to:
   `modernc.org/sqlite`, `github.com/BurntSushi/toml`, `github.com/fsnotify/fsnotify`.
   Standard library for everything else (incl. the socket server).
9. Do not modify `poc/` (reference implementation) or `references/` (read-only).
   If you must deviate from `DESIGN.md`, update `DESIGN.md` in the same commit and
   list the deviation in your final report.

## 3. Repository facts

- Module path: `github.com/gataky/afto` (matches `git remote`).
- Go 1.26.5 (`.tool-versions`; direnv `layout go` — `GOPATH` under `.direnv/`,
  already gitignored).
- Primary platform: macOS (darwin/arm64), zsh. Code should be portable to Linux
  but macOS is the test target. Note macOS has no `$XDG_RUNTIME_DIR` by default —
  path fallbacks below handle it.

## 4. Deliverables (new files)

```
go.mod, go.sum
Makefile                          # build, test, e2e, bench, lint targets
daemon/cmd/aftod/main.go          # CLI: aftod [serve|import|query|ping|version]
daemon/internal/ipc/              # socket server, JSON-lines + TSV codec, protocol types
daemon/internal/store/            # sqlite open/migrate, ingest, import, redaction
daemon/internal/scoring/          # frecency scoring (pure functions, table-tested)
daemon/internal/provider/         # Provider iface, history & frecency providers, race/merge
shell/zsh/afto.plugin.zsh         # production plugin (async client)
tests/e2e/harness.zsh             # zpty-driven end-to-end test (see §10)
tests/e2e/latency.zsh             # keystroke→ghost latency measurement
```

## 5. Protocol (unix socket)

JSON-lines: one JSON object per `\n`-terminated line. Every message carries
`"v":1`. One connection per shell session, one request in flight at a time.

**suggest** (client → daemon):
```json
{"v":1,"type":"suggest","id":42,"fmt":"tsv","buffer":"git ch","cursor":6,"cwd":"/Users/x/proj","last_exit":0,"session":"host.1234.1722180000","recent":["git add -p"]}
```
Response, `"fmt":"json"` (default — used by `aftod query`, future tooling):
```json
{"v":1,"id":42,"candidates":[{"text":"git checkout main","score":8.1,"source":"frecency"}]}
```
Response, `"fmt":"tsv"` (used by the ZLE client; trivially parseable in zsh):
```
42\tgit checkout main\n
```
Top candidate only. Literal `\t`, `\n`, `\\` inside the text are escaped as two-char
sequences (`\t`,`\n`,`\\`); first unescaped tab is the separator; empty suggestion →
`42\t\n`.

**record** (client → daemon, fire-and-forget, no response):
```json
{"v":1,"type":"record","cmd":"git checkout main","exit":0,"cwd":"/Users/x/proj","session":"host.1234.1722180000","ts":1722180042}
```

**ping** → `{"v":1,"ok":true,"version":"..."}` (used by `aftod ping` and tests).

Malformed lines: log at debug, drop, keep the connection. Unknown `type`: same.

## 6. Storage (`daemon/internal/store`)

Paths (first hit wins): `$AFTO_DATA_DIR` → `$XDG_DATA_HOME/afto` → `~/.local/share/afto`;
DB file `afto.db`. SQLite pragmas: WAL, `busy_timeout=250`, `synchronous=NORMAL`.

```sql
CREATE TABLE events (          -- raw ledger; feeds Phase 3 transitions later
  id INTEGER PRIMARY KEY,
  cmd TEXT NOT NULL, cwd TEXT NOT NULL DEFAULT '',
  session TEXT NOT NULL DEFAULT '', exit_code INTEGER NOT NULL DEFAULT 0,
  ts INTEGER NOT NULL
);
CREATE TABLE stats (           -- aggregate, maintained on every ingest
  cmd TEXT NOT NULL, cwd TEXT NOT NULL DEFAULT '',   -- one row per (cmd,cwd), plus rollup row with cwd=''
  count INTEGER NOT NULL, last_ts INTEGER NOT NULL,
  PRIMARY KEY (cmd, cwd)
);
CREATE INDEX stats_cmd ON stats(cmd);
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);  -- schema_version, import_done
```

Prefix lookup must be a range scan, not `LIKE`:
`WHERE cmd >= :prefix AND cmd < :prefix || X'F4808FBFBD'` (or compute the upper
bound by incrementing the last byte — implementer's choice, must use the index).

**Redaction (before persistence; matching lines are skipped entirely, not stored
masked** — masked text would produce broken suggestions). Starter pattern list
(config-extendable, case-insensitive where sensible):
- `AKIA[0-9A-Z]{16}` (AWS access key), `(?i)aws_secret`
- `(?i)(--password|--token|--secret|--api-key)[= ]\S`
- `(?i)authorization:\s*(bearer|basic)`
- `(?i)^\s*export\s+\w*(TOKEN|SECRET|KEY|PASSWORD)\w*=`
- `xox[baprs]-` (Slack), `ghp_[A-Za-z0-9]{36}` / `github_pat_` (GitHub)
- leading-space commands (`^ ` — user asked the shell to forget it; honor that)

**HISTFILE import** (`aftod import`, auto-run once when `meta.import_done` absent):
parse zsh extended history (`: <ts>:<elapsed>;<cmd>`) and plain format; entries may
span lines (continuation lines don't match the `: …;` prefix — append them).
**Unmetafy:** zsh history files escape bytes ≥ 0x80 with the Meta byte `0x83`
followed by `byte ^ 0x20`; decode before storing or multibyte commands will be
corrupted. Apply redaction to every imported line. Dedupe into `stats` (count,
max ts); also write `events` rows (session `'import'`).

## 7. Providers & scoring

```go
type Provider interface {
    Name() string
    Suggest(ctx context.Context, q Query) ([]Candidate, error)
}
```
(`Query`/`Candidate` shapes per `DESIGN.md §3.1`.)

- **history**: most recent `events`/`stats` prefix match — pure recency. Guarantees
  PoC parity (the PoC suggested the most recent matching history entry).
- **frecency**: prefix matches ranked by
  `score = ln(1+count_all) · 2^(-age_hours/168) + 2.0 · ln(1+count_cwd) · 2^(-age_cwd_hours/168)`
  (7-day half-life; constants in one place, documented as tunable; table-driven
  tests pin the ranking behavior, not exact floats).
- **Race/merge** (`provider` package): run all providers concurrently with a
  per-request budget (default 40 ms, config `latency_budget_ms`); on deadline,
  merge whatever has answered — a slow provider is dropped for that request, never
  awaited. Merge: dedupe identical text (keep max score), stable order, cap 10.
  The daemon does *not* enforce the prefix rule — the client does — but providers
  should only return extensions by construction.

## 8. Daemon lifecycle (`daemon/cmd/aftod`)

- Socket path: `$AFTO_SOCKET` → `$XDG_RUNTIME_DIR/afto/afto.sock` →
  `~/.cache/afto/afto.sock`. Parent dir created 0700; stale socket file from a dead
  daemon is detected (connect fails) and replaced.
- **Lazy start, no launchd:** the *client* spawns the daemon when connect fails
  (see §9). Single-instance enforcement: `flock` on a lockfile next to the socket;
  losing racer exits 0 silently.
- `aftod serve` foreground; `--daemonize` detached (setsid, stdio → logfile).
  Idle shutdown after `idle_shutdown_min` (default 30) with no connections.
- Config `~/.config/afto/config.toml` (all keys optional; missing file = defaults):
  ```toml
  latency_budget_ms = 40
  idle_shutdown_min = 30
  log_level = "info"                 # logs → ~/.local/state/afto/aftod.log
  [providers]
  history = true
  frecency = true
  [redact]
  extra_patterns = []
  ```
  Hot reload via fsnotify (SIGHUP as fallback). Reload swaps config atomically;
  never restarts the process or drops connections.
- Version embedded via `-ldflags`; `aftod version`, `aftod ping` for diagnostics;
  `aftod query --buffer "git ch" [--cwd …]` for humans/tests (JSON to stdout).

## 9. Production zsh plugin (`shell/zsh/afto.plugin.zsh`)

Start from `poc/afto.plugin.zsh` (keep: ghost render/clear, accept widgets,
`line-finish` hook, `afto off`, all guards). Replace `_afto_engine` with the async
client; add ingestion. Requirements:

- `zmodload zsh/net/socket zsh/system zsh/datetime` (all builtin — the keystroke
  path spawns no processes, ever).
- **Connect:** `zsocket $sockpath` on load (and lazily on demand). On failure:
  spawn `aftod serve --daemonize &!` at most once per `connect_backoff` (30 s),
  stay silent, retry on later keystrokes.
- **Request path** (`line-pre-redraw`): JSON-escape `$BUFFER`/`$PWD` in pure zsh —
  escape `\`, `"`, and control chars (`\n`,`\t`,`\r`; drop other C0) via parameter
  expansion; send `suggest` with `fmt:"tsv"` and a monotonically increasing id.
- **One in-flight request + dirty flag:** if the buffer changes while waiting,
  mark dirty; when the response arrives, if dirty, immediately send a fresh
  request. Never queue more than one.
- **Response path:** `zle -F $fd _afto_response`; handler reads one line
  (`sysread`/`read -u`), parses `id\tsuggestion`, unescapes, then displays only if
  (a) id matches the in-flight id and (b) the suggestion strictly extends the
  *current* `$BUFFER` (staleness + prefix invariant in one check), then `zle -R`.
  On read error/EOF: close fd, deregister, silently enter reconnect backoff.
- **Ingestion (`precmd`):** capture `$?` first; get the last command with
  `fc -ln -1` (builtin); skip if empty or identical to the previously recorded
  command (guards Enter-on-empty-prompt repeats); fire-and-forget a `record`
  message over the same socket. `preexec` is not needed in Phase 1.
- **Session id:** `${HOST}.$$.${EPOCHSECONDS}` computed once at load.
- `afto off` additionally closes the fd and deregisters the `zle -F` handler.
  `afto status` (new): prints connect state, socket path, daemon version via ping —
  the only place output is allowed.

## 10. Milestones (each = one commit, tests green before moving on)

- **M1 — Scaffold:** `go mod init github.com/gataky/afto`; Makefile
  (`build → bin/aftod`, `test`, `e2e`, `bench`, `vet`); `aftod version`.
- **M2 — Protocol + codec** (`ipc`): types, JSON-lines framing, TSV encoder with
  escaping, table tests incl. malformed-input handling.
- **M3 — Store:** open/migrate, ingest (events + stats upsert), prefix range-scan,
  redaction engine + starter patterns, HISTFILE import with unmetafy. Fixture-based
  tests: extended/plain/multiline/metafied history files; every redaction pattern.
- **M4 — Providers:** history, frecency, scoring table-tests, race/merge with a
  deliberately-slow fake provider proving the deadline drop.
- **M5 — Daemon:** socket server wiring ipc→providers→store, lazy-start locking,
  idle shutdown, config + hot reload, logging, `ping`/`query`/`import` subcommands.
  Integration test: real daemon on a temp socket, suggest/record round-trips,
  concurrent connections.
- **M6 — zsh plugin:** async client per §9. Manual smoke: `source` it, verify ghost
  from daemon, `kill` the daemon mid-session → typing unaffected, ghost returns
  after auto-respawn.
- **M7 — E2E + gates:** `tests/e2e/harness.zsh` using `zmodload zsh/zpty`: spawn
  an interactive `zsh -f -i` under zpty, source the plugin, seed history through
  the daemon, then drive keystrokes with `zpty -wn` and assert on captured output
  (strip ANSI before matching). Required scenarios: (a) ghost text renders dim
  (`\e[90m` bytes present) after typing a known prefix; (b) `^]` accept → the
  executed command equals exactly what was displayed (assert via a
  `&& print MARKER` suffix); (c) with ghost visible, TAB on a partial dotfile name
  (`ls .zshrc-f<TAB>` against a fixture file) completes natively and never replaces
  the typed buffer; (d) `kill` the daemon mid-session → typing continues clean,
  no error bytes appear. `tests/e2e/latency.zsh`: measure keystroke→ghost via
  timestamped zpty reads. Run the full checklist in `DESIGN.md §6` (items 1–6, 9–11 apply).

## 11. Acceptance gates (all must pass)

1. `make test` green; `go vet ./...` clean.
2. Isolation grep (constraint #1) returns nothing.
3. E2E harness passes, including the native-TAB and daemon-kill scenarios.
4. Latency: p99 keystroke→ghost < 50 ms over ≥200 keystrokes on the dev machine
   (with warm daemon); daemon-side per-request handle time logged and < 40 ms.
5. `aftod import` on a copy of the real `~/.zsh_history` completes without error;
   spot-check that a known secret-looking line was skipped.
6. Fresh shell with `AFTO_DISABLE=1` → `zle -l`/`bindkey` diff vs vanilla is empty.
7. `DESIGN.md` updated if anything deviated; final report lists deviations, known
   gaps, and anything punted to Phase 2.

## 12. Left to implementer's judgment (don't ask, decide and document)

Debounce (whether to add one at all vs pure dirty-flag), exact SQLite busy/retry
handling, log rotation, TSV upper-bound byte trick vs increment, `stats` rollup row
vs `SUM` query, Makefile vs adding `just` (repo has no preference yet).
