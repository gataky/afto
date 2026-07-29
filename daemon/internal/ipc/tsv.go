package ipc

import (
	"fmt"
	"strings"
)

// EncodeTSV renders a suggest response in TSV format:
// "<id>\t<escaped-text>[\t<escaped-text>…]\n".
//
// This format exists for exactly one consumer: the zsh plugin's zle -F
// response handler, which must parse a reply using only parameter expansion
// (no forks allowed in the keystroke path). Phase 1 carried only the top
// candidate; since Phase 2 it carries as many as the client asked for via
// the request's "limit" (the ghost still uses the first, the passive list
// rows use the rest). Clients that want scores/sources use the default JSON
// format.
//
// Escaping contract (must match _afto_tsv_unescape in the zsh plugin):
// literal \t, \n and \ in the text become the two-character sequences \t,
// \n, \\ — so every real tab byte on the line is a field separator, and an
// embedded newline can never break line framing. That is what lets the
// client split the whole line on plain tab before unescaping each field.
// A multiline suggestion therefore survives transport intact — and is then
// rejected client-side by the prefix invariant, since the buffer is guarded
// to be single-line.
//
// Zero candidates encode as "<id>\t\n": the line always has at least one
// (possibly empty) text field, which Phase 1 clients relied on.
func EncodeTSV(id int64, texts []string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "%d", id)
	if len(texts) == 0 {
		b.WriteByte('\t')
	}
	for _, t := range texts {
		b.WriteByte('\t')
		b.WriteString(escapeTSV(t))
	}
	b.WriteByte('\n')
	return []byte(b.String())
}

func escapeTSV(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`)
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
		case '\\':
			b.WriteByte('\\')
		default: // unknown escape: keep verbatim
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
