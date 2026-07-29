# afto.plugin.zsh — production zsh client for the aftod suggestion daemon.
#
# Architecture: DESIGN.md §2. Wire protocol: docs/protocol.md. Phase 1
# requirements: plans/phase-1.md §9. UI mechanics inherited from the
# validated PoC (poc/afto.plugin.zsh).
#
# The contract this file must uphold (violations fail the phase):
#   * TAB is never bound, wrapped, or observed.
#   * Suggestions render only in $POSTDISPLAY (display-only ghost text and,
#     since Phase 2, passive candidate rows below the prompt); $BUFFER is
#     written solely by the accept widgets, and only with text already
#     shown to the user.
#   * Prefix invariant: a candidate displays only if it strictly extends
#     the CURRENT $BUFFER — enforced here, whatever the daemon returns.
#   * While the passive list is visible, no keys are claimed. The tier-3
#     menu keymap exists only between an explicit AFTO_MENU_KEY entry and
#     the next exit; any key it doesn't know exits and replays natively.
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

: ${AFTO_HIGHLIGHT:="fg=8"}            # ghost style; dim = "this came from afto, not you"
: ${AFTO_HIGHLIGHT_ROW:="fg=8"}        # passive list rows
: ${AFTO_HIGHLIGHT_SELECTED:="standout"} # the ▸-marked row
: ${AFTO_ACCEPT_KEY:="^]"}    # dedicated accept key (unbound in emacs keymap)
: ${AFTO_ROWS:=4}             # passive list rows below the ghost, 0..10 (0 = ghost only)
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
typeset -g  _afto_shown=""          # full text of the SELECTED candidate on display
typeset -g  _afto_ghost=""          # its remainder past $BUFFER (the dim part);
                                    # accepts consume this, never $POSTDISPLAY,
                                    # which since Phase 2 also holds list rows
typeset -ga _afto_cands=()          # last response's candidates (staleness-filtered at render)
typeset -ga _afto_disp=()           # candidates currently rendered as rows
typeset -gi _afto_sel=1             # selected row (1 unless menu mode moved it)
typeset -ga _afto_hl=()             # our region_highlight entries, for exact removal
typeset -gi _afto_last_spawn=0      # last daemon spawn attempt (spawn backoff)
typeset -gi _afto_last_exit=0       # $? of the previous command, sent as context
typeset -g  _afto_pending_cmd=""    # command captured by preexec, recorded in precmd
typeset -g  _afto_session=""

# Rows wanted, clamped once at load; the request's limit follows from it
# (the ghost needs one candidate even in rows=0 ghost-only mode).
typeset -gi _afto_rows=$AFTO_ROWS
(( _afto_rows < 0 )) && _afto_rows=0
(( _afto_rows > 10 )) && _afto_rows=10
typeset -gi _afto_limit=$(( _afto_rows > 0 ? _afto_rows : 1 ))

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

# --- rendering: ghost + passive rows (DESIGN.md §2.1 tiers 1–2) ----------------

