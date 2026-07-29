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
	got := string(EncodeTSV(42, []string{"a\tb\nc"}))
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
	got := string(EncodeTSV(7, []string{"git checkout main", "a\tb", `a\b`}))
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

func TestUnescapeUnknownEscapeKeptVerbatim(t *testing.T) {
	if got := unescapeTSV(`a\xb`); got != `a\xb` {
		t.Fatalf("got %q", got)
	}
	if got := unescapeTSV(`trailing\`); got != `trailing\` {
		t.Fatalf("got %q", got)
	}
}
