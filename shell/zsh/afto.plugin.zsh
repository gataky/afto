# afto.plugin.zsh — production zsh client for the aftod suggestion daemon.
#
# Architecture: DESIGN.md §2. Wire protocol: docs/protocol.md. Phase 1
# requirements: plans/phase-1.md §9. UI mechanics inherited from the
# validated PoC (poc/afto.plugin.zsh).
#
# The contract this file must uphold (violations fail the phase):
#   * TAB is never bound, wrapped, or observed.
#   * Suggestions render only in $POSTDISPLAY (display-only ghost text);
#     $BUFFER is written solely by the accept widgets, and only with text
#     already shown to the user.
#   * Prefix invariant: a suggestion displays only if it strictly extends
#     the CURRENT $BUFFER — enforced here, whatever the daemon returns.
#   * The keystroke path never blocks and never spawns a process: all
#     engine work is async over a unix socket (zsh/net/socket + zle -F);
#     every failure mode degrades to "no ghost text", silently.
#
# Flow control (docs/protocol.md): at most ONE suggest request in flight.
# Keystrokes while waiting set a dirty flag; the response handler re-sends
# with the fresh buffer. Responses are TSV ("id \t escaped-text") because
# parsing JSON without forking is not a thing zsh can do.

[[ -n $AFTO_DISABLE ]] && return 0
[[ -o interactive ]] || return 0
zmodload zsh/net/socket 2>/dev/null || return 0
zmodload zsh/datetime 2>/dev/null || return 0
zmodload zsh/system 2>/dev/null || return 0
autoload -Uz add-zle-hook-widget add-zsh-hook || return 0

: ${AFTO_HIGHLIGHT:="fg=8"}   # ghost style; dim = "this came from afto, not you"
: ${AFTO_ACCEPT_KEY:="^]"}    # dedicated accept key (unbound in emacs keymap)
: ${AFTO_CMD:="aftod"}        # daemon binary (PATH or absolute)
# AFTO_DEBUG=<file>: append a client-side event trace (connects, sends,
# responses, displays). The only sanctioned diagnostic output — it goes to a
# file, never the terminal, so the "failure is silence" contract holds.

_afto_debug() {
  [[ -n $AFTO_DEBUG ]] || return 0
  print -r -- "$EPOCHREALTIME $*" >>| $AFTO_DEBUG 2>/dev/null
}

# --- state -------------------------------------------------------------------

typeset -g  _afto_sock=""           # resolved socket path
typeset -g  _afto_fd=""             # connected fd ("" = disconnected)
typeset -gi _afto_req_id=0          # monotonically increasing request id
typeset -g  _afto_inflight=""       # id awaiting a response ("" = idle)
typeset -gi _afto_dirty=0           # buffer changed while a request was in flight
typeset -g  _afto_shown=""          # full text (BUFFER+ghost) currently displayed
typeset -g  _afto_hl_entry=""       # our region_highlight entry, for clean removal
typeset -gi _afto_last_spawn=0      # last daemon spawn attempt (spawn backoff)
typeset -gi _afto_last_exit=0       # $? of the previous command, sent as context
typeset -g  _afto_pending_cmd=""    # command captured by preexec, recorded in precmd
typeset -g  _afto_session=""

# Socket path mirrors daemon/cmd/aftod/paths.go — keep in sync.
if [[ -n $AFTO_SOCKET ]]; then
  _afto_sock=$AFTO_SOCKET
elif [[ -n $XDG_RUNTIME_DIR ]]; then
  _afto_sock=$XDG_RUNTIME_DIR/afto/afto.sock
else
  _afto_sock=$HOME/.cache/afto/afto.sock
fi

# --- tiny codecs (pure zsh: the hot path may not fork) ------------------------

