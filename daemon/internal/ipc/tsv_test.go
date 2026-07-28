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
	got := string(EncodeTSV(42, "a\tb\nc"))
	want := "42\ta\\tb\\nc\n"
	if got != want {
		t.Fatalf("EncodeTSV = %q, want %q", got, want)
	}
	if got[len(got)-1] != '\n' {
		t.Fatal("response must be newline-terminated")
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
