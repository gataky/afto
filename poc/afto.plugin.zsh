# afto.plugin.zsh — Phase 0 proof-of-concept
#
# Non-disruptive ghost-text suggestions for zsh.
# Contract:
#   * TAB is never bound, wrapped, or observed. compsys/fzf-tab own it entirely.
#   * Suggestions render only in $POSTDISPLAY (display-only, never part of $BUFFER).
#   * $BUFFER is written in exactly one place: the accept widgets, on explicit
#     user action, and only ever by appending the visible ghost text.
#   * A suggestion is shown only if it is a strict prefix-extension of $BUFFER.
#
# Requires zsh >= 5.3 (add-zle-hook-widget).
# Engine: synchronous most-recent-history prefix match (swapped for the aftod
# socket client in Phase 1 — only _afto_engine changes).

[[ -n $AFTO_DISABLE ]] && return 0
zmodload zsh/parameter 2>/dev/null || return 0
autoload -Uz add-zle-hook-widget || return 0

: ${AFTO_HIGHLIGHT:="fg=8"}      # ghost text style (dim); provenance is always visible
: ${AFTO_ACCEPT_KEY:="^]"}       # dedicated accept key; ^] is unbound in emacs keymap

typeset -g _afto_hl_entry=""     # the region_highlight entry we own, for clean removal

# --- rendering -------------------------------------------------------------

_afto_clear_ghost() {
  POSTDISPLAY=""
  if [[ -n $_afto_hl_entry ]]; then
    region_highlight=(${region_highlight:#$_afto_hl_entry})
    _afto_hl_entry=""
  fi
}

_afto_show_ghost() {
  local suggestion=$1
  _afto_clear_ghost
  # Prefix invariant: only display strict extensions of what the user typed.
  [[ -n $suggestion && $suggestion == ${BUFFER}?* ]] || return 0
  POSTDISPLAY=${suggestion#$BUFFER}
  _afto_hl_entry="${#BUFFER} $(( ${#BUFFER} + ${#POSTDISPLAY} )) $AFTO_HIGHLIGHT"
  region_highlight+=($_afto_hl_entry)
}

# --- engine (Phase 0: sync history lookup; Phase 1: async socket via zle -F) --

_afto_engine() {
  # Most recent history entry starting with $BUFFER. (b) escapes pattern chars.
  typeset -g _afto_reply=${history[(r)${(b)BUFFER}*]}
}

# --- refresh hook (read-only w.r.t. the buffer) ------------------------------

_afto_suggest() {
  emulate -L zsh
  # Only at a normal top-level prompt: never in isearch, vared, menus, PS2.
  if [[ $CONTEXT != start || -z $BUFFER || $BUFFER == *$'\n'* ]]; then
    _afto_clear_ghost
    return 0
  fi
  _afto_engine
  _afto_show_ghost "$_afto_reply"
}

_afto_line_finish() {
  # Enter executes only $BUFFER regardless, but clear the ghost so it doesn't
  # linger on the accepted line in scrollback.
  emulate -L zsh
  _afto_clear_ghost
}

add-zle-hook-widget line-pre-redraw _afto_suggest
add-zle-hook-widget line-finish _afto_line_finish

# --- accept widgets (the ONLY code paths that write $BUFFER) -----------------

_afto_accept_full() {
  emulate -L zsh
  BUFFER+=$POSTDISPLAY
  _afto_clear_ghost
  CURSOR=${#BUFFER}
}

# forward-char: native everywhere except EOL-with-ghost, where native is a no-op.
_afto_forward_char() {
  if (( CURSOR == ${#BUFFER} )) && [[ -n $POSTDISPLAY ]]; then
    _afto_accept_full
  else
    zle .forward-char
  fi
}

# forward-word at EOL with ghost: accept one word of the suggestion.
_afto_forward_word() {
  if (( CURSOR == ${#BUFFER} )) && [[ -n $POSTDISPLAY ]]; then
    local ghost=$POSTDISPLAY word
    # first word plus its leading whitespace
    word=${ghost%%${ghost##[[:space:]]#[^[:space:]]#}}
    [[ -z $word ]] && word=$ghost
    local rest=${ghost#$word}
    BUFFER+=$word
    _afto_clear_ghost
    CURSOR=${#BUFFER}
    [[ -n $rest ]] && _afto_show_ghost "${BUFFER}${rest}"
  else
    zle .forward-word
  fi
}

zle -N afto-accept _afto_accept_full
zle -N forward-char _afto_forward_char
zle -N forward-word _afto_forward_word
bindkey "$AFTO_ACCEPT_KEY" afto-accept

# --- kill switch --------------------------------------------------------------

afto() {
  case $1 in
    off)
      add-zle-hook-widget -d line-pre-redraw _afto_suggest
      add-zle-hook-widget -d line-finish _afto_line_finish
      zle -N forward-char .forward-char 2>/dev/null || zle -A .forward-char forward-char
      zle -A .forward-word forward-word
      bindkey -r "$AFTO_ACCEPT_KEY"
      print "afto: disabled" ;;
    *)
      print "usage: afto off" ;;
  esac
}
