# afto

Shell suggestions for zsh that **supplement your workflow and never compete
with it**. Ghost text and a candidate list appear as you type, ranked by what
you actually run — in this directory, in this repo, after the command you just
finished.

TAB is never bound, wrapped, or observed. Neither is `^R`, `^T`, or `Alt+C`.
Your completion, your history search, and your fzf setup work exactly as they
did before you installed this.

```
$ git ch█eckout main              ← what you typed + dim ghost text
  ▸ git checkout main             ← the candidate list (passive; claims no keys)
    git cherry-pick abc123
    git checkout -b feature/x
```

Press `→` at the end of the line to accept. Press TAB and you get normal
completion, computed from the text *you* typed — never from the suggestion.

## Why another one of these

Tools in this space often work by launching your shell under a pseudo-terminal
and reading keystrokes before the shell sees them. That buys a pretty overlay
and costs you the shell: TAB gets intercepted, so `vim .z<TAB>` stops cycling
your dotfiles; the tool's idea of your command line drifts from reality the
moment you use vi-mode or a custom widget.

afto integrates *inside* the line editor instead. `$BUFFER` is zsh's, and afto
only reads it — writing only when you explicitly accept something that is
already on your screen. The full argument, including the root-cause analysis of
the tool this design reacts to, is in [DESIGN.md](DESIGN.md).

The practical consequence: **when afto fails, suggestions are absent.** Never a
hung prompt, never an error in your terminal, never a mangled line. Kill the
daemon mid-session and you will notice nothing except that ghost text stopped.

## Install

Requires zsh and Go 1.26+ (build only — the daemon is a single static binary,
no cgo, no SQLite to install).

```sh
git clone https://github.com/gataky/afto && cd afto
make build                      # → bin/aftod, bin/afto-make-targets
```

Put `bin/` on your `PATH` (the plugin starts the daemon itself, and needs to
find it), then source the plugin from your `.zshrc`:

```zsh
export PATH="$HOME/path/to/afto/bin:$PATH"
source ~/path/to/afto/shell/zsh/afto.plugin.zsh
```

Open a new shell and start typing. On first run afto imports your existing
`$HISTFILE`, so it is useful immediately rather than after a week of training.
The daemon starts on demand and shuts itself down once every terminal using it
has closed.

## Keys

| Key | What it does | What it did before |
|---|---|---|
| `→` at end of line | accept the whole suggestion | nothing (no-op) |
| `^]` | accept the whole suggestion | unbound |
| `Alt+f` at end of line | accept one word | nothing (no-op) |
| `^O` | open the menu (arrows navigate, `Enter` puts the pick on the line without running it, `Esc` leaves) | rarely bound |
| `^O` on an empty line | show what usually comes next after your last command | rarely bound |
| **TAB** | **your completion, untouched** | — |
| everything else | unchanged | — |

afto only claims keys that do nothing in that exact state, or that are unbound
by default — and every one of them is configurable. While the candidate list is
visible, no keys are claimed at all: TAB completes, arrows walk history, typing
refines the list.

Next-command prediction (`^O` on an empty line) is the one feature that cannot
be bootstrapped from your history file: which command *followed* which is only
knowable from commands afto watched you run, since a history file interleaves
every terminal you had open. It gets useful after a day of real use.

## Turning it off

```zsh
afto off              # unhook everything in this shell, right now
AFTO_DISABLE=1        # set before sourcing: the plugin is a no-op
afto status           # socket path, connection state, daemon version
```

With `AFTO_DISABLE=1`, `zle -l` and `bindkey` are byte-identical to a shell
that never loaded afto. That is checked by the test suite, not just claimed.

## Configuration

**In your shell** (set before sourcing the plugin):

| Variable | Default | Meaning |
|---|---|---|
| `AFTO_ROWS` | `4` | candidate rows below the ghost; `0` = ghost text only |
| `AFTO_EMPTY_ROWS` | `0` | rows shown unprompted on an empty line; `0` keeps a bare prompt bare |
| `AFTO_ACCEPT_KEY` | `^]` | accept key |
| `AFTO_MENU_KEY` | `^O` | menu key |
| `AFTO_HIGHLIGHT` | `fg=8` | ghost text style |
| `AFTO_HIGHLIGHT_ROW` / `AFTO_HIGHLIGHT_SELECTED` | `fg=8` / `standout` | row styles |
| `AFTO_CMD` | `aftod` | daemon binary to run |
| `AFTO_DEBUG` | unset | path to a client-side event trace (a file, never your terminal) |

