package ipc

import "testing"

func TestTSVRoundTrip(t *testing.T) {
	cases := []struct {
		name, text, wire string
	}{
		{"plain", "git checkout main", "git checkout main"},
		{"empty", "", ""},
		{"tab", "a\tb", `a\tb`},
		{"newline", "a\nb", `a\nb`},
		{"backslash", `a\b`, `a\\b`},
		{"backslash-t", `a\` + "\t" + `b`, `a\\\tb`},
		{"literal-backslash-t", `a\tb`, `a\\tb`},
		{"unicode", "ls ~/日本語/ファイル", "ls ~/日本語/ファイル"},
		// A literal unit separator in a command must not look like the
		// text/note separator on the wire.
		{"unit-separator", "printf 'a\x1fb'", `printf 'a\ub'`},
		{"backslash-u", `a\ub`, `a\\ub`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := escapeTSV(c.text)
			if got != c.wire {
				t.Fatalf("escape(%q) = %q, want %q", c.text, got, c.wire)
			}
			back := unescapeTSV(got)
			if back != c.text {
				t.Fatalf("unescape(escape(%q)) = %q", c.text, back)
			}
		})
	}
}

func TestEncodeTSVFraming(t *testing.T) {
	got := string(EncodeTSV(42, []TSVCandidate{{Text: "a\tb\nc"}}))
	want := "42\ta\\tb\\nc\n"
	if got != want {
		t.Fatalf("EncodeTSV = %q, want %q", got, want)
	}
	if got[len(got)-1] != '\n' {
		t.Fatal("response must be newline-terminated")
	}
}

func TestEncodeTSVMultiCandidate(t *testing.T) {
	// Every real tab byte on the wire must be a separator: candidates with
	// literal tabs/newlines/backslashes arrive escaped, so a client that
	// splits the line on plain tab gets exactly the fields back.
	got := string(EncodeTSV(7, []TSVCandidate{{Text: "git checkout main"}, {Text: "a\tb"}, {Text: `a\b`}}))
	want := "7\tgit checkout main\ta\\tb\ta\\\\b\n"
	if got != want {
		t.Fatalf("EncodeTSV = %q, want %q", got, want)
	}
}

func TestEncodeTSVZeroCandidates(t *testing.T) {
	// The empty response keeps its Phase 1 shape: one empty text field.
	if got := string(EncodeTSV(9, nil)); got != "9\t\n" {
		t.Fatalf("EncodeTSV = %q, want %q", got, "9\t\n")
	}
}

func TestEncodeTSVNotes(t *testing.T) {
	// No note: byte-identical to a Phase 2 line, which is what lets an old
	// client read a new daemon's reply.
	if got := string(EncodeTSV(1, []TSVCandidate{{Text: "gco main"}})); got != "1\tgco main\n" {
		t.Fatalf("got %q", got)
	}
	got := string(EncodeTSV(1, []TSVCandidate{{Text: "gco main", Note: "gco = git checkout"}}))
	if got != "1\tgco main\x1fgco = git checkout\n" {
		t.Fatalf("got %q", got)
	}
	// Both halves are escaped, so the separators stay unambiguous even
	// when the text or the note contains one.
	got = string(EncodeTSV(1, []TSVCandidate{{Text: "printf 'a\x1fb'", Note: "x\ty"}}))
	if want := "1\t" + `printf 'a\ub'` + "\x1f" + `x\ty` + "\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestUnescapeUnknownEscapeKeptVerbatim(t *testing.T) {
	if got := unescapeTSV(`a\xb`); got != `a\xb` {
		t.Fatalf("got %q", got)
	}
	if got := unescapeTSV(`trailing\`); got != `trailing\` {
		t.Fatalf("got %q", got)
	}
}
