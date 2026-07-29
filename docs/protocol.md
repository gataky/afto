# The afto wire protocol

How the pieces of afto talk to each other. The normative spec is
`plans/phase-1.md §5`; this document is the narrative version — what actually
happens on the wire and why it's shaped that way. The implementation lives in
`daemon/internal/ipc/`.

## Participants

A **client** is anything that dials the daemon's unix socket and speaks this
protocol:

- **The zsh plugin** (`shell/zsh/afto.plugin.zsh`) — the primary client. Each
  interactive shell session holds one long-lived connection, opened from
  inside the shell process with zsh's builtin `zsocket` (no helper program).
  One daemon serves every terminal you have open; the `session` field tells
  them apart.
- **The `aftod` CLI** in its `query`/`ping` subcommands — the same binary
  acting as a one-shot diagnostic client for humans and tests.
- Later phases: the bash integration, the optional fzf widget.

Not a client: Phase 4 provider plugins. The daemon spawns those as
subprocesses and talks to them over stdio (same message shapes, opposite
direction — there, the daemon does the asking).

## Framing and versioning

One message per `\n`-terminated line. Requests are always JSON objects and
always carry `"v":1`. Responses are JSON by default; the one exception is
described next. Lines over 1 MiB, malformed JSON, and unknown `type`s are
dropped without closing the connection — see "Error philosophy" below.

## The keystroke round trip

You type the `h` in `git ch`:

```
 zsh (ZLE)                                    aftod
    │  line-pre-redraw hook fires               │
    │                                           │
    │ {"v":1,"type":"suggest","id":42,          │
    │  "fmt":"tsv","limit":4,"buffer":"git ch", │
    │  "cursor":6,"cwd":"/Users/x/proj",        │
    │  "last_exit":0,"session":"host.81021..."} │
    │──────────────────────────────────────────▶│
    │                                           │ race providers vs
    │                                           │ latency budget
    │      "42\tgit checkout main\tgit cherry-  │
    │       pick abc123\tgit checkout -b x\n"   │
    │◀──────────────────────────────────────────│
    │  zle -F handler wakes:                    │
    │   id matches in-flight request?           │
    │   texts still extend current $BUFFER?     │
    │   → render "eckout main" as dim ghost,    │
    │     survivors as passive list rows        │
```

**Why the request is JSON but the response is TSV:** the two directions are
not equally hard in zsh. *Building* JSON is string interpolation into a
template plus a few escaping parameter expansions. *Parsing* JSON needs a
real parser or a forked helper — and forking on every keystroke is forbidden
(the keystroke path must never block or spawn). A TSV response
(`<id>\t<escaped text>[\t<escaped text>…]\n`) parses with a tab split plus
an unescape per field. Clients that can afford real parsing (tests,
`aftod query`, the future fzf widget) omit `fmt` and get JSON:

```json
{"v":1,"id":42,"candidates":[{"text":"git checkout main","score":8.1,"source":"frecency"}]}
```

**How many candidates come back** is the client's call, via the optional
`limit` field (clamped daemon-side to the engine's cap of 10). The ghost
renders from the first candidate; the passive list rows (Phase 2) render
from the rest. A request without `limit` keeps each format's historical
default — top-1 for TSV (so a Phase 1 client is served unchanged),
everything for JSON.

**Notes** (Phase 3) ride along in the same field as their candidate,
behind a unit separator (`0x1f`), and only for clients that set
`"notes":true`:

```
42\tgco main\x1fgco = git checkout\tgit status\n
```

A note is annotation — an alias expansion today, a plugin's explanation
later. It is display text: it never enters `$BUFFER`, and the client strips
it from anything an accept key can take.

Escaping contract: literal `\t`, `\n`, `0x1f` and `\` in the text become
two-character escapes (`\t`, `\n`, `\u`, `\\`), so every real tab byte on
the line is a field separator, every real `0x1f` separates a text from its
note, and an embedded newline can never break framing. Zero candidates is
`<id>\t\n` — one empty field.

## Flow control: one in flight + dirty flag

The shell keeps **at most one** suggest request in flight. If you type three
characters quickly, it does not send three requests: it sends one, marks
itself dirty as further keystrokes land, and sends one fresh request (with
the newest buffer) the moment the response arrives. Consequences:

- The daemon's inbound rate is bounded by its own response speed, not your
  typing speed — no queue can build up.
- Staleness is handled twice over: the `id` echo lets the client discard a
  reply to an outdated request, and the prefix check (`does this text still
  extend the *current* buffer?`) rejects anything the id check missed.
- The prefix invariant is enforced **client-side**. The daemon's providers
  return buffer extensions by construction, but the shell re-verifies —
  a misbehaving provider (or a future AI one) structurally cannot rewrite
  the user's line.

## Recording history

After each command finishes, the shell fire-and-forgets one `record` line
over the same connection:

```json
{"v":1,"type":"record","cmd":"git checkout main","exit":0,"cwd":"/Users/x/proj","session":"host.81021.1722180000","ts":1722180042}
```

There is deliberately **no response**: the shell must never wait on
ingestion, and a lost record is a non-event. Redaction happens daemon-side
before persistence (secret-shaped commands are skipped entirely).

## Shipping the alias table

The daemon can annotate `gco main` with "gco = git checkout" only if it
knows the shell's aliases — and aliases live in the shell, not on disk. So
the client sends them, fire-and-forget like `record`, on connect and again
whenever the table changes:

```json
{"v":1,"type":"aliases","session":"host.81021.1722180000","map":{"gco":"git checkout","ll":"ls -la"}}
```

The daemon holds this per session **in memory only** — alias definitions are
configuration, not command history, and the store is for history. The
message is sent from `precmd`, never from the keystroke path, and is capped
so the write cannot fill the socket buffer; an oversized table is truncated
(fewer notes) rather than delayed.

## Liveness

`{"v":1,"type":"ping"}` → `{"v":1,"ok":true,"version":"..."}`. Used by
`aftod ping`, tests, and `afto status` in the shell.

## Error philosophy

afto's user-facing contract is that failure manifests as the *absence of a
suggestion* — never as an error message, a hung prompt, or a broken
connection. Hence: malformed input is dropped, not answered with errors;
unknown message types are ignored (older daemon + newer client keeps
working); an empty candidate list is a normal, well-formed response. The
client side mirrors this: socket errors silently disable suggestions and
enter reconnect backoff.

## Evolving the protocol

- New context for providers (e.g. git state) = new optional request fields.
  Old daemons ignore them; new daemons tolerate their absence. Bump `v` only
  for changes that break this tolerance. (`limit`, added in Phase 2, is the
  worked example: an old daemon ignores it and answers top-1 TSV, which a
  new client renders as a one-row list; a new daemon without `limit` serves
  a Phase 1 client exactly as before.)
- The same rule shapes how notes were added in Phase 3. A daemon that
  doesn't know them sends no separator and the new client reads an empty
  note; a client that doesn't know them never sets `"notes":true`, so it can
  never receive a byte it would render as command text. Extending an
  encoding is safe when both "extra field absent" and "extra field ignored"
  are already meaningful.
- The AI provider (Phase 5) needs **no** protocol change: `buffer`, `cwd`,
  `recent`, `last_exit` are already on every suggest request.
- Phase 4 plugins reuse `Query`/`Candidate` JSON shapes over stdio, so a
  plugin author learns one format.
