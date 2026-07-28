package ipc

import (
	"fmt"
	"strings"
)

// EncodeTSV renders a suggest response in TSV format: "<id>\t<escaped-text>\n".
//
// This format exists for exactly one consumer: the zsh plugin's zle -F
// response handler, which must parse a reply using only parameter expansion
// (no forks allowed in the keystroke path). It carries only the top
// candidate because ghost text can display only one suggestion anyway;
// clients that want the full ranked list use the default JSON format.
//
// Escaping contract (must match _afto_unescape in the zsh plugin): literal
// \t, \n and \ in the text become the two-character sequences \t, \n, \\ so
// that the first unescaped tab is always the id/text separator and an
// embedded newline can never break line framing. A multiline suggestion
// therefore survives transport intact — and is then rejected client-side by
// the prefix invariant, since the buffer is guarded to be single-line.
func EncodeTSV(id int64, text string) []byte {
	return []byte(fmt.Sprintf("%d\t%s\n", id, escapeTSV(text)))
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
