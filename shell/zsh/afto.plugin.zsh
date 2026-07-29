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
# zsh/parameter exposes $aliases. Optional: without it we simply never send
# an alias table and candidates go un-annotated.
zmodload zsh/parameter 2>/dev/null
autoload -Uz add-zle-hook-widget add-zsh-hook || return 0

: ${AFTO_HIGHLIGHT:="fg=8"}            # ghost style; dim = "this came from afto, not you"
: ${AFTO_HIGHLIGHT_ROW:="fg=8"}        # passive list rows
: ${AFTO_HIGHLIGHT_SELECTED:="standout"} # the ▸-marked row
: ${AFTO_ACCEPT_KEY:="^]"}    # dedicated accept key (unbound in emacs keymap)
: ${AFTO_MENU_KEY:="^O"}      # tier-3 menu mode entry (rare-by-default binding)
: ${AFTO_ROWS:=4}             # passive list rows below the ghost, 0..10 (0 = ghost only)
# Rows to show UNPROMPTED on an empty line. Default 0 — next-command
# predictions are worth having, but printing them under every fresh prompt
# would move the prompt after every command. At 0 they are one ^O away;
# raise it if you want them always visible.
: ${AFTO_EMPTY_ROWS:=0}
: ${AFTO_CMD:="aftod"}        # daemon binary (PATH or absolute)
# AFTO_DEBUG=<file>: append a client-side event trace (connects, sends,
# responses, displays). The only sanctioned diagnostic output — it goes to a
# file, never the terminal, so the "failure is silence" contract holds.

# Box UI theme — hex truecolor (ZSH 5.8+ / truecolor terminal). Override any
# of these before sourcing the plugin to retheme without touching this file.
: ${AFTO_BOX_BORDER:="fg=#6d6a7f"}                  # ╭─╮│╰╯ and dim rows
: ${AFTO_BOX_ACCENT:="fg=#a277ff"}                  # scroll counter, key hints
: ${AFTO_BOX_SEL_BG:="bg=#3d375e,fg=#edecee"}       # selected row background
: ${AFTO_BOX_MARKER:="fg=#61ffca,bold,bg=#3d375e"}  # ▸ on selected row
: ${AFTO_BOX_BADGE:="fg=#61ffca,bg=#1a2d36"}        # source badge, unselected
: ${AFTO_BOX_BADGE_SEL:="fg=#110f18,bg=#61ffca,bold"} # source badge, selected

_afto_debug() {
  [[ -n $AFTO_DEBUG ]] || return 0
  print -r -- "$EPOCHREALTIME $*" >>| $AFTO_DEBUG 2>/dev/null
}

# --- state -------------------------------------------------------------------

typeset -g  _afto_sock=""           # resolved socket path
typeset -g  _afto_fd=""             # connected fd ("" = disconnected)
typeset -gi _afto_req_id=0          # monotonically increasing request id
typeset -g  _afto_inflight=""       # id awaiting a response ("" = idle)
typeset -g  _afto_req_buffer=""     # the buffer that in-flight request was made for
typeset -gi _afto_dirty=0           # buffer changed while a request was in flight
typeset -g  _afto_shown=""          # full text of the SELECTED candidate on display
typeset -g  _afto_ghost=""          # its remainder past $BUFFER (the dim part);
                                    # accepts consume this, never $POSTDISPLAY,
                                    # which since Phase 2 also holds list rows
typeset -ga _afto_cands=()          # last response's candidates (staleness-filtered at render)
typeset -ga _afto_cnotes=()         # their notes, index-parallel to _afto_cands
typeset -ga _afto_disp=()           # candidates currently rendered as rows
typeset -ga _afto_dnotes=()         # their notes, index-parallel to _afto_disp
typeset -gi _afto_sel=1             # selected row (1 unless menu mode moved it)
typeset -ga _afto_hl=()             # our region_highlight entries, for exact removal
typeset -gi _afto_menu_active=0     # tier-3 menu mode engaged (afto-menu keymap live)
typeset -g  _afto_menu_prev=""      # keymap to restore on menu exit
typeset -g  _afto_last_cmd=""       # previous command, sent as `recent` context
typeset -g  _afto_alias_sig=""      # snapshot of the alias table, to detect edits
typeset -gi _afto_last_spawn=0      # last daemon spawn attempt (spawn backoff)
typeset -gi _afto_last_exit=0       # $? of the previous command, sent as context
typeset -g  _afto_pending_cmd=""    # command captured by preexec, recorded in precmd
typeset -g  _afto_session=""

