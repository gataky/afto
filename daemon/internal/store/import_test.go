package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeHist(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "zsh_history")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestImportExtendedFormat(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	hist := writeHist(t, []byte(
		": 1722000000:0;git status\n"+
			": 1722000010:2;git status\n"+
			": 1722000020:0;make build\n"+
			": 1722000030:0;export API_TOKEN=shh\n"))

	st, err := s.ImportHistfile(ctx, hist)
	if err != nil {
		t.Fatal(err)
	}
	if st.Commands != 4 || st.Imported != 3 || st.Redacted != 1 {
		t.Fatalf("stats = %+v", st)
	}

	rows, err := s.PrefixStats(ctx, "git status", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Count != 2 || rows[0].LastTS != 1722000010 {
		t.Fatalf("git status rollup = %+v", rows)
	}
}

func TestImportMultilineExtended(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	hist := writeHist(t, []byte(
		": 1722000000:0;for f in *; do\n"+
			"  echo $f\n"+
			"done\n"+
			": 1722000010:0;ls\n"))

	st, err := s.ImportHistfile(ctx, hist)
	if err != nil {
		t.Fatal(err)
	}
	if st.Commands != 2 || st.Imported != 2 {
		t.Fatalf("stats = %+v", st)
	}
	rows, err := s.PrefixStats(ctx, "for f", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := "for f in *; do\n  echo $f\ndone"
	if len(rows) != 1 || rows[0].Cmd != want {
		t.Fatalf("multiline command mangled: %+v", rows)
	}
}

func TestImportPlainFormatPreservesRelativeRecency(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	hist := writeHist(t, []byte("make old\nmake new\n"))

	if _, err := s.ImportHistfile(ctx, hist); err != nil {
		t.Fatal(err)
	}
	rows, err := s.MostRecentPrefix(ctx, "make", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Cmd != "make new" || rows[1].Cmd != "make old" {
		t.Fatalf("relative recency lost: %+v", rows)
	}
}

func TestImportUnmetafiesMultibyte(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	// Metafy "ls 日" the way zsh writes it: bytes >= 0x80 become 0x83, b^0x20.
	orig := "ls 日"
	var metafied []byte
	for _, b := range []byte(orig) {
		if b >= 0x80 {
			metafied = append(metafied, 0x83, b^0x20)
		} else {
			metafied = append(metafied, b)
		}
	}
	hist := writeHist(t, append([]byte(": 1722000000:0;"), append(metafied, '\n')...))

	if _, err := s.ImportHistfile(ctx, hist); err != nil {
		t.Fatal(err)
	}
	rows, err := s.PrefixStats(ctx, "ls ", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Cmd != orig {
		t.Fatalf("metafied command corrupted: %+v", rows)
	}
}

func TestImportEmptyFile(t *testing.T) {
	s := open(t)
	st, err := s.ImportHistfile(context.Background(), writeHist(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if st.Commands != 0 || st.Imported != 0 {
		t.Fatalf("stats = %+v", st)
	}
}
