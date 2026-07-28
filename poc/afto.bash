# afto.bash — Phase 0 proof-of-concept for bash (explicit-invoke tier)
#
# Readline has no display-only ghost text, so the bash PoC is invoke-only:
# nothing renders and nothing changes until the user presses the afto key.
# TAB remains 100% readline / bash-completion.
#
# Default key: C-] (character-search is rarely used; override with AFTO_ACCEPT_KEY).

[[ -n $AFTO_DISABLE ]] && return 0
[[ $- == *i* ]] || return 0

: "${AFTO_ACCEPT_KEY:=\C-]}"

# Phase 0 engine: most recent history entry extending the current line.
# Phase 1 swaps this for a socket query to aftod.
_afto_engine_bash() {
  HISTTIMEFORMAT= builtin history \
    | sed 's/^ *[0-9]* *//' \
    | awk -v p="$1" 'index($0, p) == 1 && $0 != p { m = $0 } END { print m }'
}

_afto_invoke() {
  local line=$READLINE_LINE suggestion
  [[ -z $line ]] && return 0
  suggestion=$(_afto_engine_bash "$line")
  # Prefix invariant: only ever extend what the user typed, never rewrite it.
  if [[ -n $suggestion && $suggestion == "$line"?* ]]; then
    READLINE_LINE=$suggestion
    READLINE_POINT=${#READLINE_LINE}
  fi
}

bind -x "\"${AFTO_ACCEPT_KEY}\": _afto_invoke"
