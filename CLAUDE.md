# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

afto is a shell autocompletion/suggestion tool designed to **supplement, never
disrupt** native shell workflows: ghost-text suggestions rendered inside ZLE, with
a Go daemon (`aftod`) providing history/frecency-ranked candidates over a unix
socket. It is a non-hijacking rethink of IRIS (a PTY-interposer tool that steals
TAB and other keys — the failure mode this project exists to avoid).

**Current stage:** design + validated PoC. There is no Go code yet; Phase 1 (the
daemon and production zsh plugin) is fully specified but not implemented.

## Where truth lives

- `DESIGN.md` — authoritative architecture: integration pattern, UI tiers,
  provider/plugin model, daemon design, decisions log (§0), and the
  non-disruption test checklist (§6). If implementation deviates from it, update
  it in the same commit.
- `plans/phase-1.md` — the complete Phase 1 implementation spec: wire protocol,
  SQLite schema, scoring formula, milestones, and acceptance gates. Follow it
  when building the daemon/plugin.
- `docs/protocol.md` — narrative wire-protocol reference: who the clients are,
  the keystroke round trip, why requests are JSON but ZLE responses are TSV,
  one-in-flight flow control, and protocol evolution rules.
- `poc/` — the validated Phase 0 proof of concept. **Do not modify**; it is the
  reference for the UI contract. Production shell code goes in `shell/zsh/`.
- `references/IRIS/` — read-only checkout of IRIS for study (gitignored, 0BSD
  licensed; its `internal/scoring` ideas are intentionally reusable). Never edit.

## The design contract (applies to all code, all phases)

Any change that touches shell integration must preserve these invariants
(rationale and full list in `DESIGN.md §2.4`, `plans/phase-1.md §2`):

1. TAB is never bound, wrapped, or observed — no `bindkey '\t'`, no wrapping of
   completion widgets, no `compdef`/`zstyle`. Gate:
   `grep -nE 'bindkey.*\\t|expand-or-complete|complete-word|menu-select|compdef|zstyle' <plugin>` must return nothing.
2. Suggestions render only via `$POSTDISPLAY` + `region_highlight` (display-only);
   `$BUFFER` is written only by explicit accept widgets, only with text already
   shown as ghost.
3. Prefix invariant, enforced client-side: display only candidates that strictly
   extend `$BUFFER`.
4. Failure mode is absence of suggestions — never prompt latency, never error
   output to the terminal. Nothing in the keystroke path may block or spawn
   subprocesses.
5. Go code: no cgo (SQLite via `modernc.org/sqlite`); dependency allowlist in
   `plans/phase-1.md §2.8`.

## Environment & commands

- Go 1.26.5 via `.tool-versions`; direnv `layout go` (`GOPATH` under `.direnv/`).
- Module path: `github.com/gataky/afto`.
- Build/test targets (`make build|test|e2e|bench|vet` → `bin/aftod`) are defined
  by `plans/phase-1.md §10 M1` and exist once Phase 1 lands.

Working checks today:

```zsh
zsh -n poc/afto.plugin.zsh          # syntax check
zsh -f -i -c 'source poc/afto.plugin.zsh && bindkey "^I"'   # must print expand-or-complete
```

## Testing approach

Shell behavior is verified end-to-end by driving an interactive `zsh -f -i` under
`zmodload zsh/zpty`: send keystrokes with `zpty -wn`, capture output, strip ANSI,
and assert. Ghost text is detected by its dim-color bytes (`\e[90m`); accept
correctness by suffixing `&& print MARKER` and asserting the marker executed. The
required scenarios (native TAB with ghost visible, accept-executes-what-was-shown,
daemon-kill resilience) are listed in `plans/phase-1.md §10 M7`, and every phase
must pass the manual checklist in `DESIGN.md §6` — most importantly the IRIS
regression: `vim .z<TAB>` must cycle hidden files natively.