# Rows wanted, clamped once at load; the request's limit follows from it
# (the ghost needs one candidate even in rows=0 ghost-only mode).
typeset -gi _afto_rows=$AFTO_ROWS
(( _afto_rows < 0 )) && _afto_rows=0
(( _afto_rows > 10 )) && _afto_rows=10
typeset -gi _afto_empty_rows=$AFTO_EMPTY_ROWS
(( _afto_empty_rows < 0 )) && _afto_empty_rows=0
(( _afto_empty_rows > 10 )) && _afto_empty_rows=10
# Ask for enough candidates to fill the menu even when passive rows are
# off, so ^O has something to open.
typeset -gi _afto_limit=$(( _afto_rows > 0 ? _afto_rows : 1 ))
(( _afto_empty_rows > _afto_limit )) && _afto_limit=$_afto_empty_rows

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
  s=${s//'\u'/$'\x1f'}
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
  _afto_dnotes=()
  local e
  for e in $_afto_hl; do
    region_highlight=(${region_highlight:#$e})
  done
  _afto_hl=()
}

# The one display pipeline: filter the cached candidates against the
# CURRENT $BUFFER, then paint the selected candidate's remainder as the
# ghost and up to N candidates as list rows. Prefix invariant, staleness,
# and the single-line guard all live in the filter — a late async response,
# a menu navigation, and a fast-path keystroke all go through here and
# cannot display anything that doesn't strictly extend what the user
# actually typed.
#
# The empty buffer is the one case where candidates are NOT extensions of
# anything: those are next-command predictions (DESIGN.md §2.4.3), so they
# render as rows while _afto_ghost stays empty. That single omission is
# what makes them unreachable by every accept key — the widgets consume the
# ghost, so an empty ghost means nothing can be accepted except through
# explicit menu entry.
_afto_render() {
  emulate -L zsh
  _afto_clear
  [[ $BUFFER != *$'\n'* ]] || return 0

  # How many rows this state warrants: a typed line uses AFTO_ROWS; a bare
  # prompt shows nothing unless the user asked (menu) or opted in.
  local -i rows=$_afto_rows
  if [[ -z $BUFFER ]]; then
    (( _afto_menu_active )) && rows=$_afto_rows || rows=$_afto_empty_rows
  fi

  local -a disp dnotes
  local c
  local -i i=0
  for c in $_afto_cands; do
    (( i++ ))
    [[ $c == ${BUFFER}?* && $c != *$'\n'* ]] || continue
    disp+=("$c")
    dnotes+=("${_afto_cnotes[i]}")
    (( ${#disp} == _afto_limit )) && break
  done
  (( ${#disp} )) || return 0
  (( _afto_sel > ${#disp} )) && _afto_sel=${#disp}
  (( _afto_sel < 1 )) && _afto_sel=1
  _afto_disp=("${disp[@]}")
  _afto_dnotes=("${dnotes[@]}")

  local post=""
  if [[ -n $BUFFER ]]; then
    _afto_shown=${disp[_afto_sel]}
    _afto_ghost=${_afto_shown#$BUFFER}
    post=$_afto_ghost
  fi

  local -i B=${#BUFFER} p
  local -a hl
  [[ -n $post ]] && hl=("$B $(( B + ${#post} )) $AFTO_HIGHLIGHT")

  if (( rows > 0 )); then
    # COLUMNS-1: a row of exactly $COLUMNS chars leaves the cursor in the
    # terminal's pending-wrap state; the following \n then fires a spurious
    # extra newline, producing blank lines between rows.
    local -i boxW=$(( COLUMNS > 31 ? COLUMNS - 1 : 39 ))
    local -i innerW=$(( boxW - 2 ))

    # ── Top border: ╭── N/M ──────────────────────────────────────────────╮
    local scrollInfo=" ${_afto_sel}/${#disp} "
    local -i siLen=${#scrollInfo}
    local -i lDash=$(( (innerW - siLen) / 2 ))
    local -i rDash=$(( innerW - siLen - lDash ))
    (( lDash < 1 )) && lDash=1
    (( rDash < 1 )) && rDash=1
    local topLine="╭${(r:$lDash::─:)}${scrollInfo}${(r:$rDash::─:)}╮"

    p=$(( B + ${#post} ))
    post+=$'\n'${topLine}
    local -i tb=$(( p + 1 ))                                        # ╭ position
    hl+=("$tb $(( tb + boxW )) $AFTO_BOX_BORDER")                  # whole line dim
    hl+=("$(( tb + 1 + lDash )) $(( tb + 1 + lDash + siLen )) $AFTO_BOX_ACCENT")  # N/M

    # ── Data rows: │ ▸ paddedCmd badgePadded│
    # Row template: "│ ${marker} ${paddedCmd}${badgePadded}│"
    # Char widths:   1  1  1  1   cmdW        badgeLen       1  = 5+cmdW+badgeLen = boxW
    # → cmdW = innerW - 3 - badgeLen
    # Positions (0-indexed from │): marker@2, paddedCmd@4..3+cmdW, badge@4+cmdW
    i=0
    for c in "${disp[@]}"; do
      (( i++ ))
      (( i > rows )) && break

      local rawNote=${dnotes[i]}
      local badgePadded=""
      local -i badgeLen=0
      if [[ -n $rawNote ]]; then
        badgePadded=" ${rawNote} "
        badgeLen=${#badgePadded}
      fi
      # Drop badge if it would leave fewer than 4 chars for the command.
      (( innerW - 3 - badgeLen < 4 )) && { badgePadded=""; badgeLen=0; }

      local -i cmdW=$(( innerW - 3 - badgeLen ))
      local marker=" "
      (( i == _afto_sel )) && marker="▸"

      local cmdDisplay=$c
      (( ${#cmdDisplay} > cmdW )) && cmdDisplay="${cmdDisplay[1,$(( cmdW - 1 ))]}…"
      local paddedCmd=${(r:$cmdW:: :)cmdDisplay}

      local row="│ ${marker} ${paddedCmd}${badgePadded}│"
      p=$(( B + ${#post} ))
      post+=$'\n'${row}
      local -i rs=$(( p + 1 ))           # points to │
      local -i re=$(( rs + ${#row} ))    # exclusive end

      if (( i == _afto_sel )); then
        hl+=("$rs $re $AFTO_BOX_SEL_BG")
        hl+=("$(( rs + 2 )) $(( rs + 3 )) $AFTO_BOX_MARKER")        # ▸
        if [[ -n $badgePadded ]]; then
          local -i bs=$(( rs + 4 + cmdW ))
          hl+=("$bs $(( bs + badgeLen )) $AFTO_BOX_BADGE_SEL")
        fi
      else
        hl+=("$rs $re $AFTO_BOX_BORDER")
        hl+=("$rs $(( rs + 1 )) $AFTO_BOX_ACCENT")                  # left │
        hl+=("$(( re - 1 )) $re $AFTO_BOX_ACCENT")                  # right │
        if [[ -n $badgePadded ]]; then
          local -i bs=$(( rs + 4 + cmdW ))
          hl+=("$bs $(( bs + badgeLen )) $AFTO_BOX_BADGE")
        fi
      fi
    done

    # ── Bottom border: ╰──────────── ↑↓ Navigate  •  → Accept ──────────────╯
    local fKey1=" ↑↓ Navigate "
    local fSep=" • "
    local fKey2=" → Accept "
    local footer="${fKey1}${fSep}${fKey2}"
    local -i fLen=${#footer}
    local -i lFill=$(( (innerW - fLen) / 2 ))
    local -i rFill=$(( innerW - fLen - lFill ))
    (( lFill < 1 )) && lFill=1
    (( rFill < 1 )) && rFill=1
    local botLine="╰${(r:$lFill::─:)}${footer}${(r:$rFill::─:)}╯"

    p=$(( B + ${#post} ))
    post+=$'\n'${botLine}
    local -i bb=$(( p + 1 ))
    hl+=("$bb $(( bb + boxW )) $AFTO_BOX_BORDER")
    local -i fStart=$(( bb + 1 + lFill ))
    hl+=("$fStart $(( fStart + ${#fKey1} )) $AFTO_BOX_ACCENT")
    local -i fStart2=$(( fStart + ${#fKey1} + ${#fSep} ))
    hl+=("$fStart2 $(( fStart2 + ${#fKey2} )) $AFTO_BOX_ACCENT")
  fi

  POSTDISPLAY=$post
  _afto_hl=("${hl[@]}")
  region_highlight+=("${hl[@]}")
}

# --- async request/response ----------------------------------------------------

# Is a query warranted right now? A typed line always is. An empty line is
# only when the user asked for predictions — via ^O (menu pending) or by
# opting into passive empty rows. Without this the dirty-flag chase in
# _afto_process would query every fresh prompt.
_afto_may_query() {
  [[ -n $BUFFER ]] && return 0
  (( _afto_empty_rows > 0 || _afto_menu_active )) && return 0
  return 1
}

_afto_request() {
  # ${CURSOR:-0}: zle special params are not always populated in zle -F
  # handler context.
  _afto_may_query || return 1
  [[ -n $_afto_fd ]] || _afto_connect || return 1
  zle -F $_afto_fd _afto_response 2>/dev/null
  local REPLY buf cwd recent=""
  _afto_json_escape "$BUFFER"; buf=$REPLY
  _afto_json_escape "$PWD";    cwd=$REPLY
  # The previous command is what next-command prediction keys off. Sent
  # only when there is one, to keep the common message small.
  if [[ -n $_afto_last_cmd ]]; then
    _afto_json_escape "$_afto_last_cmd"
    recent=",\"recent\":[\"$REPLY\"]"
  fi
  (( _afto_req_id++ ))
  # Small write (buffer-sized) into a local socket: cannot meaningfully
  # block. A write error means the daemon died — silently drop the
  # connection; reconnect happens on a later keystroke.
  local msg="{\"v\":1,\"type\":\"suggest\",\"id\":$_afto_req_id,\"fmt\":\"tsv\",\"limit\":$_afto_limit,\"notes\":true,\"buffer\":\"$buf\",\"cursor\":${CURSOR:-0},\"cwd\":\"$cwd\",\"last_exit\":$_afto_last_exit,\"session\":\"$_afto_session\"$recent}"
  _afto_debug "send $msg"
  if ! print -u $_afto_fd -r -- $msg 2>/dev/null; then
    _afto_debug "send failed; disconnecting"
    _afto_disconnect
    return 1
  fi
  _afto_inflight=$_afto_req_id
  _afto_req_buffer=$BUFFER
  _afto_dirty=0
}

# Ship the shell's alias table so the daemon can annotate candidates with
# what they expand to (docs/protocol.md "Shipping the alias table").
#
# Called from precmd and on connect — never from the keystroke path. The
# cap matters for the same reason: this is the one message whose size the
# user controls, and a write large enough to fill the socket buffer would
# block the prompt. Past the cap we send fewer aliases (fewer notes),
# which is a cosmetic loss, rather than risk the prompt.
_afto_send_aliases() {
  (( ${+aliases} )) || return 0
  [[ -n $_afto_fd ]] || return 0
  local sig=${(j:\0:)${(kv)aliases}}
  [[ $sig == $_afto_alias_sig ]] && return 0     # unchanged since last send
  _afto_alias_sig=$sig

  local -a parts
  local k v REPLY ek ev
  local -i bytes=0
  for k v in "${(@kv)aliases}"; do
    _afto_json_escape "$k"; ek=$REPLY
    _afto_json_escape "$v"; ev=$REPLY
    (( bytes += ${#ek} + ${#ev} + 6 ))
    (( bytes > 6000 )) && break
    parts+=("\"$ek\":\"$ev\"")
  done
  local msg="{\"v\":1,\"type\":\"aliases\",\"session\":\"$_afto_session\",\"map\":{${(j:,:)parts}}}"
  _afto_debug "aliases ${#parts} entries"
  print -u $_afto_fd -r -- $msg 2>/dev/null || _afto_disconnect
  return 0
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
  local line=$_afto_stash REPLY f text note
  _afto_stash=""
  local -a fields cands notes
  fields=("${(@ps:\t:)line}")
  local id=$fields[1]
  _afto_debug "process id=$id fields=$(( ${#fields} - 1 )) inflight=$_afto_inflight dirty=$_afto_dirty menu=$_afto_menu_active buffer=${(q)BUFFER}"

  [[ $id == $_afto_inflight ]] || return 0   # response to an abandoned request
  _afto_inflight=""
  local for_buffer=$_afto_req_buffer
  if (( _afto_dirty )); then
    _afto_request             # buffer moved on; chase it with a fresh query
  fi
  # Answers belong to the line they were asked about. For a typed line the
  # prefix filter would catch a mismatch anyway, but on an EMPTY line every
  # candidate "extends" the buffer, so without this an answer computed for
  # the previous command line would be cached as if it were a prediction
  # for this prompt — and ^O would open a menu of it.
  [[ $BUFFER == $for_buffer ]] || return 0

  # Each field is "text" or "text<US>note" (docs/protocol.md). A daemon
  # that predates notes simply never includes the separator.
  for f in "${(@)fields[2,-1]}"; do
    [[ -n $f ]] || continue
    text=${f%%$'\x1f'*}
    note=""
    [[ $f == *$'\x1f'* ]] && note=${f#*$'\x1f'}
    _afto_tsv_unescape "$text"; cands+=("$REPLY")
    if [[ -n $note ]]; then
      _afto_tsv_unescape "$note"; notes+=("$REPLY")
    else
      notes+=("")
    fi
  done
  # Replace the cache and render against the buffer AS IT IS NOW, not as
  # it was when the request was sent: _afto_render's extension filter is
  # the staleness check. An empty response clears a now-unbacked display.
  _afto_cands=("${cands[@]}")
  _afto_cnotes=("${notes[@]}")
  # Keep the selection while the menu is open: this response is filling in
  # a menu the user is already navigating.
  (( _afto_menu_active )) || _afto_sel=1
  _afto_render
  zle -R
  return 0
}

# --- ZLE hooks -----------------------------------------------------------------

_afto_suggest() {
  emulate -L zsh
  # Menu mode: navigation widgets fire this hook too, but the buffer cannot
  # have changed — re-rendering here would reset the selection. The keymap
  # comparison doubles as self-healing: an abort (^C) starts a fresh line in
  # the main keymap without running any exit widget, so a live flag with a
  # non-menu keymap means "menu died"; fall through to normal suggesting.
  if (( _afto_menu_active )); then
    [[ $KEYMAP == afto-menu ]] && return 0
    _afto_menu_active=0
    _afto_sel=1
  fi
  # Only at a normal toplevel prompt, single-line. An empty buffer is a
  # legitimate query only when predictions were asked for.
  if [[ $CONTEXT != start || $BUFFER == *$'\n'* ]] || { [[ -z $BUFFER ]] && ! _afto_may_query }; then
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
  # Predictions are about what follows the command being run now, so the
  # cache is stale by definition once the line is accepted.
  _afto_cands=()
  _afto_cnotes=()
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
  [[ -n $_afto_fd ]] || _afto_connect
  # Alias definitions can change at any prompt (a sourced file, an
  # interactive `alias`), so re-check here — cheap, and off the keystroke
  # path. Sends only when the table actually differs.
  _afto_send_aliases
  [[ -n $_afto_pending_cmd ]] || return 0
  local cmd=$_afto_pending_cmd REPLY c cwd
  _afto_pending_cmd=""
  # What just ran is the context next-command prediction keys off.
  _afto_last_cmd=$cmd
  [[ -n $_afto_fd ]] || return 0
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

# --- tier-3 menu mode (DESIGN.md §2.1/§2.2): explicit entry, dedicated keymap ----
# Modeled on how zsh's own menu selection works: a separate keymap that
# exists only between explicit entry (AFTO_MENU_KEY) and exit. Because the
# prompt-level keymap is untouched, arrows/TAB/everything stay native until
# the user opts in — and any key the menu doesn't know exits it and replays
# through the restored keymap, so afto never interprets a key itself.

# Switch into the menu keymap. Callable from any widget context, including
# the response handler's widget (that is how an empty-prompt ^O opens once
# its predictions arrive).
_afto_menu_start() {
  _afto_menu_active=1
  _afto_menu_prev=$KEYMAP
  zle -K afto-menu
  _afto_debug "menu enter (from $_afto_menu_prev)"
}

# Entering the menu is ALWAYS synchronous, in this widget. Switching
# keymaps from the response handler's widget does not stick — ZLE keeps
# reading with the keymap that was current when the key was awaited, so an
# async `zle -K` silently loses the next keystroke to the old keymap (a
# menu Enter would execute the line instead of accepting a row). Hence:
# open now, fill in when the answer arrives.
_afto_menu_enter() {
  emulate -L zsh
  (( _afto_rows > 0 )) || return 0   # rows disabled: nothing to navigate
  # With a typed line and nothing displayed, there is no list to enter —
  # opening an empty menu there would just swallow a keystroke.
  (( ${#_afto_disp} > 0 )) || [[ -z $BUFFER ]] || return 0

  _afto_menu_start
  # A bare prompt has nothing cached: ask what usually comes next. Until
  # the answer lands the menu is open but empty, which is harmless — every
  # key it doesn't know exits and replays natively, so a daemon that never
  # answers makes ^O indistinguishable from a no-op.
  if (( ${#_afto_disp} == 0 )); then
    if [[ -n $_afto_inflight ]]; then
      _afto_dirty=1
    else
      _afto_request
    fi
  fi
  _afto_render
  return 0
}

# Shared exit: restore the keymap and passive selection. Safe to call only
# from inside a widget (zle -K needs an active ZLE).
_afto_menu_stop() {
  _afto_menu_active=0
  _afto_sel=1
  zle -K ${_afto_menu_prev:-main}
}

_afto_menu_up() {
  emulate -L zsh
  (( _afto_sel > 1 )) && (( _afto_sel-- ))
  _afto_render
  return 0
}

_afto_menu_down() {
  emulate -L zsh
  (( _afto_sel < ${#_afto_disp} )) && (( _afto_sel++ ))
  _afto_render
  return 0
}

# Enter: the selected row's text — already on display — becomes the buffer.
# It is NOT executed; the user reviews/edits and presses Enter again.
_afto_menu_accept() {
  emulate -L zsh
  local pick=${_afto_disp[_afto_sel]}
  _afto_menu_stop
  # Nothing selected (an empty menu still waiting on its answer): behave
  # exactly like any other unknown key — exit and let Enter do its native
  # job, rather than swallowing it.
  if [[ -z $pick ]]; then
    _afto_render
    zle -U -- "$KEYS"
    return 0
  fi
  BUFFER=$pick
  CURSOR=${#BUFFER}
  _afto_clear
  return 0
}

_afto_menu_esc() {
  emulate -L zsh
  _afto_menu_stop
  _afto_render   # back to passive: marker returns to row 1
  return 0
}

# Catch-all for every key the menu doesn't bind: exit, then push the key
# back onto the input queue so it is reinterpreted by the restored keymap —
# printables self-insert, control keys do their native thing.
_afto_menu_other() {
  emulate -L zsh
  _afto_menu_stop
  _afto_render
  zle -U -- "$KEYS"
  return 0
}

# --- optional fzf picker (DESIGN.md §2.3; docs/fzf.md) ----------------------------
# Defined but DELIBERATELY UNBOUND. afto never claims ^R, ^T or Alt+C —
# coexistence with fzf is guaranteed precisely because we don't touch its
# keys. A user who wants afto's frecency ranking behind ^R opts in:
#
#     bindkey '^R' afto-fzf
#
# This widget forks (fzf, aftod). That is fine and is not a contract
# violation: it runs only when the user presses its key, never from a hook.
# The keystroke path remains fork-free.
_afto_fzf() {
  emulate -L zsh
  (( $+commands[fzf] )) || return 0
  _afto_clear
  local pick
  # --prefix "$LBUFFER": what is left of the cursor filters the list, so the
  # widget refines a partly-typed line instead of discarding it.
  pick=$(command $AFTO_CMD list --prefix "$LBUFFER" 2>/dev/null |
    fzf --height 40% --reverse --query "$LBUFFER" --no-sort) || {
    zle redisplay
    return 0
  }
  [[ -n $pick ]] || { zle redisplay; return 0 }
  BUFFER=$pick
  CURSOR=${#BUFFER}
  zle redisplay
  return 0
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
      bindkey -r "$AFTO_MENU_KEY"
      # afto-fzf is never bound by us, but a user may have bound it; leave
      # their binding alone rather than guessing which key it is on.
      bindkey -D afto-menu 2>/dev/null   # menu cannot be active here: running
      _afto_menu_active=0                # a command means the line was accepted
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
zle -N afto-menu-enter _afto_menu_enter
zle -N afto-fzf _afto_fzf        # defined, never bound: see _afto_fzf
zle -N _afto_menu_up
zle -N _afto_menu_down
zle -N _afto_menu_accept
zle -N _afto_menu_esc
zle -N _afto_menu_other
bindkey "$AFTO_ACCEPT_KEY" afto-accept
bindkey "$AFTO_MENU_KEY" afto-menu-enter

# The tier-3 keymap. Built parentless, then given a catch-all so that any
# key without an explicit meaning below exits the menu and replays natively
# (via _afto_menu_other). Only ^O — an explicit, configurable action — can
# make this keymap current; at the prompt it does not exist as far as key
# handling is concerned.
bindkey -N afto-menu
bindkey -M afto-menu -R "^@-^?" _afto_menu_other          # 0x00–0x7f
bindkey -M afto-menu -R "\M-^@-\M-^?" _afto_menu_other    # 0x80–0xff (incl. UTF-8 lead bytes)
bindkey -M afto-menu "^[[A" _afto_menu_up      # arrows: CSI and SS3 encodings
bindkey -M afto-menu "^[OA" _afto_menu_up
bindkey -M afto-menu "^P"   _afto_menu_up
bindkey -M afto-menu "^[[B" _afto_menu_down
bindkey -M afto-menu "^[OB" _afto_menu_down
bindkey -M afto-menu "^N"   _afto_menu_down
bindkey -M afto-menu "^M"   _afto_menu_accept
bindkey -M afto-menu "^J"   _afto_menu_accept
bindkey -M afto-menu "^["   _afto_menu_esc

# Optimistic connect so the first keystroke already has a live socket (and
# a cold system starts the daemon now rather than mid-typing). On a cold
# start this fails (the daemon is still coming up) and that is fine — but a
# sourced plugin must never leak a failure status to `source`, so end the
# file successfully regardless.
_afto_connect 2>/dev/null
return 0
