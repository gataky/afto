# Phase 3 — Completion Report

Implementation of `plans/phase-3.md` on branch `phase-3`, one commit per
milestone (M12 store, M13 ranking/prediction, M14 notes, M15 client, M16
gates). All acceptance gates pass.

## Gate results (plans/phase-3.md §9)

| # | Gate | Result |
|---|------|--------|
| 1 | `make test` green, `go vet` clean, isolation grep empty | ✅ 7/7 packages (new: `project`); grep clean, also asserted in-harness |
| 2 | `make e2e` — every existing scenario plus §8 | ✅ 34 scenarios, stable across reruns |
| 3 | `make bench` p99 < 50 ms with the wider scan + project resolution | ✅ p50 6.5 ms, p90 7.0 ms, **p99 7.2 ms**, max 7.5 ms; daemon worst handle 0.55 ms (unchanged from Phase 2 — the project walk is cached and the scan stayed one query) |
| 4 | A v1 database migrates in place and keeps serving | ✅ `TestMigrateV1BackfillsTransitions` builds the Phase 1/2 schema verbatim, seeds it, opens it with current code, and asserts backfill correctness + idempotence on re-open |
| 5 | Old/new compatibility both directions for notes | ✅ by construction and pinned by tests: a reply with no notes is byte-identical to a Phase 2 line, and notes are sent only to clients that set `"notes":true` |
| 6 | Docs updated in-commit; deviations here | ✅ `docs/protocol.md` (M14), `DESIGN.md`/`CLAUDE.md` (M16) |

## Deviations from DESIGN.md / the plan (all deliberate)

1. **`alias-note` is a Decorator, not a Provider.** `DESIGN.md §3.1` lists it
   among the providers, but the `Provider` interface returns a source's own
   candidates, while alias-note annotates whatever the other sources
   produced. It runs as a post-merge `Decorator` stage in the engine —
   after the cap, so no work is spent on candidates nobody will see. Phase
   4's plugin notes will want the same stage. `DESIGN.md §3.1` updated to
   say so.
2. **Empty-prompt predictions are opt-in per keystroke.** The plan and
   DESIGN describe "empty-prompt next-command rows in the menu"; the
   default is now that a bare prompt shows *nothing* until `^O`
   (`AFTO_EMPTY_ROWS`, default 0, opts into passive rows). Rendering rows
   under every fresh prompt would move the prompt after every command,
   which is precisely the kind of unsolicited disruption the project
   exists to avoid.
3. **Imported history does not teach transitions.** A `HISTFILE`
   interleaves every terminal that was open, so consecutive lines are not
   causally related. Consequence worth telling users: prediction is quiet
   on a fresh install and becomes useful after a day of real use.
4. **`aftod query` no longer requires `--buffer`**, and gained
   `--session`/`--recent`: an empty buffer is now a real question ("what
   comes next"), not a mistake.
5. **Scoring weights stay Go constants** (Phase 1 precedent) rather than
   becoming config; `[project] markers` is configurable.

## Implementation notes worth knowing (the non-obvious bits)

- **A keymap switch from the response handler's widget does not stick.**
  ZLE keeps reading with the keymap that was current when the key was
  awaited, so an async `zle -K` silently loses the next keystroke to the
  old keymap — concretely, menu `Enter` executed the line instead of
  accepting a row. Menu entry is therefore always synchronous in the `^O`
  widget: open immediately, fill the rows in when the answer arrives. An
  open-but-empty menu is harmless because every key it doesn't bind exits
  and replays natively.
- **On an empty buffer, the prefix invariant has no teeth** — every
  candidate "extends" an empty string. Without a second check, an answer
  computed for the *previous* command line gets cached as if it were this
  prompt's predictions, and `^O` opens a menu of stale suggestions. The
  client now records which buffer each request was made for and drops
  responses that no longer match. This is the same class of bug the
  non-empty case never had, and it is why the `-n $BUFFER` guard existed
  in Phase 1.
- **Predictions are made unacceptable by omission, not by a check.** The
  client leaves `_afto_ghost` empty for a non-extending candidate; since
  every accept widget consumes the ghost, there is no code path by which
  `→`, `^]`, or `forward-word` can take one. Structural beats conditional.
- **Path-prefix ranges are not path containment.** `cwd >= '/w/proj' AND
  cwd < '/w/prok'` sweeps in `/w/proj-old`. The project scan matches
  `cwd = root OR cwd BETWEEN root+'/' AND …` instead; a test pins the
  sibling case.
- **The trace records commands as typed.** The harness's quote-split
  markers (`print MAR''KER`) appear split in `record` lines and unsplit in
  real output — assertions must pick the right one.
- macOS eats `^T` on a pty (SIGINFO), so it is unusable as a test key; the
  menu key `^O` and accept key `^]` are both safe.

## Known gaps (deliberate)

- Transitions are first-order (previous command only). Longer context
  (`Recent[1..]`, cwd-conditioned pairs) is a ranking improvement, not an
  architecture change.
- Only regular aliases are annotated; global (`-g`) and suffix (`-s`)
  aliases are out of scope.
- The alias message is capped near 6 KB; a larger table simply yields
  fewer notes.
- Project roots are resolved from markers only — no per-user overrides
  beyond the marker list.
- zsh-syntax-highlighting still isn't installed on this machine, so the
  coexistence scenario reports a SKIP for it (fzf half runs and passes).
