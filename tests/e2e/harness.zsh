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
#   * Strip ANSI before matching text; assert output markers with grep -x
#     (echoes of typed text would false-positive a substring match).

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
export HISTFILE=$D/seedhist
{
  print ": $EPOCHSECONDS:0;true afto-e2e-accept-target && print ACCEPT-SHOWN"
  print ": $EPOCHSECONDS:0;ls .zshrc-fantastic-history-suggestion"
} > $HISTFILE

mkdir -p $D/fix && touch $D/fix/.zshrc-fake   # fixture for the native-TAB test

typeset -g CAP="" FAILS=0
pass() { print -r -- "PASS: $1" }
fail() { print -r -- "FAIL: $1"; (( FAILS++ )) }
drain()    { local c i; for i in {1..40}; do zpty -r -t z c 2>/dev/null && CAP+=$c || sleep 0.03; done }
typeslow() { local ch; for ch in ${(s::)1}; do zpty -wn z $ch; sleep 0.05; done }
enter()    { zpty -wn z $'\r' }
stripped() { print -r -- $CAP | perl -pe 's/\e\[[0-9;?]*[a-zA-Z]//g; s/\r//g' }
count_line() { stripped | grep -cx -- $1 }
# File + slurp mode: robust against arbitrary terminal bytes and against
# the escape sequence spanning what perl -n would treat as separate lines.
seg_has_ghost() {
  print -rn -- ${CAP[$1,-1]} > $D/seg.tmp
  perl -0777 -ne 'exit(/\e\[90m/ ? 0 : 1)' $D/seg.tmp
}

zpty z zsh -f -i

# --- S0: plugin loads; daemon comes up ---------------------------------------
zpty -w z "source $PLUGIN && print LOADED-MARKER"
sleep 2.5; drain
if (( $(count_line LOADED-MARKER) == 1 )); then pass "plugin loads"; else fail "plugin loads"; fi
if $BIN ping --socket $AFTO_SOCKET >/dev/null 2>&1; then pass "daemon lazily spawned"; else fail "daemon lazily spawned"; fi

# --- S1: ghost text renders from imported history ------------------------------
mark=$(( ${#CAP} + 1 ))
typeslow "true afto-e2e-acc"
sleep 0.5; drain
if seg_has_ghost $mark; then pass "ghost text renders (dim bytes present)"; else fail "ghost text renders"; fi

# --- S2: ^] accept executes exactly what was displayed --------------------------
zpty -wn z $'\x1d'   # accept: buffer becomes the full displayed suggestion
sleep 0.3
typeslow " && print ACCEPT-OK"; enter
sleep 1; drain
# ACCEPT-SHOWN printing proves the *displayed* suggestion (not some other
# text) is what landed in the buffer and ran.
if (( $(count_line ACCEPT-SHOWN) == 1 )); then pass "accept executed the displayed suggestion"; else fail "accept executed the displayed suggestion"; fi
if (( $(count_line ACCEPT-OK) == 1 )); then pass "post-accept editing works"; else fail "post-accept editing works"; fi

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
# Completion must have used the FILESYSTEM (.zshrc-fake), not the ghost
# (.zshrc-fantastic-history-suggestion), and executing it lists the file.
if (( $(count_line .zshrc-fake) >= 1 )); then pass "TAB completed natively from filesystem"; else fail "TAB completed natively from filesystem"; fi
if stripped | grep -q "fantastic-history-suggestion\$"; then fail "ghost leaked into completion"; else pass "ghost did not leak into completion"; fi

# --- S4: daemon killed mid-session → typing unaffected, silence ------------------
pid=$(lsof -t $D/afto.sock.lock 2>/dev/null)
[[ -n $pid ]] && kill $pid && sleep 0.3
typeslow "print STILL-ALIVE-OK"; enter
sleep 1; drain
if (( $(count_line STILL-ALIVE-OK) == 1 )); then pass "shell fully functional after daemon kill"; else fail "shell fully functional after daemon kill"; fi

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
