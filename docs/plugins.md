# Writing an afto plugin

A plugin is a program that reads a JSON request per line on stdin and writes
a JSON response per line on stdout. That is the whole contract — no SDK, no
linking, no language requirement. The shipped example
(`examples/plugins/afto-echo.sh`) is fifteen lines of POSIX shell.

Plugins exist for knowledge afto cannot have. The built-in providers rank
what you have *already run*; a plugin can suggest from a Makefile you cloned
five minutes ago, the clusters in your kubeconfig, or the hosts in your
`~/.ssh/config`.

## The protocol

Daemon → plugin (stdin), one line:

```json
{"v":1,"id":7,"buffer":"make ","cursor":5,"cwd":"/w/proj","last_exit":0,"session":"host.1234.1722180000","recent":["git pull"]}
```

Plugin → daemon (stdout), one line:

```json
{"v":1,"id":7,"candidates":[{"text":"make test","score":1.5,"note":"make target"}]}
```

Rules that matter:

- **Echo the `id`.** The daemon correlates on it and discards anything else.
  If your plugin answers a request the daemon has already given up on, that
  answer is dropped rather than being mistaken for the next one's.
- **Answer every request**, even if the answer is `"candidates":[]`. An empty
  list means "nothing for this buffer" and costs you nothing; silence looks
  like a hang and eventually benches you (see *Failure handling*).
- **Flush after every line.** A buffered plugin is indistinguishable from a
  slow one.
- **`text` should extend the buffer.** The shell client displays only strict
  extensions of what the user typed, so `{"text":"test"}` for the buffer
  `make ` is dropped — return `make test`. (Non-extending candidates are not
  wasted: they can still appear in the `^O` menu.)
- **`score` is optional**, defaulting to 1.0. Scores share the built-ins'
  scale — roughly `ln(1+times_used)` decayed by age, so a frequently used
  command sits near 2–3. Scoring yourself at 999 wins every row, which is
  usually the wrong thing to do to your own users.
- **`note` is optional** annotation shown dim beside the row. It is display
  text only and can never enter the command line.
- **`source` is ignored.** The daemon stamps the plugin's configured name, so
  a plugin cannot present itself as `history`.
- **stderr is yours for diagnostics.** The daemon logs it at debug level and
  it never reaches the user's terminal.

## Configuring one

```toml
[[plugin]]
name       = "make-targets"
command    = "/usr/local/bin/afto-make-targets"
args       = []          # optional
timeout_ms = 40          # optional; defaults to latency_budget_ms
enabled    = true        # optional; default true
```

`command` is executed directly — there is no shell, so no quoting or
word-splitting rules, and `args` is a list. Plugins are wired at daemon
start: after editing this table, `kill` the daemon (the next keystroke
respawns it).

**Configuring a plugin is a trust decision.** The daemon sends it your
command line, your working directory, and what you just ran. Only configure
programs you would be comfortable handing your shell history to.

## Failure handling (why your plugin can't hurt the prompt)

Three independent layers, because processes fail in three different ways:

| Failure | What happens |
|---|---|
| Slow | Abandoned at `timeout_ms`, inside the daemon's own latency budget. Your late answer is discarded by id. |
| Crash | Restarted on the next request, with exponential backoff from 1 s to 60 s — a plugin that exits instantly can't become a fork bomb. |
| Persistently broken | After 3 consecutive failures the plugin is **benched** for 30 s: killed, and not even asked. One probe after the cooldown brings it back if it has recovered. |

A failure is a timeout, a crash, a malformed line, or an unreadable
(>64 KiB) response. An empty candidate list is *not* a failure.

Responses are also capped: at most 10 candidates, each with text and note
bounded at 4 KiB. Exceeding those truncates rather than fails.

Plugins are started when the daemon starts, not on the first keystroke — a
cold `fork`+`exec` loses its race against a 40 ms budget, which would make
your plugin mysteriously silent on the first suggestion.

## Testing a plugin by hand

The protocol is line-oriented, so a pipe is enough:

```sh
echo '{"v":1,"id":1,"buffer":"make ","cwd":"'"$PWD"'"}' | ./afto-make-targets
```

With it configured, `aftod query --buffer "make " --limit 10` shows the
merged, ranked result including `"source":"<your plugin>"`.

## Worked example

`daemon/cmd/afto-make-targets/main.go` is a complete, tested plugin in ~120
lines: it parses the Makefile in the query's cwd and suggests its targets.
It shows the shape worth copying — answer only the buffers you understand
(`make <partial>`), return whole-buffer extensions, and stay quiet
otherwise. Every candidate you return competes for the same few rows.
