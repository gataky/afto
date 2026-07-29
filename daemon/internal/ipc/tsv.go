package ipc

import (
	"fmt"
	"strings"
)

// US is the sub-separator that joins a candidate to its note inside one
// TSV field. Escaping (below) removes it from text, so its presence is
// unambiguous — and its absence is what makes an annotated response
// readable by a client that knows nothing about notes.
const US = "\x1f"

// EncodeTSV renders a suggest response in TSV format:
// "<id>\t<field>[\t<field>…]\n", where a field is "<text>" or, when the
// client asked for notes, "<text>\x1f<note>".
//
// This format exists for exactly one consumer: the zsh plugin's zle -F
// response handler, which must parse a reply using only parameter expansion
// (no forks allowed in the keystroke path). Phase 1 carried only the top
// candidate; since Phase 2 it carries as many as the client asked for via
// the request's "limit" (the ghost still uses the first, the passive list
// rows use the rest); since Phase 3 each may carry a note. Clients that
// want scores/sources use the default JSON format.
//
// Escaping contract (must match _afto_tsv_unescape in the zsh plugin):
// literal \t, \n, \x1f and \ in the text become the two-character sequences
// \t, \n, \u, \\ — so every real tab byte on the line is a field separator
// and every real \x1f is a text/note separator. That is what lets the
// client split the line on plain tab, then each field on plain \x1f, before
// unescaping. An embedded newline can never break line framing: a multiline
// suggestion survives transport intact and is then rejected client-side by
// the prefix invariant, since the buffer is guarded to be single-line.
//
// Zero candidates encode as "<id>\t\n": the line always has at least one
// (possibly empty) text field, which Phase 1 clients relied on.
func EncodeTSV(id int64, cs []TSVCandidate) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "%d", id)
	if len(cs) == 0 {
		b.WriteByte('\t')
	}
	for _, c := range cs {
		b.WriteByte('\t')
		b.WriteString(escapeTSV(c.Text))
		// A note is omitted entirely rather than sent empty, so a reply
		// with no notes is byte-identical to a Phase 2 one.
		if c.Note != "" {
			b.WriteString(US)
			b.WriteString(escapeTSV(c.Note))
		}
	}
	b.WriteByte('\n')
	return []byte(b.String())
}

// TSVCandidate is one candidate on its way to the wire. Escaping happens
// inside EncodeTSV rather than at call sites: forgetting it for one field
// would corrupt the framing of the whole line.
type TSVCandidate struct {
	Text string
	Note string // omitted from the wire when empty
}

func escapeTSV(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, US, `\u`)
	return r.Replace(s)
}

// unescapeTSV reverses escapeTSV. Left-to-right scan: naive Replace would
// mis-decode sequences like `\\t`.
func unescapeTSV(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'u':
			b.WriteString(US)
		case '\\':
			b.WriteByte('\\')
		default: // unknown escape: keep verbatim
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
