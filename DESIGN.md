# afto — Architectural Design

A shell suggestion tool that **supplements** interactive workflows and never competes
with native completion. This document defines the integration pattern, the isolation
guarantees, the engine/plugin architecture, and the phased implementation plan.

## 0. Decisions log

| Decision | Choice | Rationale |
|---|---|---|
| Shell scope | zsh first; daemon stays shell-agnostic | Full experience needs ZLE; bash/fish clients can come later without rework |
| Menu UI | Native ZLE dropdown (IRIS-style) | Preferred aesthetic; fzf coexists untouched regardless |
| AI | Not now; must be trivially addable | AI = just another provider behind the standard interface, off by default |
| Daemon language | Go | Single static binary; can adapt IRIS scoring ideas (0BSD) |
| Plugin system | Subprocess plugins, JSON-lines over stdio | Language-agnostic, no version-lock, same protocol as shell client |
| IRIS features kept | cwd-aware ranking, command-transition prediction, alias expansion *preview* | Command specs (Fig-style flag DB) rejected — overlaps TAB's job |
| SSH/remote | Local-only for now | Keep door open: static binary, no root, XDG paths |

## 1. Why IRIS-style tools disrupt workflows (root-cause analysis)

IRIS (`references/IRIS`) is a **PTY interposer**: it launches your shell under a
pseudo-terminal (`pty.Start` in `root/wrapper.go`), puts the real terminal in raw mode,
and inspects every keystroke before the shell ever receives it. Three consequences:

1. **TAB is stolen.** `wrapper.go:906` marks `0x09` as `intercepted` unconditionally.
   When the overlay is visible, TAB sends `Ctrl+U` (kill line) + the selected
   suggestion into the shell. Native compsys never runs — the `vim .z<TAB>` bug.
2. **Shadow-buffer drift.** IRIS reconstructs the command line by parsing raw stdin
   bytes into a `naiveBuffer`. Any editing operation it doesn't model — vi-mode,
   kill ring, multibyte input, custom widgets — desynchronizes it from ZLE.
3. **Rendering races.** The overlay and the shell both write to the terminal,
   requiring crash/rescue-shell machinery.

**Design axiom:** integrate *inside* the line editor (ZLE), never *in front of* it.
`$BUFFER` is the single source of truth; we only read it, and we write it only on an
explicit, user-initiated accept action. IRIS's sin isn't the dropdown UI — it's
stealing keys to drive it. Passive *display* is safe; only *navigation* needs keys,
and those activate only after explicit menu entry.

## 2. Integration pattern (zsh)

```
┌────────────────────────── zsh process ──────────────────────────┐
│  ZLE (owns $BUFFER, $CURSOR — authoritative, no shadow copy)    │
│                                                                 │
│  line-pre-redraw ──► _afto_suggest()      (read-only)           │
│        │  reads $BUFFER/$CURSOR/$CONTEXT                        │
│        ▼  writes ONLY $POSTDISPLAY + region_highlight           │
│  ┌───────────────────────────────────────────────┐              │
│  │ tier 1: inline ghost text        (passive)    │              │
│  │ tier 2: candidate list rows      (passive)    │              │
│  │ tier 3: menu-mode navigation     (explicit)   │              │
│  └───────────────────────────────────────────────┘              │
│                                                                 │
│  accept widgets (explicit user action only):                    │
│    • forward-char at end-of-line  (native no-op → safe reuse)   │
│    • dedicated key (default ^], configurable)                   │
│    • partial accept: forward-word at EOL (one word)             │
│    • menu mode entry: dedicated key (default ^O, configurable)  │
│                                                                 │
│  TAB: ***never bound, never wrapped, never observed***          │
└──────────────┬──────────────────────────────────────────────────┘
               │ async: zle -F on a unix-socket fd
               ▼
        aftod suggestion daemon (Go, shell-agnostic)
```

### 2.1 UI tiers