# Clears the DISPLAY only. The candidate cache survives: it is re-filtered
# against the buffer on the next render, which is what makes the local
# fast path (typing through a suggestion) instant.
_afto_clear() {
  emulate -L zsh
  POSTDISPLAY=""
  _afto_shown=""
  _afto_ghost=""
  _afto_disp=()
  local e
  for e in $_afto_hl; do
    region_highlight=(${region_highlight:#$e})
  done
  _afto_hl=()
}

# The one display pipeline: filter the cached candidates against the
# CURRENT $BUFFER, then paint the selected candidate's remainder as the
# ghost and up to _afto_rows candidates as list rows. Prefix invariant,
# staleness, and the single-line guard all live in the filter — a late
# async response, a menu navigation, and a fast-path keystroke all go
# through here and cannot display anything that doesn't strictly extend
# what the user actually typed. The explicit non-empty-BUFFER check
# matters: with BUFFER="" the extension pattern would accept ANY text,
# and a late response could paint onto a brand-new empty prompt.
_afto_render() {
  emulate -L zsh
  _afto_clear
  [[ -n $BUFFER && $BUFFER != *$'\n'* ]] || return 0
  local -a disp
  local c
  for c in $_afto_cands; do
    [[ $c == ${BUFFER}?* && $c != *$'\n'* ]] || continue
    disp+=($c)
    (( ${#disp} == _afto_limit )) && break
  done
  (( ${#disp} )) || return 0
  (( _afto_sel > ${#disp} )) && _afto_sel=${#disp}
  (( _afto_sel < 1 )) && _afto_sel=1
  _afto_disp=($disp)
  _afto_shown=${disp[_afto_sel]}
  _afto_ghost=${_afto_shown#$BUFFER}

  local post=$_afto_ghost
  local -i B=${#BUFFER} start i=0
  local -a hl
  hl=("$B $(( B + ${#post} )) $AFTO_HIGHLIGHT")
  if (( _afto_rows > 0 )); then
    for c in $disp; do
      (( i++ ))
      start=$(( B + ${#post} + 1 ))     # +1: highlight starts after the \n
      if (( i == _afto_sel )); then
        post+=$'\n'"  ▸ $c"
        hl+=("$start $(( B + ${#post} )) $AFTO_HIGHLIGHT_SELECTED")
      else
        post+=$'\n'"    $c"
        hl+=("$start $(( B + ${#post} )) $AFTO_HIGHLIGHT_ROW")
      fi
    done
  fi
  POSTDISPLAY=$post
  _afto_hl=($hl)
  region_highlight+=($hl)
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
  local msg="{\"v\":1,\"type\":\"suggest\",\"id\":$_afto_req_id,\"fmt\":\"tsv\",\"limit\":$_afto_limit,\"buffer\":\"$buf\",\"cursor\":${CURSOR:-0},\"cwd\":\"$cwd\",\"last_exit\":$_afto_last_exit,\"session\":\"$_afto_session\"}"
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
#
# The line is "id \t text [\t text …]": escaping guarantees every real tab
# byte is a separator, so a plain tab split recovers the fields (a Phase 1
# daemon that ignores "limit" just yields a one-candidate list).
_afto_process() {
  emulate -L zsh
  local line=$_afto_stash REPLY f
  _afto_stash=""
  local -a fields cands
  fields=("${(@ps:\t:)line}")
  local id=$fields[1]
  _afto_debug "process id=$id fields=$(( ${#fields} - 1 )) inflight=$_afto_inflight dirty=$_afto_dirty buffer=${(q)BUFFER}"

  [[ $id == $_afto_inflight ]] || return 0   # response to an abandoned request
  _afto_inflight=""
  if (( _afto_dirty )); then
    _afto_request             # buffer moved on; chase it with a fresh query
  fi

  for f in "${(@)fields[2,-1]}"; do
    [[ -n $f ]] || continue
    _afto_tsv_unescape "$f"
    cands+=("$REPLY")
  done
  # Replace the cache and render against the buffer AS IT IS NOW, not as
  # it was when the request was sent: _afto_render's extension filter is
  # the staleness check. An empty response clears a now-unbacked display.
  _afto_cands=($cands)
  _afto_sel=1
  _afto_render
  zle -R
  return 0
}

# --- ZLE hooks -----------------------------------------------------------------

_afto_suggest() {
  emulate -L zsh
  # Only at a normal toplevel prompt, single-line, non-empty buffer.
  if [[ $CONTEXT != start || -z $BUFFER || $BUFFER == *$'\n'* ]]; then
    _afto_clear
    return 0
  fi
  # Local fast path: re-filtering the cache against the new buffer shrinks
  # the ghost and prunes rows instantly, no round trip. The async refresh
  # below still runs so better candidates can replace them.
  _afto_sel=1
  _afto_render
  if [[ -n $_afto_inflight ]]; then
    _afto_dirty=1
  else
    _afto_request
  fi
  return 0
}

_afto_line_finish() {
  emulate -L zsh
  _afto_clear   # Enter executes only $BUFFER; don't ghost/list the scrollback
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
# All of them consume _afto_ghost/_afto_shown — text that is on display by
# definition — never $POSTDISPLAY, which since Phase 2 also carries the rows.

_afto_accept_full() {
  emulate -L zsh
  BUFFER+=$_afto_ghost   # BUFFER becomes exactly _afto_shown, as displayed
  _afto_clear
  CURSOR=${#BUFFER}
}

# forward-char stays native everywhere except at end-of-line with a ghost
# visible — where the native action is a no-op, so claiming it hijacks
# nothing (DESIGN.md §2.2).
_afto_forward_char() {
  if (( CURSOR == ${#BUFFER} )) && [[ -n $_afto_ghost ]]; then
    _afto_accept_full
  else
    zle .forward-char
  fi
}

# forward-word at EOL with a ghost: accept one word of it.
_afto_forward_word() {
  emulate -L zsh
  setopt extended_glob   # the word-splitting pattern below uses `#` repetition
  if (( CURSOR == ${#BUFFER} )) && [[ -n $_afto_ghost ]]; then
    local ghost=$_afto_ghost word
    word=${ghost%%${ghost##[[:space:]]#[^[:space:]]#}}
    [[ -z $word ]] && word=$ghost
    BUFFER+=$word
    CURSOR=${#BUFFER}
    _afto_render   # what remains of the suggestion (and the rows) re-paints
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
      _afto_clear 2>/dev/null
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