**In `~/.config/afto/config.toml`** (every key optional; no file is a valid
setup). The latency budget, log level and redaction patterns take effect on the
next keystroke — no restart, no re-sourcing. Provider toggles and plugins are
wired at daemon start, so changing those needs a `kill aftod` (your next
keystroke starts a fresh one):

```toml
latency_budget_ms = 40      # providers race against this; the prompt never waits longer
idle_shutdown_min = 30
log_level = "info"

[providers]                 # all on by default
history = true
frecency = true
transition = true
alias_note = true

[project]                   # what marks a "project" for directory-affinity ranking
markers = [".git", ".hg", "go.mod", "package.json", "Cargo.toml", ".project-root"]

[redact]                    # extends the built-in secret patterns
extra_patterns = ['(?i)mycorp_token']
```

## Where things live

| | |
|---|---|
| Database | `~/.local/share/afto/afto.db` (or `$XDG_DATA_HOME/afto`) |
| Config | `~/.config/afto/config.toml` |
| Socket | `$XDG_RUNTIME_DIR/afto/afto.sock`, else `~/.cache/afto/afto.sock` |
| Log | `~/.local/state/afto/aftod.log` |

## Privacy

Everything is local: one SQLite file, one unix socket, no network code, no
telemetry, ever.

Secrets are **skipped before they are stored**, not masked at display time — a
masked command would only come back as a broken suggestion. Built-in patterns
cover AWS keys, GitHub and Slack tokens, `--password`/`--token` flags,
`export *_TOKEN=`, bearer headers, and commands you prefixed with a space to
keep out of history. Extend the list with `[redact] extra_patterns`.

## Extending it

**Plugins.** Anything that reads a JSON line on stdin and writes one back is a
suggestion source — no SDK, no linking, no language requirement. The shipped
example is fifteen lines of POSIX shell. A plugin cannot hurt your prompt: it
is bounded by a timeout, restarted with backoff if it crashes, and benched
entirely if it keeps failing. See [docs/plugins.md](docs/plugins.md).

```toml
[[plugin]]
name    = "make-targets"          # bin/afto-make-targets — suggests Makefile
command = "/path/to/afto-make-targets"   # targets from your current directory
```

**fzf.** Coexistence needs no setup at all. If you want afto's ranking behind a
picker, the plugin ships an `afto-fzf` widget bound to nothing — bind it
yourself, or pipe `aftod list` into whatever you like. See
[docs/fzf.md](docs/fzf.md).

```zsh
bindkey '^R' afto-fzf
```

## How it works

```
zsh ── ZLE hook reads $BUFFER, writes only $POSTDISPLAY ──┐
       accept widgets are the only path to $BUFFER        │ unix socket
                                                          ▼  (async, zle -F)
                                       aftod ── SQLite history + frecency
                                             ── providers raced against a
                                                latency budget, plus plugins
```

The keystroke path never blocks and never spawns a process. Requests go out
asynchronously, one at a time; answers are checked against the line as it is
*now* before anything is displayed. Suggestions live in `$POSTDISPLAY`, which
zsh treats as decoration — so a misbehaving provider (or a future AI one)
structurally cannot rewrite what you are about to run.

## Development

```sh
make build    # binaries → bin/
make test     # Go unit + integration tests
make vet
make e2e      # drives a real interactive zsh under zpty against a real daemon
make bench    # keystroke→ghost latency gate (p99 < 50 ms)
```

`make e2e` is the interesting one: it types into a live shell and asserts on the
bytes that reach the terminal, including the regression that motivated this
project — TAB completing a dotfile natively while ghost text is on screen.

## Documentation

- **[DESIGN.md](DESIGN.md)** — the architecture and, more importantly, the
  reasoning: the isolation guarantees, the accept-semantics contract, and the
  non-disruption checklist every change is held to.
- **[docs/protocol.md](docs/protocol.md)** — the shell↔daemon wire protocol.
- **[docs/plugins.md](docs/plugins.md)** — writing a plugin.
- **[docs/fzf.md](docs/fzf.md)** — fzf coexistence and the optional picker.
- **[plans/](plans/)** — per-phase implementation specs and completion reports.
  The reports are the honest record: what shipped, what deviated, and the
  hard-won ZLE and pty behaviors that are not discoverable from the code.

## Status

Phases 0–4 are complete: the daemon, the async zsh client, the native dropdown
and menu, project-affinity ranking with next-command prediction, and the plugin
host. zsh is the only shell client so far; bash and fish are Phase 5, and the
daemon has been kept shell-agnostic for exactly that reason.

No license file yet — if you plan to use or redistribute this, ask first.