**Tier 1 — ghost text (validated in the PoC).** The top candidate's remainder renders
dim (`region_highlight fg=8`) in `$POSTDISPLAY`. Display-only; structurally cannot
corrupt the buffer.

**Tier 2 — passive candidate list (the IRIS look).** `$POSTDISPLAY` may contain
newlines, and `region_highlight` spans it, so we render the top N candidates as styled
rows below the prompt:

```
$ git ch█eckout main              ← buffer + dim ghost
  ▸ git checkout main             ← selected row (highlighted)
    git cherry-pick abc123
    git checkout -b feature/x
```

While the list is visible, **no keys are claimed**: TAB completes natively, arrows do
history, typing refines the list. Config: `menu.passive_rows = 0..10` (0 = ghost-only).
Known tradeoffs vs IRIS: rows scroll the screen at the bottom edge (no floating
overlay), and styling is color/bold rather than icon fonts. True floating panels
(kitty/wezterm protocols) are a future plugin, not core.

**Tier 3 — menu mode (explicit entry only).** A dedicated key (default `^O`) enters a
custom keymap, exactly how zsh's own `menuselect` works: `↑/↓` move the selection,
`Enter` accepts into the buffer (does **not** execute), `Esc` exits, any other key
exits and self-inserts. Because the keymap exists only after explicit entry, prompt-
level arrows/TAB are never touched.

### 2.2 Accept semantics (the "zero hijacking" contract)

| Key | Native behavior | afto behavior |
|---|---|---|
| `TAB` | compsys / fzf-tab / menu cycling | **untouched — not bound, not wrapped** |
| `→` mid-line | move right | unchanged (native) |
| `→` at EOL, ghost visible | no-op | accept full suggestion |
| `^]` (configurable) | unbound in emacs keymap | accept full suggestion |
| forward-word at EOL, ghost visible | no-op | accept next word |
| `^O` (configurable) | unbound-ish (`operate-and-get-next` is rare) | enter menu mode |
| `Enter` | accept line | unchanged; ghost/list cleared, only `$BUFFER` executes |
| everything else | native | native |

Rule: afto may only claim key events that are **native no-ops in that state** or keys
that are **unbound/rare by default** — and every binding is configurable. `Alt+f` is
not free (emacs `forward-word`); it participates only via the wrap-at-EOL rule.

### 2.3 fzf policy: coexistence guaranteed, integration optional

- **Coexistence is free.** afto never binds `^T`, `^R`, or `Alt+C`. fzf widgets edit
  `$BUFFER` directly; our `line-pre-redraw` hook just observes the result like any
  other edit (and will happily ghost-suggest on top of an fzf-inserted path).
- **Integration (optional widget, post-Phase 2):** `aftod query --list | fzf` →
  insert pick into `$BUFFER`. Later: feed afto's frecency-ranked history to fzf's
  `^R` for users who prefer that picker. Both are additive opt-ins.

### 2.4 Isolation guarantees

1. **No TAB involvement**: no `bindkey '\t'`, no wrapping of `expand-or-complete` /
   `complete-word` / `menu-select`, no `compdef`, no `zstyle` writes. Grep-testable
   (see `poc/README.md`).
2. **Display-only rendering**: suggestions live exclusively in `$POSTDISPLAY`.
   `$BUFFER` is written only by accept widgets, only with text already shown.
3. **Prefix invariant (client-enforced)**: display only if
   `candidate == $BUFFER + extension`. Non-extensions are dropped — a misbehaving
   provider (or future AI) structurally cannot rewrite the line.
   Exception: tier-2/3 list rows may show non-prefix candidates (e.g. transition
   predictions on an empty prompt), but they enter the buffer only via explicit
   menu-mode accept, never via ghost-accept keys.
4. **Staleness check**: async responses carry the buffer they were computed for;
   discard if `$BUFFER` changed.
5. **Context guards**: suggest only when `$CONTEXT == start` — never in isearch,
   `vared`, completion menus, or PS2.
