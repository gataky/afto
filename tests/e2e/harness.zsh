#!/usr/bin/env zsh
# tests/e2e/harness.zsh — Phase 1 acceptance harness (plans/phase-1.md §10 M7).
#
# Drives a REAL interactive zsh under zpty, talking to a REAL daemon over a
# REAL socket — nothing is mocked. Run via `make e2e`.
#
# Hard-won zpty facts this harness encodes (violating any of them makes the
# tests silently meaningless — see the M6 debugging history):
#   * Type char-by-char with small delays. Bulk `zpty -w` writes never let
#     ZLE go idle between characters, so `zle -F` handlers cannot fire
#     mid-line and no ghost text will ever render.
#   * Enter is $'\r' on a pty. `zpty -w z ""` sends NOTHING — not a newline.
#   * Drain the pty continuously. A full pty output buffer wedges the child.
#   * ZLE specials ($BUFFER etc.) don't exist in fd handlers — the plugin
#     handles that internally, but it means assertions must come from the
#     rendered terminal bytes, not from poking shell state.
#   * Strip ANSI before matching text — and mind that with a multi-line
#     POSTDISPLAY (Phase 2 rows) ZLE repositions the cursor with escape
#     sequences instead of emitting "\n", so the FIRST line of a command's
#     output can merge into redraw bytes in the stripped stream. Markers
#     therefore (a) are written with a quote-split (print MAR''KER) so the
#     marker string can never occur in echoed/displayed text — only in
#     real output — and (b) are asserted end-anchored, not whole-line.

emulate -L zsh
zmodload zsh/zpty || { print "harness: zsh/zpty unavailable"; exit 2 }
zmodload zsh/datetime

REPO=${0:A:h:h:h}
BIN=$REPO/bin/aftod
PLUGIN=$REPO/shell/zsh/afto.plugin.zsh
[[ -x $BIN ]] || { print "harness: $BIN missing — run make build"; exit 2 }

# Short tmp root: unix socket paths are capped at 104 bytes on macOS.
D=$(mktemp -d /tmp/afto-e2e-XXXXXX) || exit 2

cleanup() {
  # Kill only OUR daemon: it holds an flock on the lock file, so lsof on
  # that file finds exactly its pid. Never pkill by name — the developer
  # may have a real aftod running.
  local pid=$(lsof -t $D/afto.sock.lock 2>/dev/null)
  [[ -n $pid ]] && kill $pid 2>/dev/null
  zpty -d z 2>/dev/null
  rm -rf $D
}
trap cleanup EXIT INT TERM

export AFTO_SOCKET=$D/afto.sock AFTO_DATA_DIR=$D/data AFTO_CONFIG=$D/cfg.toml
export XDG_STATE_HOME=$D/state AFTO_DEBUG=$D/client.trace
path=($REPO/bin $path)

# Seed suggestions via HISTFILE auto-import (runs on daemon start): this
# exercises the import path AND avoids having to execute weird commands
# just to get them into history.
# Seed commands print their markers through a quote-split so the marker
# appears in the transcript ONLY if the command actually executed — never
# from the candidate being displayed as ghost text or a list row.
# The menupick seeds are ranked deterministically for the menu scenarios:
# alpha ×3 (row 1, and most recent) beats beta ×1 (row 2) on frecency.
export HISTFILE=$D/seedhist
{
  print ": $EPOCHSECONDS:0;true afto-e2e-accept-target && print AC''CEPT-SHOWN"
  print ": $EPOCHSECONDS:0;ls .zshrc-fantastic-history-suggestion"
  print ": $(( EPOCHSECONDS - 40 )):0;true menupick-beta && print M''P-BETA"
  print ": $(( EPOCHSECONDS - 30 )):0;true menupick-alpha && print M''P-ALPHA"
  print ": $(( EPOCHSECONDS - 20 )):0;true menupick-alpha && print M''P-ALPHA"
  print ": $(( EPOCHSECONDS - 10 )):0;true menupick-alpha && print M''P-ALPHA"
} > $HISTFILE

mkdir -p $D/fix && touch $D/fix/.zshrc-fake   # fixture for the native-TAB test

