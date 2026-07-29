# Phase 3 Implementation Plan — context intelligence

**Audience:** an implementing agent/engineer with access to this repository and no
other context. Read `DESIGN.md` (authoritative architecture, esp. §2.4 and §3.1),
`docs/protocol.md`, and the Phase 1/2 reports (`plans/phase-*-report.md`) — the
latter record the ZLE and zpty facts that make the shell code and its tests
correct — before writing code.

## 1. Mission

Build Phase 3 from `DESIGN.md §5`:

> **Context intelligence:** cwd/project-affinity ranking, `transition` provider
> (empty-prompt next-command rows in the menu), alias-expansion notes.

Phases 1–2 rank by what you run and show it as ghost text plus a list. Phase 3
makes the ranking understand *where* you are (this repo, not just this
directory), predicts what comes *next* after the command you just ran, and
annotates candidates with what an alias actually expands to. No new UI tier and
no new key bindings: the transition rows surface through the existing tier-3
menu, and notes render inside the existing tier-2 rows.

## 2. Non-negotiable constraints

All of `plans/phase-1.md §2` and `plans/phase-2.md §2` still apply verbatim. The
ones this phase can newly violate:

1. **Non-prefix candidates are menu-only.** Transition predictions on an empty
   prompt do not extend anything the user typed. `DESIGN.md §2.4.3` allows them
   as *list rows only*: they must never be reachable by a ghost-accept key
   (`→` at EOL, `^]`, `forward-word`), only by explicit menu-mode accept. With
   an empty buffer the plugin therefore renders **rows without a ghost** and
   leaves `_afto_ghost` empty, which structurally disables every accept widget.
2. **No unsolicited screen changes.** Rendering rows under every fresh prompt
   would move the prompt on every command (rows scroll the screen at the bottom
   edge). Empty-prompt predictions are therefore **opt-in per keystroke**: `^O`
   on an empty buffer fetches and opens them. `AFTO_EMPTY_ROWS=N` (default 0)
   optionally makes them passive for users who want that.
3. **The keystroke path still never blocks or forks.** The alias map is sent
   from `precmd` and on connect — never from a ZLE hook — and is capped so the
   write cannot fill the socket buffer.
4. **Redaction still precedes persistence**, and now covers transitions: a pair
   is only recorded between two commands that were themselves stored.
5. **Notes are informational.** A note never enters `$BUFFER`. The rejected
   IRIS behavior (auto-expanding aliases into the line on space) stays rejected.

## 3. Ranking: project affinity

Today's frecency score (`scoring/frecency.go`) has a global term and an exact-
`cwd` term. Phase 3 adds a **project** term so that a command you ran in
`~/proj/api` ranks highly in `~/proj/web` — the useful unit is the repo, not the
directory.

```
score = ln(1+count_all)·D(age_all)
      + W_cwd·ln(1+count_cwd)·D(age_cwd)
      + W_proj·ln(1+count_proj)·D(age_proj)
```

- `W_cwd = 2.0` (unchanged), `W_proj = 1.0` — a project habit outranks a global
  one, an exact-directory habit outranks both. Constants live in `scoring`
  alongside the existing ones, documented as tunable; tests pin ordering, not
  floats.
- `count_proj`/`age_proj` aggregate **all** stats rows whose cwd is inside the
  project root, *including* the exact-cwd row (running it here is running it in
  the project — a command run only in this directory legitimately earns both
  boosts).
- **Project root resolution** (`daemon/internal/project`): walk up from the
  query's cwd for the first directory containing a marker; default markers
  `.git`, `.hg`, `go.mod`, `package.json`, `Cargo.toml`, `.project-root`;
  configurable via `[project] markers = [...]`. No marker found → no project
  term (the score degrades to Phase 1 behavior exactly). Results are cached
  (cwd → root, small map + TTL) because this runs on the keystroke path: a
  cache miss costs a handful of `stat` calls, a hit costs a map lookup.
- **One scan, not two.** `PrefixStats` keeps its single range scan on the
  `(cmd, cwd)` primary key and widens its cwd predicate to
  `cwd = '' OR cwd = :cwd OR (cwd >= :root AND cwd < :rootUpper)`; the provider
  folds the three row kinds per command. No new query on the hot path.

