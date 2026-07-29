# Phase 4 — Completion Report

Implementation of `plans/phase-4.md` on branch `phase-4`, one commit per
milestone (M17 host, M18 wiring, M19 sample+docs, M20 fzf, M21 gates). All
acceptance gates pass.

## Gate results (plans/phase-4.md §9)

| # | Gate | Result |
|---|------|--------|
| 1 | `make test` green, `go vet` clean, isolation grep empty | ✅ 9/9 packages (new: `plugin`, `afto-make-targets`) |
| 2 | `make e2e` — all existing scenarios plus §8 | ✅ 40 scenarios; the whole suite now runs with a hanging and a crashing plugin configured |
| 3 | `make bench` p99 < 50 ms **with a hanging plugin configured** | ✅ p50 6.5 ms, p90 6.9 ms, **p99 7.4 ms**, max 47.5 ms |
| 4 | A crashing plugin is benched, daemon stays healthy | ✅ host tests + `TestDaemonSurvivesABrokenPlugin` + e2e "repeatedly failing plugin was benched" |
| 5 | `docs/plugins.md` and `docs/fzf.md` match the implementation | ✅ every documented command was executed while writing them |
| 6 | Deviations listed | ✅ below |

### What the bench number actually shows

The interesting figure is `max: 47.5 ms` against a `p99` of 7.4 ms, with a
daemon-side worst handle time of 41.1 ms. That is the containment working,
visible in one number: the first few keystrokes wait out the full 40 ms
latency budget for a plugin that will never answer, the breaker benches it
after three consecutive failures, and every subsequent keystroke returns to
the ~7 ms baseline. A plugin can cost the budget a handful of times; it
cannot cost it forever, and it never costs more than the budget.

## Deviations from the plan (all deliberate)

1. **Plugins are pre-warmed at daemon start**, not "started lazily on first
   use" as `plans/phase-4.md §4` said. The integration test showed why: a
   cold `fork`+`exec` reliably loses its race against the 40 ms budget, so
   lazy start meant the first keystroke after daemon startup silently
   dropped plugin candidates — precisely when a user who just configured a
   plugin is watching for it. Warm-up runs off the serving path, and the
   lazy path (with its backoff) still covers a plugin that fails to start.
2. **An over-64 KiB response line is a failure, not a truncation.** The plan
   said caps "truncate rather than fail"; that holds for candidate count and
   text length, but a line past the scanner's limit cannot be truncated into
   valid JSON, so it is dropped and counted as a failure. The distinction is
   pinned by a test.
3. **The sample plugin is Go, with a POSIX-sh example alongside**, rather
   than the Python illustration first considered — `python3` is broken on
   the dev machine, and a skipped test is not a gate. `sh` is always
   present, so `examples/plugins/afto-echo.sh` is actually executed by a
   test through the production host.
4. **`aftod list` is a separate subcommand from `aftod query --list`.**
   `DESIGN.md §2.3` mentions only the latter. Both exist because they answer
   different questions: `query` runs the live provider stack for a prefix,
   while an *empty* buffer there means "predict what comes next" — right for
   ghost text, wrong for a fuzzy finder. `list` reads the store directly and
   ranks the whole history, so the `^R` replacement also works with no
   daemon running.

## Implementation notes worth knowing (the non-obvious bits)

- **The protocol is the easy part.** Everything in `daemon/internal/plugin`
  exists to contain failure, and the three layers are not interchangeable:
  a *slow* plugin needs its request abandoned (but the process kept), a
  *dead* one needs a restart with backoff, and a *persistently broken* one
  needs to stop being asked at all. Collapsing any two of these produces
  either a fork bomb or a plugin that costs the budget on every keystroke
  forever.
- **A late answer must be dropped by id, not by position.** Without the
  echo-and-match, a plugin that answers request N after the deadline hands
  that answer to request N+1 — which is worse than silence, because it is
  silently wrong suggestions.
- **`zle -l` prints `name (function)`** for a widget defined with
  `zle -N name fn`, so `grep -x name` never matches. Cost a red test.
- **Makefile recipes run under `sh`**, where zsh's `print` does not exist —
  a fixture using it fails invisibly (the target "succeeds", printing
  nothing).
- **zpty appends to whatever is on the line.** A scenario that types without
  pressing Enter prefixes the *next* scenario's command with its leftovers.
  Scenarios must leave a clean line.
- **Assertion markers must not be suffixes of one another**: markers are
  matched end-anchored, and `UNBOUND` ends with `BOUND`.
- The e2e suite keeps a hanging and a crashing plugin configured for its
  whole run, so every scenario — not just the plugin ones — is evidence that
  a broken plugin changes nothing.

## Known gaps (deliberate)

- Plugins are wired at daemon start; editing `[[plugin]]` needs a `kill`
  (the next keystroke respawns the daemon). Consistent with the existing
  provider toggles, which also do not hot-reload.
- No sandboxing: a plugin is a subprocess with the user's privileges, and
  the daemon hands it the command line. Documented as a trust decision in
  `docs/plugins.md` rather than solved.
- The breaker is per-plugin and in-memory; a daemon restart forgets that a
  plugin was misbehaving.
- `afto-fzf` replaces the whole buffer with the pick. Preserving `$RBUFFER`
  would be friendlier for mid-line use.
- zsh-syntax-highlighting is still not installed on this machine, so that
  half of the coexistence scenario reports a SKIP.
