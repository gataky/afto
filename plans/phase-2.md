# Phase 2 Implementation Plan — native dropdown (tier-2 list + tier-3 menu mode)

**Audience:** an implementing agent/engineer with access to this repository and no
other context. Read `DESIGN.md` (authoritative architecture, esp. §2.1–§2.4),
`docs/protocol.md`, and `shell/zsh/afto.plugin.zsh` (the Phase 1 client this phase
extends) before writing code. `plans/phase-1-report.md` records the zpty/zle facts
that make the tests meaningful — reread it before touching the harness.

## 1. Mission

Build Phase 2 from `DESIGN.md §5`:

> **Native dropdown:** passive tier-2 list (multi-line `POSTDISPLAY` +
> `region_highlight` rows) and tier-3 menu mode (custom keymap via `^O`). Exit
> criteria: checklist passes *with the list visible*; menu mode never activates
> without explicit entry.

Phase 1 delivered ghost text fed by a daemon that already ranks up to 10
candidates — but the TSV response carries only the top one. Phase 2 (a) lets the
ZLE client request N candidates, (b) renders the top rows passively below the
prompt, and (c) adds an explicitly-entered menu keymap to pick a row. No new
providers, no daemon behavior changes beyond the response shape.

## 2. Non-negotiable constraints (unchanged from Phase 1, restated)

All of `plans/phase-1.md §2` still applies verbatim; the ones this phase can
newly violate:

1. **TAB is never bound, wrapped, or observed.** The isolation grep must stay
   clean. Corollary for this phase: no identifier or comment in the plugin may
   contain the literal strings `menu-select`, `complete-word`,
   `expand-or-complete` — the gate greps the whole file. (`afto-menu` is fine.)
2. **While the passive list is visible, no keys are claimed.** TAB completes
   natively, arrows do history, typing refines. Row rendering is display-only:
   `$POSTDISPLAY` + `region_highlight`, nothing else.
3. **Menu mode activates only via its dedicated key** (`AFTO_MENU_KEY`,
   default `^O`), and only while rows are visible. Its keymap claims keys only
   after entry; `Esc` exits; any other key exits and behaves natively.
4. **`$BUFFER` is written only by accept widgets, only with text already shown.**
   Menu-mode `Enter` writes the selected row's text into the buffer and does
   **not** execute it.
5. **Failure mode is absence.** All new client code stays fork-free and
   non-blocking; a daemon that returns one candidate (old daemon) or garbage
   degrades to Phase 1 behavior or silence, never noise.

## 3. Protocol change: `limit` + multi-candidate TSV

Additive, per the evolution rules in `docs/protocol.md` (no `v` bump):

- **Request:** `suggest` gains optional `"limit": N` — the number of candidates
  the client wants. Absent/0/negative → 1. Daemon clamps to the engine cap (10).
  Applies to both response formats (JSON today returns everything; with `limit`
  it returns at most N).