## 4. The `transition` provider

**Data.** Schema v2 adds the aggregate the raw `events` ledger was collected
for:

```sql
CREATE TABLE transitions (
  prev TEXT NOT NULL, next TEXT NOT NULL,
  count INTEGER NOT NULL, last_ts INTEGER NOT NULL,
  PRIMARY KEY (prev, next)
);
CREATE INDEX events_session ON events(session, id);   -- "last command in session"
```

- **Maintained on ingest**, inside the existing transaction: look up the
  session's previous command (`events` by `(session, id)` — the new index makes
  this a one-row index seek), upsert `(prev, next)`. Sessions with no prior
  command (first command in a shell) record nothing.
- **Backfilled once by the migration** from recorded events, pairing consecutive
  rows within each session. **Imported history is deliberately excluded**
  (`session = 'import'`): a `HISTFILE` interleaves every terminal you had open,
  so consecutive lines are not causally related and would poison the table.
  Consequence to document for users: transitions get useful after a day of real
  use, not at install time.
- Self-transitions (`make test` → `make test`) are kept: re-running a command is
  a real, useful prediction.

**Provider.** Fires only when `Buffer == ""` (a prefix query is the other
providers' job); returns the top `next` commands for the current session's
previous command, scored `ln(1+count)·D(age)` on the same scale as frecency.
The previous command comes from `Query.Recent[0]` when the client sends it —
this closes the "recent is never sent" gap from `plans/phase-1-report.md` — and
falls back to a `LastCommand(session)` store lookup for clients that don't
(e.g. `aftod query`).

**Engine.** The empty-buffer case must not be short-circuited: `history` and
`frecency` already return nothing for an empty buffer, so the engine simply
races all three as before.

## 5. Alias notes

`Candidate.Note` has existed since Phase 1 but nothing produced or transported
it. Phase 3 fills both halves.

**Producing (daemon).** The shell owns the alias table, so the client ships it;
the daemon annotates. A new fire-and-forget message carries it:

```json
{"v":1,"type":"aliases","session":"host.1234.…","map":{"gco":"git checkout","ll":"ls -la"}}
```

Sent on connect and whenever the table changes (detected in `precmd` by
comparing a joined snapshot — never in the keystroke path), capped at ~6 KB so
the write cannot block; the cap is documented and drops the overflow rather than
truncating an entry. Aliases are per-session state held in memory only — never
persisted, never logged at info level.

Annotation runs as a **post-merge decorator stage** in the engine, not as a
`Provider`: a provider produces its own candidates, while `alias-note` annotates
whatever the others produced. (`DESIGN.md §3.1` lists it under providers; this
is a deviation to record in the report — the interface shape is wrong for it,
and Phase 4's plugin notes will want the same stage.) It resolves the
candidate's first word through the session's alias map and sets
`Note = "gco = git checkout"`-style text; unknown words get no note. Only
regular aliases (`$aliases`) are covered; global/suffix aliases are out of
scope.

**Transporting (TSV).** A note travels in the same field as its candidate,
separated by US (`0x1f`):

```
42\tgco main\x1fgco = git checkout\tgit status\n
```

The escaping contract extends to escape a literal `0x1f` in text (as `\u`), so
the separator is unambiguous exactly like tab and newline already are. This
shape degrades cleanly in both directions: a Phase 2 daemon sends no separator
and the new client reads an empty note; a Phase 2 client never sets
`"notes":true` so a Phase 3 daemon sends it plain text. Notes are gated on that
request field precisely so an old client can never receive a byte it would
render as command text.

**Rendering (client).** The note appends to a row in the row's dim style,
visually separated: `  ▸ gco main    gco = git checkout`. Notes never appear in
the ghost (which must be exactly the accepted text) and are stripped from any
value that can reach `$BUFFER`.

## 6. Config additions

```toml
[providers]
transition = true     # start-time toggle, like the others
alias_note = true
[project]
markers = [".git", ".hg", "go.mod", "package.json", "Cargo.toml", ".project-root"]
```

Scoring weights stay Go constants (Phase 1 precedent: one place, documented
tunable, pinned by ordering tests).

## 7. Milestones (each = one commit, tests green before moving on)

- **M12 — Store (schema v2):** transitions table + `events(session,id)` index +
  migration with backfill from recorded sessions; ingest maintains transitions
  in-transaction; `PrefixStats` widened to project scope; `LastCommand(session)`;
  `TopNext(prev, limit)`. Tests: backfill excludes imports, pairs respect
  session boundaries, ingest updates counts, migration is idempotent and
  upgrades a v1 database in place.
- **M13 — Ranking & prediction:** `project` package (root resolution + cache),
  scoring project term, frecency fold, transition provider, config toggles,
  `serve.go` wiring. Tests: ordering (project beats global, cwd beats project),
  transition provider with and without `Recent`, marker walk-up and no-marker
  fallback.
- **M14 — Notes end-to-end (daemon):** note transport (escape `0x1f`, US
  sub-separator, `notes` request flag), `aliases` message, alias-note decorator,
  `docs/protocol.md`. Tests: round-trip escaping incl. a literal `0x1f`,
  old/new client-daemon degradation, decorator annotates first word only.
- **M15 — Client:** send `recent`, ship the alias map (connect + change,
  capped), `^O` on an empty buffer fetches transitions and opens the menu on
  arrival (dropping it if the user typed meanwhile), note rendering,
  `AFTO_EMPTY_ROWS`.
- **M16 — E2E + gates + report:** harness scenarios per §8, full gates,
  `plans/phase-3-report.md`, `DESIGN.md`/`CLAUDE.md` updates.

## 8. E2E additions (`tests/e2e/harness.zsh`)

Transitions cannot be seeded through `HISTFILE` (import is excluded by design),
so the harness must **execute** commands in the zpty shell to build the pair —
which also proves live ingestion feeds prediction end to end.

- **Empty-prompt menu:** run `true trans-first` then `true trans-second …`
  (twice, so the pair has count ≥ 1 and outranks noise); at a fresh empty
  prompt press `^O` → a menu appears containing `trans-second`; `Enter` accepts
  it into the buffer without executing; a second `Enter` runs it.
- **Empty prompt is silent by default:** at a fresh prompt with nothing typed,
  no `▸` row and no dim bytes appear until `^O` is pressed.
- **Ghost-accept cannot take a prediction:** at an empty prompt after `^O`
  rows exist; `Esc` out, then `^]` (accept) → nothing enters the buffer and the
  next `Enter` executes an empty line (prompt unchanged, no marker output).
- **Alias note renders:** define `alias tq='true q'` in the zpty shell, run
  `tq marker` once, type `tq` → the row shows the candidate followed by the
  expansion note; accepting with `^]` puts **only** `tq marker` in the buffer
  (assert via the executed command in the debug trace).
- Existing scenarios (rows, menu, TAB, daemon-kill, isolation grep, coexistence,
  `AFTO_DISABLE`) must stay green unchanged.

## 9. Acceptance gates

1. `make test` green; `go vet ./...` clean; isolation grep returns nothing.
2. `make e2e` green — every existing scenario plus §8.
3. `make bench` p99 keystroke→ghost still < 50 ms with the wider stats scan and
   project resolution on the hot path.
4. A v1 database (Phase 2 daemon's `afto.db`) opens under the new daemon,
   migrates in place, and keeps serving suggestions — verified by a test that
   builds a v1 schema, inserts rows, then opens it with the current code.
5. Old/new compatibility both directions for notes and `limit` (§5).
6. `DESIGN.md §5`, `CLAUDE.md`, and `docs/protocol.md` updated in the commits
   that change the behavior they describe; deviations in the report.

## 10. Left to implementer's judgment (decide and document)

Project-root cache size/TTL; whether the transition provider requires a minimum
pair count before suggesting; note format (`gco = git checkout` vs
`→ git checkout`); how many aliases the cap admits; whether `LastCommand`
consults events or an in-memory per-session map.
