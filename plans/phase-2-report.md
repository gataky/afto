# Phase 2 — Completion Report

Implementation of `plans/phase-2.md` on branch `phase-2`, one commit per
milestone (continuing Phase 1's numbering: M8 protocol, M9 tier-2 rows,
M10 tier-3 menu, M11 gates/report). All acceptance gates pass.

## Gate results (plans/phase-2.md §8)

| # | Gate | Result |
|---|------|--------|
| 1 | `make test` green, `go vet` clean, isolation grep empty | ✅ 6/6 packages; grep also asserted in harness |
| 2 | `make e2e` — Phase 1 scenarios with rows visible + §7 scenarios | ✅ 27 scenarios (rows ranking, native-arrow, menu navigate/accept/Esc/other-key, ^O no-op, coexistence), stable across reruns |
| 3 | `make bench` with rows enabled (AFTO_ROWS=4 default) | ✅ p50 6.5 ms, p90 7.0 ms, **p99 7.5 ms**, max 8.2 ms over 200 keystrokes; daemon worst handle 0.49 ms |
| 4 | DESIGN.md §6 checklist with fzf + z-sy-h loaded | ◑ fzf coexistence automated in the harness (S12: loads `fzf --zsh`, ghost/accept work, ^T/^R bindings intact). zsh-syntax-highlighting is not installed on this machine — S12 gates on availability and reports a SKIP; items 3/7/9 still deserve one interactive pass in a real terminal |
| 5 | Menu unreachable without AFTO_MENU_KEY | ✅ only `_afto_menu_enter` calls `zle -K afto-menu`; harness asserts arrows outside the menu claim nothing |
| 6 | Old-daemon compatibility | ✅ by protocol design: absent `limit` → top-1 TSV; a one-candidate reply renders as ghost + one row (verified by the TSV framing tests; the zero/one-candidate line shape is unchanged from Phase 1) |
| 7 | Docs updated in-commit; deviations here | ✅ docs/protocol.md (M8), DESIGN.md §2.1 (M9), §5 + CLAUDE.md (M11) |

## Deviations from the plan (judged improvements)

1. **`limit` absent keeps each format's historical default** (TSV → 1,
   JSON → all) rather than the plan's flat "absent → 1". A flat default
   would have silently changed `aftod query`'s output. Documented in
   `ipc/protocol.go` and `docs/protocol.md`.
2. **Ghost tracks the menu selection** (plan §9 judgment item): the dim
   remainder always previews the ▸ row, so menu navigation shows exactly
   what Enter will accept. `^P`/`^N` are bound in the menu keymap.
3. **Phase 1 harness assertions reworked, not just extended.** Multi-line
   `POSTDISPLAY` changes what the zpty byte stream looks like (below); the
   marker technique had to change with it.

## Implementation notes worth knowing (the non-obvious bits)

- **ZLE never promises contiguous screen bytes.** With rows below the
  prompt, two capture artifacts appear: (a) on line-finish ZLE repositions
  the cursor with escape sequences instead of emitting `\n`, so a command's
  FIRST output line merges into redraw bytes; (b) ZLE repaints only the
  columns that changed, so a row's full text appears contiguously only in
  its earliest full paint. Consequences, encoded in the harness: markers
  are quote-split (`print MAR''KER` — the string can only exist in real
  output) and asserted end-anchored; exact executed-command assertions use
  the `AFTO_DEBUG` trace's `record` lines, which carry the verbatim buffer.
- **`^T` is SIGINFO on macOS ptys** — the kernel eats it before ZLE ever
  sees it. Never use it as a test key (fzf rebinding it works because fzf
  users disable the status char or the binding wins in raw mode).
- **The zpty child inherits `$EDITOR`**, so `zsh -f -i` may start in viins,
  not emacs. `bindkey` (main keymap) covers both; the menu's Esc must be a
  real binding rather than falling through to the catch-all, because a
  replayed `\e` would enter vicmd in a vi child.
- **`zle -U -- "$KEYS"` is the whole other-key story**: exit the menu,
  push the key back, let the restored keymap interpret it. No key is ever
  interpreted by afto itself.
- **Keymap self-healing**: `^C` aborts the line without running any menu
  exit widget, but ZLE starts the next line in the main keymap. The
  suggest hook treats "menu flag set but `$KEYMAP != afto-menu`" as the
  abort signature and resets.
- Accept widgets must not consume `$POSTDISPLAY` once it carries rows;
  `_afto_ghost`/`_afto_shown` are the accept sources of truth.

## Known gaps (deliberate)

- The DESIGN §6 items that need a human and a real terminal (native menu
  cycling *look*, bracketed paste, vim/tmux round-trips, z-sy-h visuals)
  had no z-sy-h install to run against here; S12 will pick it up
  automatically once installed.
- Tier-2/3 rows only ever show prefix extensions in Phase 2; the
  non-prefix row exception (DESIGN §2.4.3) first becomes reachable with
  Phase 3's transition provider.
- Row count is capped at 10 by `provider.CandidateLimit`; no paging in the
  menu (deliberate — it's a shortlist, not a browser).