6. **Kill switch**: `AFTO_DISABLE=1` or `afto off` unhooks everything at runtime.

## 3. Daemon architecture (`aftod`, Go)

Single static binary (pure-Go SQLite via `modernc.org/sqlite`, no cgo). Per-user,
socket-activated on first query. Socket: `$XDG_RUNTIME_DIR/afto/afto.sock`
(fallback `~/.cache/afto/`), dir mode 0700. Config: `~/.config/afto/config.toml`,
hot-reloaded via file watch (no SIGUSR1/exec dance like IRIS).

### 3.1 Provider model — AI-ready without AI

```go
type Provider interface {
    Name() string
    Suggest(ctx context.Context, q Query) ([]Candidate, error)
}

type Query struct {         // already carries everything an AI provider would need
    V        int
    Buffer   string
    Cursor   int
    CWD      string
    LastExit int
    Session  string
    Recent   []string
}

type Candidate struct {
    Text   string  // must extend Buffer to be ghost-eligible (client re-verifies)
    Score  float64
    Source string  // "history" | "frecency" | "transition" | plugin name | ...
    Note   string  // optional annotation, e.g. alias expansion preview
}
```

- **Race with deadline:** all enabled providers run concurrently against a latency
  budget (default 50 ms). Ghost text renders from whoever answered in time;
  stragglers are dropped for this keystroke. A slow provider degrades itself, never
  the prompt.