# JSON string escape into $REPLY: backslash and quote, C0 controls we care
# about as \n/\t/\r, remaining control chars dropped. Enough for content
# that originates from a command line.
_afto_json_escape() {
  local s=$1
  s=${s//'\'/'\\'}
  s=${s//'"'/'\"'}
  s=${s//$'\n'/'\n'}
  s=${s//$'\t'/'\t'}
  s=${s//$'\r'/'\r'}
  s=${s//[[:cntrl:]]/}
  REPLY=$s
}

# TSV unescape into $REPLY (inverse of the daemon's escapeTSV; see
# daemon/internal/ipc/tsv.go). The placeholder trick stands in for a
# left-to-right parse: swap '\\' out first so '\\t' cannot mis-decode.
_afto_tsv_unescape() {
  local s=$1 ph=$'\x01'
  s=${s//'\\'/$ph}
  s=${s//'\t'/$'\t'}
  s=${s//'\n'/$'\n'}
  s=${s//$ph/'\'}
  REPLY=$s
}

# --- connection management -----------------------------------------------------

_afto_disconnect() {
  if [[ -n $_afto_fd ]]; then
    zle -F $_afto_fd 2>/dev/null   # deregister the response handler
    exec {_afto_fd}>&- 2>/dev/null
    _afto_fd=""
  fi
  _afto_inflight=""
  _afto_dirty=0
}

# Connect, lazily spawning the daemon. A failed connect is cheap (one
# syscall), so we retry on every keystroke; the SPAWN is what's throttled
# (once per 30s), because a broken install would otherwise fork a doomed
# daemon per keypress.
_afto_connect() {
  local REPLY
  if zsocket $_afto_sock 2>/dev/null; then
    _afto_fd=$REPLY
    _afto_debug "connect ok fd=$_afto_fd"
    # Registration can fail before ZLE has initialized (e.g. the optimistic
    # connect at load time); _afto_request re-asserts it on every send, and
    # re-registering an already-watched fd just replaces the handler.
    zle -F $_afto_fd _afto_response 2>/dev/null
    return 0
  fi
  if (( EPOCHSECONDS - _afto_last_spawn >= 30 )); then
    _afto_last_spawn=$EPOCHSECONDS
    command $AFTO_CMD serve --daemonize >/dev/null 2>&1 &!
  fi
  return 1
}

# --- ghost text rendering (identical mechanics to the PoC) --------------------

_afto_clear_ghost() {
  POSTDISPLAY=""
  _afto_shown=""
  if [[ -n $_afto_hl_entry ]]; then
    region_highlight=(${region_highlight:#$_afto_hl_entry})
    _afto_hl_entry=""
  fi
}

_afto_show_ghost() {
  local suggestion=$1
  _afto_clear_ghost
  # Prefix invariant + single-line guard, enforced at the last moment
  # before anything becomes visible. The explicit non-empty-BUFFER check
  # matters: with BUFFER="" the pattern would accept ANY text, and a late
  # async response could paint a ghost on a brand-new empty prompt.
  [[ -n $BUFFER && -n $suggestion && $suggestion == ${BUFFER}?* && $suggestion != *$'\n'* ]] || return 0
  POSTDISPLAY=${suggestion#$BUFFER}
  _afto_hl_entry="${#BUFFER} $(( ${#BUFFER} + ${#POSTDISPLAY} )) $AFTO_HIGHLIGHT"
  region_highlight+=($_afto_hl_entry)
  _afto_shown=$suggestion
}

# --- async request/response ----------------------------------------------------

_afto_request() {
  # Guarded here, not just in the hook: the dirty-flag chase in
  # _afto_response can land on a fresh empty prompt, where there is nothing
  # to ask about. ${CURSOR:-0} likewise: zle special params are not always
  # populated in zle -F handler context.
  [[ -n $BUFFER ]] || return 1
  [[ -n $_afto_fd ]] || _afto_connect || return 1
  zle -F $_afto_fd _afto_response 2>/dev/null
  local REPLY buf cwd
  _afto_json_escape "$BUFFER"; buf=$REPLY
  _afto_json_escape "$PWD";    cwd=$REPLY
  (( _afto_req_id++ ))
  # Small write (buffer-sized) into a local socket: cannot meaningfully
  # block. A write error means the daemon died — silently drop the
  # connection; reconnect happens on a later keystroke.
  local msg="{\"v\":1,\"type\":\"suggest\",\"id\":$_afto_req_id,\"fmt\":\"tsv\",\"buffer\":\"$buf\",\"cursor\":${CURSOR:-0},\"cwd\":\"$cwd\",\"last_exit\":$_afto_last_exit,\"session\":\"$_afto_session\"}"
  _afto_debug "send $msg"
  if ! print -u $_afto_fd -r -- $msg 2>/dev/null; then
    _afto_debug "send failed; disconnecting"
    _afto_disconnect
    return 1
  fi
  _afto_inflight=$_afto_req_id
  _afto_dirty=0
}

# zle -F handler: ZLE calls this when the socket fd is readable, i.e. a
# response arrived while we wait for input — typing was never blocked.
#
# CRITICAL SUBTLETY: fd handlers are NOT widgets. ZLE's special parameters
# ($BUFFER, $CURSOR, $POSTDISPLAY, region_highlight) are simply absent in
# this context — reading them yields empty, writing them does nothing
# useful. So the handler only reads the line off the socket and stashes it;
# `zle _afto_process` then invokes a real widget, inside which the specials
# work. (zsh-autosuggestions' async mode uses the same two-step shape.)
_afto_response() {
  local fd=$1 line
  if ! IFS= read -r -u $fd line; then
    _afto_disconnect          # EOF/error: daemon went away
    return 0
  fi
  typeset -g _afto_stash=$line
  zle _afto_process 2>/dev/null || _afto_debug "process skipped (zle inactive)"
  return 0
}

# Widget half of the response path: correlate, chase a dirty buffer, and
# display — all with live access to the real $BUFFER.
_afto_process() {
  local line=$_afto_stash REPLY
  _afto_stash=""
  local id=${line%%$'\t'*} text=${line#*$'\t'}
  _afto_debug "process id=$id inflight=$_afto_inflight dirty=$_afto_dirty buffer=${(q)BUFFER}"

  [[ $id == $_afto_inflight ]] || return 0   # response to an abandoned request
  _afto_inflight=""
  if (( _afto_dirty )); then
    _afto_request             # buffer moved on; chase it with a fresh query
  fi

  _afto_tsv_unescape "$text"
  # Staleness and safety in one check: display only a strict extension of
  # the buffer AS IT IS NOW, not as it was when the request was sent.
  if [[ -n $REPLY && $REPLY == ${BUFFER}?* ]]; then
    _afto_show_ghost "$REPLY"
    zle -R
  fi
  return 0
}

# --- ZLE hooks -----------------------------------------------------------------

_afto_suggest() {
  emulate -L zsh
  # Only at a normal toplevel prompt, single-line, non-empty buffer.
  if [[ $CONTEXT != start || -z $BUFFER || $BUFFER == *$'\n'* ]]; then
    _afto_clear_ghost
    return 0
  fi
  # Local fast path: typing "through" the current ghost shrinks it
  # instantly, no round trip. The async refresh below still runs so a
  # better candidate can replace it.
  if [[ -n $_afto_shown && $_afto_shown == ${BUFFER}?* ]]; then
    _afto_show_ghost "$_afto_shown"
  else
    _afto_clear_ghost
  fi
  if [[ -n $_afto_inflight ]]; then
    _afto_dirty=1
  else
    _afto_request
  fi
  return 0
}

_afto_line_finish() {
  emulate -L zsh
  _afto_clear_ghost   # Enter executes only $BUFFER; don't ghost the scrollback
}

# --- history ingestion (docs/protocol.md "Recording history") -------------------
# preexec hands us the exact command string with zero forking (the plan's
# `fc -ln -1` needs a $(…) subshell, so preexec is the better source);
# precmd fires after execution with its exit status.

_afto_preexec() {
  _afto_pending_cmd=$1
  _afto_debug "preexec ${(q)1}"
}

_afto_precmd() {
  local -i code=$?
  _afto_last_exit=$code
  _afto_debug "precmd exit=$code pending=${(q)_afto_pending_cmd}"
  [[ -n $_afto_pending_cmd ]] || return 0
  local cmd=$_afto_pending_cmd REPLY c cwd
  _afto_pending_cmd=""
  [[ -n $_afto_fd ]] || _afto_connect || return 0
  _afto_json_escape "$cmd"; c=$REPLY
  _afto_json_escape "$PWD"; cwd=$REPLY
  # Fire-and-forget: record has no response by design.
  local msg="{\"v\":1,\"type\":\"record\",\"cmd\":\"$c\",\"exit\":$code,\"cwd\":\"$cwd\",\"session\":\"$_afto_session\",\"ts\":$EPOCHSECONDS}"
  _afto_debug "record $msg"
  print -u $_afto_fd -r -- $msg 2>/dev/null || _afto_disconnect
  return 0
}

# --- accept widgets: the ONLY code paths that write $BUFFER ----------------------

_afto_accept_full() {
  emulate -L zsh
  BUFFER+=$POSTDISPLAY
  _afto_clear_ghost
  CURSOR=${#BUFFER}
}

# forward-char stays native everywhere except at end-of-line with a ghost
# visible — where the native action is a no-op, so claiming it hijacks
# nothing (DESIGN.md §2.2).
_afto_forward_char() {
  if (( CURSOR == ${#BUFFER} )) && [[ -n $POSTDISPLAY ]]; then
    _afto_accept_full
  else
    zle .forward-char
  fi
}

# forward-word at EOL with a ghost: accept one word of it.
_afto_forward_word() {
  emulate -L zsh
  setopt extended_glob   # the word-splitting pattern below uses `#` repetition
  if (( CURSOR == ${#BUFFER} )) && [[ -n $POSTDISPLAY ]]; then
    local ghost=$POSTDISPLAY word
    word=${ghost%%${ghost##[[:space:]]#[^[:space:]]#}}
    [[ -z $word ]] && word=$ghost
    local rest=${ghost#$word}
    local full=$_afto_shown
    BUFFER+=$word
    _afto_clear_ghost
    CURSOR=${#BUFFER}
    [[ -n $rest ]] && _afto_show_ghost "$full"
  else
    zle .forward-word
  fi
}

# --- user command ----------------------------------------------------------------

afto() {
  case $1 in
    off)
      add-zle-hook-widget -d line-pre-redraw _afto_suggest
      add-zle-hook-widget -d line-finish _afto_line_finish
      add-zsh-hook -d preexec _afto_preexec
      add-zsh-hook -d precmd _afto_precmd
      zle -A .forward-char forward-char 2>/dev/null
      zle -A .forward-word forward-word 2>/dev/null
      bindkey -r "$AFTO_ACCEPT_KEY"
      _afto_clear_ghost 2>/dev/null
      _afto_disconnect
      print "afto: disabled for this session"
      ;;
    status)
      print "socket:    $_afto_sock"
      if [[ -n $_afto_fd ]]; then
        print "connected: yes (fd $_afto_fd)"
      else
        print "connected: no"
      fi
      print "session:   $_afto_session"
      command $AFTO_CMD ping --socket $_afto_sock 2>/dev/null \
        || print "daemon:    not reachable"
      ;;
    *)
      print "usage: afto off|status"
      ;;
  esac
}

# --- wire everything up ------------------------------------------------------------

_afto_session="${HOST}.$$.${EPOCHSECONDS}"
_afto_json_escape "$_afto_session"; _afto_session=$REPLY; unset REPLY

add-zle-hook-widget line-pre-redraw _afto_suggest
add-zle-hook-widget line-finish _afto_line_finish
add-zsh-hook preexec _afto_preexec
add-zsh-hook precmd _afto_precmd

zle -N afto-accept _afto_accept_full
zle -N forward-char _afto_forward_char
zle -N forward-word _afto_forward_word
zle -N _afto_process
bindkey "$AFTO_ACCEPT_KEY" afto-accept

# Optimistic connect so the first keystroke already has a live socket (and
# a cold system starts the daemon now rather than mid-typing). On a cold
# start this fails (the daemon is still coming up) and that is fine — but a
# sourced plugin must never leak a failure status to `source`, so end the
# file successfully regardless.
_afto_connect 2>/dev/null
return 0