typeset -g CAP="" FAILS=0
pass() { print -r -- "PASS: $1" }
fail() { print -r -- "FAIL: $1"; (( FAILS++ )) }
drain()    { local c i; for i in {1..40}; do zpty -r -t z c 2>/dev/null && CAP+=$c || sleep 0.03; done }
typeslow() { local ch; for ch in ${(s::)1}; do zpty -wn z $ch; sleep 0.05; done }
enter()    { zpty -wn z $'\r' }
stripped() { print -r -- $CAP | perl -pe 's/\e\[[0-9;?]*[a-zA-Z]//g; s/\r//g' }
# End-anchored on purpose: output can merge into a redraw line (see header),
# but nothing except real output ever ENDS in a quote-split marker.
count_marker() { stripped | grep -c -- "${1}\$" }
# File + slurp mode: robust against arbitrary terminal bytes and against
# the escape sequence spanning what perl -n would treat as separate lines.
seg_has_ghost() {
  print -rn -- ${CAP[$1,-1]} > $D/seg.tmp
  perl -0777 -ne 'exit(/\e\[90m/ ? 0 : 1)' $D/seg.tmp
}

zpty z zsh -f -i

# --- S0: plugin loads; daemon comes up ---------------------------------------
zpty -w z "source $PLUGIN && print LOADED''-MARKER"
sleep 2.5; drain
if (( $(count_marker LOADED-MARKER) == 1 )); then pass "plugin loads"; else fail "plugin loads"; fi
if $BIN ping --socket $AFTO_SOCKET >/dev/null 2>&1; then pass "daemon lazily spawned"; else fail "daemon lazily spawned"; fi

