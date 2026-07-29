#!/usr/bin/env zsh
# tests/e2e/latency.zsh — keystroke→ghost latency measurement (gate 4:
# p99 < 50ms over ≥200 samples, plans/phase-1.md §11). Run via `make bench`.
#
# What one sample measures — the full async round trip:
#   keystroke written to the pty
#   → ZLE reads it, line-pre-redraw fires, suggest request sent
#   → daemon races providers, replies over the socket
#   → zle -F handler + display widget render POSTDISPLAY
#   → the dim-color bytes (\e[90m) appear in pty output   ← stop clock
#
# Methodology note: each sample types ONE character onto an EMPTY line and
# then clears it (Ctrl+U). Typing whole words would be flattered by the
# plugin's local fast path, which re-renders an existing ghost from cache
# with no daemon round trip at all — real, but not what this gate measures.

emulate -L zsh
zmodload zsh/zpty || { print "latency: zsh/zpty unavailable"; exit 2 }
zmodload zsh/datetime

typeset -g REPO=${0:A:h:h:h}
typeset -g BIN=$REPO/bin/aftod
typeset -g PLUGIN=$REPO/shell/zsh/afto.plugin.zsh
typeset -g TARGET=${AFTO_BENCH_SAMPLES:-200}
typeset -g D

cleanup() {
  local pid=$(lsof -t $D/afto.sock.lock 2>/dev/null)
  [[ -n $pid ]] && kill $pid 2>/dev/null
  zpty -d z 2>/dev/null
  rm -rf $D
}

settle() { local junk; while zpty -r -t z junk 2>/dev/null; do :; done }

main() {
  [[ -x $BIN ]] || { print "latency: $BIN missing — run make build"; return 2 }
  D=$(mktemp -d /tmp/afto-lat-XXXXXX) || return 2
  trap cleanup EXIT INT TERM

  export AFTO_SOCKET=$D/afto.sock AFTO_DATA_DIR=$D/data AFTO_CONFIG=$D/cfg.toml
  export XDG_STATE_HOME=$D/state
  print 'log_level = "debug"' > $AFTO_CONFIG   # for daemon-side handle times
  path=($REPO/bin $path)

  # One seeded command; every sample types "t" and gets this as the ghost.
  export HISTFILE=$D/seedhist
  print ": $EPOCHSECONDS:0;true afto-latency-target with several words of tail" > $HISTFILE

  zpty z zsh -f -i
  zpty -w z "source $PLUGIN"
  sleep 2.5
  settle

  local -a samples
  local -i misses=0 ok
  local -F t0 t1
  local chunk got

  while (( ${#samples} < TARGET )); do
    t0=$EPOCHREALTIME
    zpty -wn z "t"
    got="" ok=0
    repeat 150; do                     # ≤ ~450ms before declaring a miss
      if zpty -r -t z chunk 2>/dev/null; then
        got+=$chunk
        if [[ $got == *$'\e[90m'* ]]; then ok=1; break; fi
      else
        sleep 0.003
      fi
    done
    t1=$EPOCHREALTIME
    if (( ok )); then
      samples+=( $(( (t1 - t0) * 1000.0 )) )
    else
      (( misses++ ))
      (( misses > 20 )) && { print "latency: too many misses; aborting"; return 1 }
    fi
    zpty -wn z $'\x15'                 # Ctrl+U: back to an empty line
    sleep 0.01
    settle
  done

  local -a sorted
  sorted=( ${(n)samples} )
  pct() { local -i i=$(( ($1 * ${#sorted}) / 100.0 )); (( i < 1 )) && i=1; (( i > ${#sorted} )) && i=${#sorted}; print -r -- $sorted[$i] }

  printf "samples: %d   misses: %d\n" ${#sorted} $misses
  printf "p50: %.1fms   p90: %.1fms   p99: %.1fms   max: %.1fms\n" \
    $(pct 50) $(pct 90) $(pct 99) $sorted[-1]

  # Daemon-side handle times, from the debug log (gate: < 40ms). Go logs
  # sub-millisecond durations with a µs suffix; normalize before comparing.
  local worst
  worst=$(grep -o 'took=[0-9.]*[µm]s' $D/state/afto/aftod.log 2>/dev/null \
    | perl -ne '/took=([0-9.]+)(µs|ms)/ and printf "%.3f\n", $2 eq "µs" ? $1/1000 : $1' \
    | sort -n | tail -1)
  [[ -n $worst ]] && printf "daemon worst handle time: %sms\n" $worst

  local p99=$(pct 99)
  if (( p99 < 50 )); then
    print "latency gate: PASS (p99 ${p99}ms < 50ms)"
    return 0
  fi
  print "latency gate: FAIL (p99 ${p99}ms >= 50ms)"
  return 1
}

main "$@"
