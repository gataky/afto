// Package ipc implements the afto wire protocol: JSON-lines over a unix
// socket, with an optional TSV response format. See plans/phase-1.md §5 for
// the authoritative spec and docs/protocol.md for the narrative version
// (round-trip walkthrough, flow control, evolution rules).
//
// # Who connects
//
// A "client" is anything that dials the daemon's unix socket and speaks this
// protocol. In Phase 1 there are two:
//
//   - The zsh plugin (shell/zsh/afto.plugin.zsh). Each interactive shell
//     session holds one long-lived connection, opened from inside the shell
//     process via zsh's builtin zsocket — no helper subprocess. It sends
//     "suggest" as the user types and fire-and-forget "record" after each
//     command finishes. The Session field distinguishes shells, since one
//     daemon serves every terminal the user has open.
//
//   - The aftod CLI in its "query"/"ping" subcommands: the same binary acting
//     as a one-shot diagnostic client for humans and tests.
//
// Later phases add more clients (bash integration, fzf widget). Phase 4
// provider plugins are NOT clients: the daemon spawns those as subprocesses
// and talks to them over stdio.
//
// # Why JSON-lines, and why a TSV mode
//
// One JSON object per newline-terminated line keeps framing trivial for
// every client and keeps the protocol greppable in logs. The exception is
// the suggest *response* when the client asks for fmt:"tsv": zsh cannot
// parse JSON without forking a helper process, which is forbidden in the
// keystroke path, so the ZLE client receives "<id>\t<escaped text>\n"
// instead — parseable with two parameter expansions. JSON remains the
// default response format for every other consumer.
//
// # Error philosophy
//
// The tool's contract is that failure manifests as the absence of a
// suggestion, never as noise or latency in the user's terminal. So the
// server drops malformed lines and unknown message types (logging at debug)
// and keeps the connection open; it never sends error replies that clients
// would have to handle mid-keystroke.
package ipc

import "github.com/gataky/afto/daemon/internal/provider"

// Protocol version. Every message carries "v".
const V = 1

// MaxLine bounds a single protocol line; longer lines are dropped.
const MaxLine = 1 << 20

// Message types.
const (
	TypeSuggest = "suggest"
	TypeRecord  = "record"
	TypePing    = "ping"
	TypeAliases = "aliases"
)

// Response formats for suggest.
const (
	FmtJSON = "json"
	FmtTSV  = "tsv"
)

// Request is any client→daemon message; Type discriminates which fields
// are meaningful. A single struct (rather than one type per message) keeps
// decoding to one json.Unmarshal on the hot path and mirrors how the zsh
// client builds requests — by string interpolation into a template.
//
// Fields for "suggest": ID correlates the response to the request so a
// client can discard stale replies (the zsh client bumps it every request
// and ignores mismatches). Limit is how many candidates the client wants
// back (clamped to provider.CandidateLimit; absent means the format's
// historical default — 1 for TSV, everything for JSON — so Phase 1 clients
// and `aftod query` behave unchanged). Buffer/Cursor are the live command
// line. CWD,
// LastExit, Session and Recent exist so providers can rank by context —
// they are already sufficient for the future AI provider, which is what
// keeps that addition protocol-compatible.
//
// Fields for "record": Cmd is the command line as executed, Exit its status,
// TS the unix time it finished. Record has no response by design: the shell
// must never wait on ingestion.
type Request struct {
	V    int    `json:"v"`
	Type string `json:"type"`

	// suggest
	ID       int64    `json:"id,omitempty"`
	Fmt      string   `json:"fmt,omitempty"`
	Limit    int      `json:"limit,omitempty"`
	Notes    bool     `json:"notes,omitempty"`
	Buffer   string   `json:"buffer,omitempty"`
	Cursor   int      `json:"cursor,omitempty"`
	CWD      string   `json:"cwd,omitempty"`
	LastExit int      `json:"last_exit,omitempty"`
	Session  string   `json:"session,omitempty"`
	Recent   []string `json:"recent,omitempty"`

	// record
	Cmd  string `json:"cmd,omitempty"`
	Exit int    `json:"exit,omitempty"`
	TS   int64  `json:"ts,omitempty"`

	// aliases: the shell's alias table, so the daemon can annotate
	// candidates with what they expand to. Per-session, in memory only,
	// never persisted — see the alias-note decorator.
	Map map[string]string `json:"map,omitempty"`
}

// Query converts a suggest request to the domain query.
func (r Request) Query() provider.Query {
	return provider.Query{
		Buffer:   r.Buffer,
		Cursor:   r.Cursor,
		CWD:      r.CWD,
		LastExit: r.LastExit,
		Session:  r.Session,
		Recent:   r.Recent,
	}
}

// Candidate is the wire form of provider.Candidate.
type Candidate struct {
	Text   string  `json:"text"`
	Score  float64 `json:"score"`
	Source string  `json:"source"`
	Note   string  `json:"note,omitempty"`
}

// SuggestResponse is the JSON-format suggest reply.
type SuggestResponse struct {
	V          int         `json:"v"`
	ID         int64       `json:"id"`
	Candidates []Candidate `json:"candidates"`
}

// PingResponse is the reply to a ping.
type PingResponse struct {
	V       int    `json:"v"`
	OK      bool   `json:"ok"`
	Version string `json:"version"`
}

func toWire(cs []provider.Candidate) []Candidate {
	out := make([]Candidate, 0, len(cs))
	for _, c := range cs {
		out = append(out, Candidate{Text: c.Text, Score: c.Score, Source: c.Source, Note: c.Note})
	}
	return out
}
