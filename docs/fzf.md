# afto and fzf

## Coexistence needs no configuration

afto never binds `^T`, `^R`, or `Alt+C`. Load both and they simply work:
fzf's widgets edit `$BUFFER` directly, and afto's `line-pre-redraw` hook
observes the result like any other edit — it will happily ghost-suggest on
top of a path fzf just inserted. `fzf-tab` is likewise untouched, because
afto never binds or wraps TAB.

This is not an integration. It is the absence of a conflict, and it is the
default.

## Optional: afto's ranking behind your picker

Everything below is opt-in. Nothing here is enabled by loading afto.

### `aftod list` — frecency-ranked history, one per line

```sh
aftod list                      # whole history, best-ranked first
aftod list --prefix "git "      # only commands starting with "git "
aftod list --limit 200
aftod list --cwd /w/proj        # rank as if standing in that directory
```

Ranking is the same frecency model the ghost text uses: frequency and
recency, boosted for the current directory and for the enclosing project.
So `make test` surfaces near the top in the repo where you run it, and sinks
elsewhere.

`aftod list` reads the store **directly**, which has two consequences worth
knowing: it works when no daemon is running, and it is a snapshot — commands
recorded a moment ago by a live daemon are included, since both use the same
SQLite database in WAL mode.

### Why not `aftod query --list`?

`query` goes through the running daemon and its full provider stack —
including plugins — but an *empty* buffer means "predict what comes next"
there, not "give me everything". That is right for ghost text and wrong for
a fuzzy finder. Use `query --list` when you want the live provider stack for
a specific prefix:

```sh
aftod query --list --buffer "git c" --limit 10
```

and `list` when you want a history picker.

### The `afto-fzf` widget

The plugin defines a widget called `afto-fzf` and **binds it to nothing**.
To use it, bind it yourself:

```zsh
bindkey '^R' afto-fzf     # replace fzf's history widget with afto's ranking
bindkey '^X^R' afto-fzf   # or keep both
```

It pipes `aftod list --prefix "$LBUFFER"` into fzf, seeded with what you have
already typed, and replaces the line with your pick (it does not execute it —
press Enter yourself). If fzf is not installed the widget does nothing.

Yes, this forks. That is fine: it runs only when you press its key. The
keystroke path — the hooks that produce ghost text — remains fork-free, which
is the property that matters for prompt latency.

### Piping it somewhere else

`aftod list` is just lines on stdout, so any picker works:

```zsh
# skim
aftod list | sk
# a plain menu
aftod list --limit 20 | nl
```
