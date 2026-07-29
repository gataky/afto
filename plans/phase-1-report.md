# Phase 1 — Completion Report

Implementation of `plans/phase-1.md` on branch `phase-1`, one commit per
milestone (M1–M7). All acceptance gates pass.

## Gate results (plans/phase-1.md §11)

| # | Gate | Result |
|---|------|--------|
| 1 | `make test` green, `go vet` clean | ✅ 6/6 packages, vet + gofmt clean |
| 2 | Isolation grep returns nothing | ✅ (also asserted in the e2e harness, scenario S6) |
| 3 | E2E harness incl. native-TAB and daemon-kill | ✅ 12/12 scenarios (`make e2e`) |
| 4 | Latency p99 < 50ms over ≥200 keystrokes | ✅ p50 11.2ms, p90 12.3ms, **p99 15.9ms**, max 16.5ms; daemon-side handle times sub-millisecond (`make bench`) |
| 5 | Import of real `~/.zsh_history` copy + secret spot-check | ✅ 558 lines → 522 commands (multiline collapsed); appended canary secret was the 1 redacted entry; SQLite query for its prefix returns 0 rows |
| 6 | `AFTO_DISABLE=1` indistinguishable from vanilla | ✅ `zle -l`/`bindkey`/hook-list diff empty (harness S7) |
| 7 | Deviations documented | ✅ this file |

## Deviations from the plan (all judged improvements; DESIGN.md unaffected)

1. **`preexec` instead of `fc -ln -1` for ingestion (§9).** The plan's
   `fc -ln -1` requires a `$(…)` command substitution — a fork in a hook.
   `preexec` receives the exact command string as `$1` with zero forking and
   makes the "skip empty Enter" dedupe unnecessary (preexec simply doesn't
   fire then). Repeat executions are deliberately recorded — they are the
   frecency signal.
2. **`daemon/internal/config` package added** (not in the §4 deliverables
   list). Config load/hot-reload deserved its own tested package rather than
   living untested in `cmd/aftod`.
3. **provider→store dependency.** The plan/DESIGN doc implied provider stays
   storage-free; the built-in providers read the store through a narrow
   `statsReader` interface defined on the consumer side (fakeable in tests,
   no SQLite in provider unit tests). `provider/types.go` documents it.
4. **`AFTO_DEBUG=<file>` client trace facility** added to the plugin — an
   event trace (connects, sends, responses, hook firings) written to a file,
   never the terminal. It was necessary to debug the async path and is
   worth keeping.
5. **`aftod serve --foreground`** flag added (log to stderr) for development.
6. **Provider enable/disable is start-time, not hot-reloaded** (budget,
   redact patterns, and log level do hot-reload). Documented in config.go.

## Implementation notes worth knowing (the non-obvious bits)

- **ZLE specials don't exist in `zle -F` handlers.** `$BUFFER`/`$CURSOR`/
  `$POSTDISPLAY` read as empty there. The response path is therefore split:
  the fd handler only reads the line and stashes it; `zle _afto_process`
  invokes a real widget where the specials work. Same shape as
  zsh-autosuggestions' async mode.
- **zpty testing facts** (encoded as comments in `tests/e2e/harness.zsh`):
  type char-by-char or `zle -F` never fires; Enter is `$'\r'` (`zpty -w z ""`
  sends nothing); drain the pty continuously or the child wedges; assert on
  ANSI-stripped whole lines.
- **A sourced plugin must end with status 0** — the optimistic first-load
  connect legitimately fails while the daemon is still spawning.
- **macOS unix sockets cap paths at 104 bytes** — tests use `mktemp -d
  /tmp/…`; the default runtime paths are safely short.
- The harness kills only *its* daemon by resolving the pid via
  `lsof -t <lockfile>` — never `pkill aftod`, which would hit a real one.

## Known gaps (deliberate, for later phases)

- vi-mode keymaps untested/unwired (Phase 5 per DESIGN.md §5).
- Multiline buffers: guarded out of suggestions by design (Phase 5 polish).
- `recent` commands context is not yet sent by the plugin (field exists in
  the protocol; the transition provider in Phase 3 is its consumer).
- Bash client not started (plan scoped Phase 1 to zsh).
- Log rotation is a single 10MB `.old` cutover; no rotation dependency.