- **TSV response** becomes `id \t text1 \t text2 … \n`, up to `limit`
  candidates. The existing escaping contract already guarantees the only real
  tab bytes are separators (literal `\t`/`\n`/`\` in text are two-char escapes),
  so the zsh client splits on tab and unescapes each field. Zero candidates
  stays `id\t\n`. Old clients never see multi-candidate lines because they never
  send `limit` — backward compatible in both directions.
- `aftod query` gains `--limit N` (default 10) for humans/tests.
- Update `docs/protocol.md` in the same commit (round-trip example + TSV
  paragraph + evolution note).

## 4. Tier 2 — passive rows (client)

Target rendering (DESIGN §2.1), with `$BUFFER == "git ch"`:

```
$ git ch█eckout main          ← BUFFER + dim ghost (remainder of SELECTED row)
  ▸ git checkout main         ← row 1, selected (AFTO_HIGHLIGHT_SELECTED)
    git cherry-pick abc123    ← rows 2..N (AFTO_HIGHLIGHT_ROW)
```

- **Config (client env vars, set before sourcing):**
  - `AFTO_ROWS` — 0..10 rows, **default 4**, 0 = ghost-only (Phase 1 behavior).
    Clamped at load. (DESIGN §2.1 called this `menu.passive_rows`; it is pure
    rendering, so it lives client-side — update DESIGN in the same commit.)
  - `AFTO_HIGHLIGHT_ROW` — default `fg=8`; `AFTO_HIGHLIGHT_SELECTED` — default
    `standout`.
- **State:** the client caches the last response's candidate list
  (`_afto_cands`) and a selection index (`_afto_sel`, 1 in passive mode). The
  ghost is the selected candidate's remainder; accept widgets stop consuming
  `$POSTDISPLAY` (it now also holds rows) and use the tracked ghost/shown text
  instead. `_afto_shown` (full selected text) remains the accept source of
  truth.
- **Render pipeline** (one function, called from response path, fast path, and
  menu navigation): filter cached candidates to strict extensions of the
  *current* `$BUFFER` (prefix invariant + staleness in one pass — same rule as
  Phase 1, now applied to the whole list); render ghost + up to `AFTO_ROWS`
  rows into `$POSTDISPLAY`; add one `region_highlight` entry per segment,
  tracked in an array for exact removal. Rows show the **full candidate text**,
  prefixed `  ▸ ` (selected) / `    ` (others). Offsets are character positions
  into `BUFFER+POSTDISPLAY`, as in Phase 1.
- **Request:** `limit` = max(1, `AFTO_ROWS`). The local fast path (typing
  through the ghost) re-filters the cache and re-renders instantly; the async
  refresh still runs.
- Single-line guard, `$CONTEXT == start` guard, non-empty-buffer guard, and
  line-finish clearing all apply to rows exactly as to the ghost.
- Rows may scroll the screen near the bottom edge — accepted tradeoff
  (DESIGN §2.1); no cursor-position probing, no escape-sequence tricks.

## 5. Tier 3 — menu mode (client)

Modeled on zsh's own menu-selection keymap mechanics: a dedicated keymap that
exists only while active.

- **Entry:** `AFTO_MENU_KEY` (default `^O`) is bound in the main keymap to a
  widget that: does nothing (silently) unless rows are currently visible;
  otherwise records the current keymap name, switches with `zle -K afto-menu`,
  and re-renders. No timer, no hidden triggers — explicit entry only.
- **Keymap `afto-menu`** (created with `bindkey -N afto-menu`, no parent):
  - `↑`/`↓` (both CSI and SS3 encodings) and `^P`/`^N` — move selection,
    clamped to [1, #rows]; re-render; ghost tracks the selection.
  - `Enter` (`^M`) — `BUFFER` ← selected row's full text (already displayed —
    contract §2.4), cursor to end, clear display, exit menu. **Does not
    execute.**
  - `Esc` (`^[`) — exit menu, restore keymap, selection back to 1, re-render
    passively. (Arrow disambiguation via `KEYTIMEOUT` is standard zsh
    behavior.)
  - **Every other key** — catch-all binding over the full byte range: exit menu
    exactly like `Esc`, then `zle -U -- $KEYS` so the key replays through the
    restored keymap — printables self-insert, everything else does its native
    thing. afto never interprets the key itself.
- **Self-heal:** `^C`/abort resets ZLE to the main keymap without running our
  exit widget. The `line-pre-redraw` hook notices `menu-active && $KEYMAP !=
  afto-menu` and clears the flag. `line-finish` also resets it.
- **Suggest hook while active:** navigation widgets trigger `line-pre-redraw`;
  the hook must return immediately in menu mode (buffer unchanged — a re-render
  would clobber the selection).
- **`afto off`** additionally: exits menu if active, `bindkey -r` the menu key,
  `bindkey -D afto-menu`.
- Phase 2 rows are always strict buffer extensions (history/frecency), so
  menu-accept is always an extension; the DESIGN §2.4.3 exception (non-prefix
  rows from the transition provider) becomes reachable only in Phase 3 and
  changes nothing here.

## 6. Milestones (each = one commit, tests green before moving on)

- **M1 — Protocol:** `limit` in `ipc.Request`, multi-candidate `EncodeTSV`,
  server slicing, `aftod query --limit`, table tests (incl. escaping across
  multiple fields, zero-candidate line, clamp), `docs/protocol.md` update.
- **M2 — Tier 2:** render pipeline + candidate cache in the plugin, accept
  widgets reworked off `$POSTDISPLAY`, `AFTO_ROWS`/highlight config,
  `DESIGN.md §2.1` config-name update. Manual smoke per §8 checklist items.
- **M3 — Tier 3:** menu keymap per §5, `afto off` cleanup, self-heal.
- **M4 — E2E + gates + report:** harness scenarios per §7, latency bench rerun
  with rows enabled, `plans/phase-2-report.md`, stale-docs fixes.

## 7. E2E additions (`tests/e2e/harness.zsh`)

Run the whole existing harness with `AFTO_ROWS=4` exported — that alone turns
S1–S5 into "with the list visible" variants (checklist requirement). Seed a
dedicated deterministic prefix for menu tests: `true menupick-alpha && print
MP-ALPHA` ×3 (row 1 by count) and `true menupick-beta && print MP-BETA` ×1
(row 2). New scenarios:

- **Rows render:** after typing `true menupick-`, stripped output contains a
  `▸`-marked row with `menupick-alpha` and an unmarked row with
  `menupick-beta`.
- **Arrows stay native outside menu:** with rows visible, press `↓` (no `^O`),
  then Enter → neither `MP-ALPHA` nor `MP-BETA` printed (typed text alone ran).
- **Menu navigate + accept:** `^O`, `↓`, `Enter` → nothing executes yet; then
  type ` && print MENU-OK`, Enter → exactly `MP-BETA` and `MENU-OK` printed
  (proves selection moved, accept inserted row 2's exact text, Enter-in-menu
  did not execute).
- **Esc/other-key exit:** `^O`, `Esc`, type suffix, Enter → typed line executes
  verbatim. `^O`, type a printable → it self-inserts (assert via executed
  line).
- **TAB with list visible** — covered by existing S3 now running with rows;
  keep the assertion that completion came from the filesystem.
- Existing S5 (no error noise), S6 (isolation grep), S7 (`AFTO_DISABLE`) run
  unchanged and must stay green.

## 8. Acceptance gates (all must pass)

1. `make test` green; `go vet ./...` clean; isolation grep returns nothing.
2. `make e2e` green — all Phase 1 scenarios (now with rows visible) plus §7.
3. `make bench` with rows enabled: p99 keystroke→ghost < 50 ms still holds.
4. Manual run of `DESIGN.md §6` checklist items 1–9 with `zsh-syntax-highlighting`
   + fzf loaded; item 8 (menu-only arrows) explicitly.
5. Menu mode is unreachable without `AFTO_MENU_KEY`: no code path calls
   `zle -K afto-menu` except the entry widget.
6. Old-daemon compatibility: plugin against a Phase 1 daemon (no `limit`
   support → single-candidate TSV) degrades to ghost-only, silently.
7. `DESIGN.md`/`docs/protocol.md` updated in the same commits as the code they
   describe; deviations listed in `plans/phase-2-report.md`.

## 9. Left to implementer's judgment (decide and document)

Exact highlight defaults if `standout` renders poorly in the test terminal;
whether the ghost hides while its row is the selected one vs always shown;
`^P`/`^N` inclusion in the menu keymap; how many rows the bench renders.