# --- S1: ghost text renders from imported history ------------------------------
mark=$(( ${#CAP} + 1 ))
typeslow "true afto-e2e-acc"
sleep 0.5; drain
if seg_has_ghost $mark; then pass "ghost text renders (dim bytes present)"; else fail "ghost text renders"; fi
# Tier 2: a ▸-marked list row renders. (Loose pattern on purpose: ZLE
# repaints only the columns that changed, so as candidates swap while the
# prefix is typed, a row's full text is rarely contiguous in the byte
# stream — only the earliest full paint is. S8 asserts exact row content
# on a prefix whose top candidate never changes.)
if stripped | grep -q -- "▸ true"; then pass "passive list row renders"; else fail "passive list row renders"; fi

# --- S2: ^] accept executes exactly what was displayed --------------------------
zpty -wn z $'\x1d'   # accept: buffer becomes the full displayed suggestion
sleep 0.3
typeslow " && print ACC''EPT-OK"; enter
sleep 1; drain
# ACCEPT-SHOWN printing proves the *displayed* suggestion (not some other
# text) is what landed in the buffer and ran.
if (( $(count_marker ACCEPT-SHOWN) == 1 )); then pass "accept executed the displayed suggestion"; else fail "accept executed the displayed suggestion"; fi
if (( $(count_marker ACCEPT-OK) == 1 )); then pass "post-accept editing works"; else fail "post-accept editing works"; fi

# --- S3: THE IRIS BUG — native TAB with ghost visible ----------------------------
zpty -w z "cd $D/fix"
sleep 0.5; drain
mark=$(( ${#CAP} + 1 ))
typeslow "ls .zshrc-f"
sleep 0.5; drain
if seg_has_ghost $mark; then pass "ghost visible before TAB"; else fail "ghost visible before TAB"; fi
zpty -wn z $'\t'     # native expand-or-complete must win
sleep 0.5
enter
sleep 1; drain
# Completion must have used the FILESYSTEM: the buffer becomes .zshrc-fake
# and executing it succeeds. Had TAB done nothing (ls .zshrc-f) or had the
# ghost/row text leaked into the buffer (.zshrc-fantastic-…), the executed
# ls would print "No such file" — which S5's noise gate also catches, but
# assert it here where the failure would be introduced. (The Phase 1
# line-end grep for the ghost text is invalid now: with AFTO_ROWS > 0 the
# suggestion legitimately appears in the transcript as a list row.)
if stripped | grep -q '\.zshrc-fake'; then pass "TAB completed natively from filesystem"; else fail "TAB completed natively from filesystem"; fi
if stripped | grep -qi "no such file"; then fail "ghost leaked into completion"; else pass "ghost did not leak into completion"; fi

# --- S8: passive rows rank deterministically; arrows stay native outside menu ----
zpty -w z "cd $D"           # leave the fixture dir; irrelevant for these
sleep 0.5; drain
typeslow "true menupick-"
sleep 0.6; drain
if stripped | grep -q -- "▸ true menupick-alpha"; then pass "row 1 is the top candidate (▸ alpha)"; else fail "row 1 is the top candidate (▸ alpha)"; fi
if stripped | grep -q -- "    true menupick-beta"; then pass "row 2 renders unselected (beta)"; else fail "row 2 renders unselected (beta)"; fi
# With the list visible but NO ^O, ↓ must be native history movement (a
# no-op on a freshly typed line), never a selection move: Enter must run
# the typed text alone, not a row.
zpty -wn z $'\e[B'
sleep 0.3
enter
sleep 0.8; drain
if (( $(count_marker MP-ALPHA) == 0 && $(count_marker MP-BETA) == 0 )); then
  pass "arrow outside menu claimed nothing (typed text ran)"
else
  fail "arrow outside menu claimed nothing (typed text ran)"
fi

# --- S9: menu mode — ^O enters, ↓ selects row 2, Enter accepts WITHOUT executing --
typeslow "true menupick-"
sleep 0.6; drain
zpty -wn z $'\x0f'          # ^O: explicit menu entry
sleep 0.3
zpty -wn z $'\e[B'          # ↓: selection moves to row 2 (beta)
sleep 0.3; drain
if stripped | grep -q -- "▸ true menupick-beta"; then pass "menu ↓ moved the selection to row 2"; else fail "menu ↓ moved the selection to row 2"; fi
enter                       # menu accept: buffer ← row 2, NOT executed
sleep 0.5; drain
if (( $(count_marker MP-BETA) == 0 )); then pass "menu Enter accepted without executing"; else fail "menu Enter accepted without executing"; fi
enter                       # plain Enter at the prompt: NOW it runs
sleep 0.8; drain
if (( $(count_marker MP-BETA) == 1 )); then pass "accepted pick executes exactly (row 2, not row 1)"; else fail "accepted pick executes exactly (row 2, not row 1)"; fi
if (( $(count_marker MP-ALPHA) == 0 )); then pass "unselected row never executed"; else fail "unselected row never executed"; fi

# --- S10: Esc exits the menu; subsequent typing is native ---------------------------
typeslow "true menupick-"
sleep 0.6
zpty -wn z $'\x0f'          # enter menu
sleep 0.3
zpty -wn z $'\e'            # Esc: back to passive (KEYTIMEOUT disambiguates)
sleep 0.5
typeslow "gamma && print E''SC-OK"
enter
sleep 0.8; drain
# The client trace's record line carries the exact executed command —
# the one full-fidelity source for "every typed char landed in the
# buffer" (screen bytes are partial repaints; see S1's comment). Had Esc
# replayed itself, \e would have entered vicmd / eaten the next key and
# this line could not have executed intact.
if grep -q "record.*menupick-gamma && print" $D/client.trace && (( $(count_marker ESC-OK) == 1 )); then
  pass "Esc exited menu; typing self-inserted natively"
else
  fail "Esc exited menu; typing self-inserted natively"
fi

# --- S11: any other key exits the menu and replays natively -------------------------
typeslow "true menupick-"
sleep 0.6
zpty -wn z $'\x0f'          # enter menu
sleep 0.3
typeslow "xyz && print O''THER-OK"   # 'x' must exit AND self-insert
enter
sleep 0.8; drain
# record proves the buffer ended "…menupick-xyz…": the 'x' that exited
# the menu also self-inserted (a swallowed key would leave "…menupick-yz").
if grep -q "record.*menupick-xyz && print" $D/client.trace && (( $(count_marker OTHER-OK) == 1 )); then
  pass "other key exited menu and self-inserted"
else
  fail "other key exited menu and self-inserted"
fi
# ^O with nothing to show must be a silent no-op.
zpty -wn z $'\x0f'
sleep 0.3
typeslow "print N''OOP-OK"; enter
sleep 0.8; drain
if (( $(count_marker NOOP-OK) == 1 )); then pass "^O without rows is a silent no-op"; else fail "^O without rows is a silent no-op"; fi

# --- S4: daemon killed mid-session → typing unaffected, silence ------------------
pid=$(lsof -t $D/afto.sock.lock 2>/dev/null)
[[ -n $pid ]] && kill $pid && sleep 0.3
typeslow "print STILL''-ALIVE-OK"; enter
sleep 1; drain
if (( $(count_marker STILL-ALIVE-OK) == 1 )); then pass "shell fully functional after daemon kill"; else fail "shell fully functional after daemon kill"; fi

# --- S5: no error noise anywhere in the session -----------------------------------
if stripped | grep -qiE "error|failed|no such|command not found|broken pipe"; then
  fail "session free of error noise"
else
  pass "session free of error noise"
fi

# --- S6: isolation grep (constraint: TAB/compsys untouched) ------------------------
if grep -qE 'bindkey.*\\t|expand-or-complete|complete-word|menu-select|compdef|zstyle' $PLUGIN; then
  fail "isolation grep clean"
else
  pass "isolation grep clean"
fi

# --- S12: coexistence — fzf/zsh-syntax-highlighting loaded alongside afto ----------
# DESIGN.md §6 wants the checklist run with these loaded. Each tool is
# gated on availability so the harness stays green on machines without
# them; a skip is reported, never silent.
typeset -g ZSYH=""
for p in /opt/homebrew/share/zsh-syntax-highlighting/zsh-syntax-highlighting.zsh \
         /usr/local/share/zsh-syntax-highlighting/zsh-syntax-highlighting.zsh \
         /usr/share/zsh/plugins/zsh-syntax-highlighting/zsh-syntax-highlighting.zsh; do
  [[ -r $p ]] && { ZSYH=$p; break }
done
HAVE_FZF=""
command -v fzf >/dev/null && fzf --zsh >/dev/null 2>&1 && HAVE_FZF=1

typeset -g CAP2=""
drain2()    { local c i; for i in {1..40}; do zpty -r -t z2 c 2>/dev/null && CAP2+=$c || sleep 0.03; done }
typeslow2() { local ch; for ch in ${(s::)1}; do zpty -wn z2 $ch; sleep 0.05; done }
stripped2() { print -r -- $CAP2 | perl -pe 's/\e\[[0-9;?]*[a-zA-Z]//g; s/\r//g' }
count_marker2() { stripped2 | grep -c -- "${1}\$" }

load="source $PLUGIN"
[[ -n $HAVE_FZF ]] && load="source <(fzf --zsh); $load"
[[ -n $ZSYH ]] && load="$load; source $ZSYH"   # z-sy-h wants to load last
zpty z2 zsh -f -i
zpty -w z2 "$load; print CO''EX-LOADED"
sleep 2.5; drain2
if (( $(count_marker2 COEX-LOADED) == 1 )); then pass "coexistence stack loads (fzf=${HAVE_FZF:-0} z-sy-h=${${ZSYH:+1}:-0})"; else fail "coexistence stack loads"; fi

mark2=$(( ${#CAP2} + 1 ))
typeslow2 "true menupick-al"
sleep 0.6; drain2
print -rn -- ${CAP2[$mark2,-1]} > $D/seg2.tmp
if perl -0777 -ne 'exit(/\e\[90m/ ? 0 : 1)' $D/seg2.tmp; then pass "ghost renders under coexistence stack"; else fail "ghost renders under coexistence stack"; fi
zpty -wn z2 $'\x1d'   # ^] accept
sleep 0.3
enter2() { zpty -wn z2 $'\r' }
enter2
sleep 0.8; drain2
if (( $(count_marker2 MP-ALPHA) == 1 )); then pass "accept works under coexistence stack"; else fail "accept works under coexistence stack"; fi

if [[ -n $HAVE_FZF ]]; then
  # afto must not have claimed fzf's keys (DESIGN.md §2.3).
  zpty -w z2 "bindkey '^T' | grep -q fzf-file-widget && bindkey '^R' | grep -q fzf-history-widget && print FZ''F-INTACT"
  sleep 0.8; drain2
  if (( $(count_marker2 FZF-INTACT) == 1 )); then pass "fzf ^T/^R bindings intact"; else fail "fzf ^T/^R bindings intact"; fi
else
  print "SKIP: fzf not installed — ^T/^R coexistence not exercised"
fi
[[ -z $ZSYH ]] && print "SKIP: zsh-syntax-highlighting not installed — region_highlight coexistence not exercised"
zpty -d z2 2>/dev/null

# --- S7: AFTO_DISABLE=1 leaves the shell untouched ---------------------------------
probe='zle -l; bindkey; add-zle-hook-widget -L 2>/dev/null; add-zsh-hook -L 2>/dev/null'
vanilla=$(zsh -f -i -c $probe 2>/dev/null)
disabled=$(AFTO_DISABLE=1 zsh -f -i -c "source $PLUGIN; $probe" 2>/dev/null)
if [[ $vanilla == $disabled ]]; then pass "AFTO_DISABLE=1 is a perfect no-op"; else fail "AFTO_DISABLE=1 is a perfect no-op"; fi

print
if (( FAILS )); then
  # Preserve evidence beyond the trap's rm -rf.
  print -r -- $CAP > /tmp/afto-e2e-failure.raw
  cp $D/client.trace /tmp/afto-e2e-failure.trace 2>/dev/null
  print "harness: $FAILS FAILURE(S)"
  print "  transcript: /tmp/afto-e2e-failure.raw   trace: /tmp/afto-e2e-failure.trace"
  exit 1
fi
print "harness: all scenarios passed"
exit 0