- **Merge:** score-weighted, source-diverse (don't let one provider fill all rows).
- **Built-in providers (roadmap order):**
  1. `history` — prefix match over imported + live-ingested history.
  2. `frecency` — frequency × recency × **cwd/project affinity** (rank by what you
     actually run in this repo). SQLite store; scoring ideas adapted from IRIS
     `internal/scoring` (0BSD).
  3. `transition` — next-command prediction from an empty prompt
     (`git add …` → suggest `git commit`), from recorded command-pair stats.
  4. `alias-note` — annotates candidates with their alias expansion as a dim note
     in the list (informational only; never rewrites the buffer — the disruptive
     IRIS auto-expand-on-space is explicitly rejected).
  5. `ai` — **future, off by default.** Same interface, same prefix invariant,
     debounce + cache; adding it is config + one struct, no architecture change.
- **Explicitly out of scope:** Fig-style command-spec database (subcommand/flag
  knowledge). That's TAB's job; we defer to compsys.

### 3.2 Plugin system — one protocol everywhere

External plugins are **subprocesses speaking the same JSON-lines protocol** as the
shell client, over stdio:

```toml
[[plugin]]
name    = "kubectx-suggest"
command = "/usr/local/bin/afto-kubectx"
timeout_ms = 40
```

- Request/response are the `Query`/`Candidate` JSON shapes above, one JSON object
  per line, with `v` for protocol versioning.
- Long-lived process, restarted with backoff on crash; **circuit breaker** (repeated
  timeouts → plugin benched, prompt never blocks).
- Why not alternatives: Go's stdlib `plugin` package is version-locked and brittle;
  hashicorp/go-plugin (gRPC) is solid but heavy for line-of-text suggestions.
  Subprocess-JSON means a plugin can be a 10-line Python script, and the shell
  client is effectively "plugin #0."

### 3.3 Data layer

- **Bootstrap import:** first run ingests existing `$HISTFILE` (extended-history
  format aware) so afto is useful in minute one. Dedupe on import.
- **Live ingestion:** `precmd` hook fire-and-forgets
  `{cmd, exit_code, cwd, session, ts}` to the daemon. Never blocks the prompt.
- **Secrets redaction — before persistence, not at display:** built-in patterns
  (AWS keys, bearer tokens, `--password …`, `export *TOKEN=…`) plus user-extendable
  redact list in config. `HISTIGNORE`-style exclusions honored.
- **Multi-session:** one SQLite DB (WAL mode) shared across terminals; `Recent`
  context is per-session (keyed by `Session` in the schema from day one).
- **Privacy:** local-only storage, no telemetry, ever.

### 3.4 Shell↔daemon transport

Unix socket, JSON lines. The zsh client connects via `zmodload zsh/net/socket`
(`zsocket`), registers the fd with `zle -F fd handler`; ZLE invokes the handler when
the response arrives — typing never blocks. If the daemon is down/slow: silently no
suggestions (and an attempt to socket-activate for next time). Failure mode is
"absence of ghost text," never latency.

## 4. Repository layout

```
afto/
├── DESIGN.md
├── poc/                        # Phase 0 — validated UI contract (done)
│   ├── afto.plugin.zsh
│   ├── afto.bash
│   └── README.md
├── daemon/                     # Go: aftod
│   ├── cmd/aftod/
│   └── internal/
│       ├── ipc/                # socket server, JSON-lines codec, protocol ver
│       ├── store/              # sqlite, import, redaction
│       ├── scoring/            # frecency, cwd affinity, transitions
│       ├── provider/           # Provider iface, race/merge, builtins
│       └── plugin/             # subprocess host, circuit breaker
└── shell/
    └── zsh/afto.plugin.zsh     # production plugin (async client + menu UI)
```

## 5. Implementation phases

- **Phase 0 — PoC (done):** UI contract validated under a scripted PTY: ghost text
  via `POSTDISPLAY`, accept at EOL/`^]`, TAB native throughout (`vim .z<TAB>` passes),
  isolation grep clean.
- **Phase 1 — Daemon + async client:** `aftod` with `history` + `frecency`
  providers, HISTFILE import, redaction, `precmd` ingestion; zsh plugin switches to
  async socket fetch (`zle -F`) with staleness checks. Ghost text only. Exit
  criteria: p99 keystroke→ghost < 50 ms; non-disruption checklist passes.
- **Phase 2 — Native dropdown:** passive tier-2 list (multi-line `POSTDISPLAY` +
  `region_highlight` rows) and tier-3 menu mode (custom keymap via `^O`). Exit
  criteria: checklist passes *with the list visible*; menu mode never activates
  without explicit entry.
- **Phase 3 — Context intelligence:** cwd/project-affinity ranking, `transition`
  provider (empty-prompt next-command rows in the menu), alias-expansion notes.
- **Phase 4 — Plugin host + fzf widget:** subprocess plugin runtime with circuit
  breaker + a sample plugin; optional `aftod query --list | fzf` widget and
  frecency-fed `^R` alternative.
- **Phase 5 — Breadth (as desired):** bash explicit-invoke client wired to the
  daemon, ble.sh source, fish autosuggestion source, vi-mode polish, `ai` provider
  behind explicit opt-in.

## 6. Non-disruption test checklist (regression gate, every phase)

Run with `zsh-syntax-highlighting` and `fzf` (incl. `fzf-tab`) loaded:

1. `vim .z<TAB>` → native completion cycles `.zshrc`, `.zshenv`, …; buffer never
   replaced. **(The IRIS bug.)**
2. `cd /u/l/b<TAB>`, `ls *.md<TAB>` → native path/glob expansion.
3. `git ch<TAB><TAB>` → native menu cycling; repeat with passive list visible.
4. Ghost text + list visible, press `TAB` → completion uses typed text only.
5. `Enter` with ghost/list visible → only typed text executes; scrollback clean.
6. Mid-line edits → `→` moves natively; accept fires only at EOL.
7. `^T` fzf file widget, `^R` fzf history, `Alt+C` → fully native; afto suggests
   normally on the buffer fzf produces.
8. `^O` menu: arrows navigate *only inside* menu mode; `Esc`/other-key exits cleanly.
9. Ctrl+R isearch, bracketed paste, `^L`, vim/tmux round-trips → no artifacts.
10. Daemon stopped (`kill aftod`) → typing latency unchanged, no errors, no ghost.
11. `afto off` / `AFTO_DISABLE=1` → indistinguishable from never loading afto.
