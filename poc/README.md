# afto PoC — install & non-disruption verification

## Try it (zsh)

```zsh
source poc/afto.plugin.zsh
```

Type a prefix of a command you've run before — the rest appears as dim ghost text.

- `→` at end of line, or `^]`: accept the whole suggestion
- `Alt+f` / `Alt+→` (forward-word) at end of line: accept one word
- `TAB`: your normal completion, always
- `afto off`: unhook everything at runtime
- `AFTO_DISABLE=1` before sourcing: no-op

## Try it (bash)

```bash
source poc/afto.bash
```

Nothing renders passively. Press `C-]` to extend the current line from history.

## Non-disruption checklist (run before/after any change)

With `zsh-syntax-highlighting` and `fzf-tab` also loaded:

| # | Action | Expected |
|---|--------|----------|
| 1 | `vim .z<TAB>` | Native completion cycles `.zshrc`/`.zshenv`/…; buffer never replaced |
| 2 | `cd /u/l/b<TAB>`, `ls *.md<TAB>` | Native path/glob expansion |
| 3 | `git ch<TAB><TAB>` | Native menu cycling |
| 4 | Ghost text visible, press `TAB` | Completion uses typed text only; ghost refreshes/clears |
| 5 | Ghost text visible, press `Enter` | Only typed text executes; no ghost in scrollback |
| 6 | `←` into the middle of the line, press `→` | Cursor moves natively; accept only fires at EOL |
| 7 | `Ctrl+R` isearch, bracketed paste, `Ctrl+L` | No ghost artifacts, no interference |
| 8 | Open/close vim, tmux | Clean redraws |
| 9 | `afto off` then repeat 1–3 | Identical to a shell that never loaded afto |

## Automated smoke test

```zsh
zsh -f -c 'source poc/afto.plugin.zsh 2>&1' && echo "loads clean"
# Verify we never touch TAB or completion machinery:
grep -nE 'bindkey.*\\t|expand-or-complete|complete-word|menu-select|compdef|zstyle' poc/afto.plugin.zsh && echo "VIOLATION" || echo "isolation OK"
```
