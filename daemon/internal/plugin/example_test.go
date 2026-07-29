package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gataky/afto/daemon/internal/provider"
)

// The claim in docs/plugins.md is that a plugin can be a dependency-free
// shell script. A claim in documentation that nothing executes is a claim
// that quietly stops being true, so the shipped example is run here — as a
// real subprocess, through the production host, in the language it
// advertises. It lives in this package because examples/ cannot import
// daemon/internal/.
func TestShippedShellExampleWorks(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("..", "..", "..", "examples", "plugins", "afto-echo.sh"))
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Fatal("example plugin is not executable; a user copying it would get exec format errors")
	}

	h := host(t, path, 5*time.Second)

	got, err := h.Suggest(context.Background(), provider.Query{Buffer: "git status ", Cursor: 11})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "git status --help" {
		t.Fatalf("unexpected candidates: %+v", got)
	}
	if got[0].Note != "from afto-echo.sh" {
		t.Fatalf("metadata: %+v", got[0])
	}

	// An empty buffer must yield a well-formed EMPTY answer, not silence:
	// the difference between "nothing to suggest" and "plugin broken" is
	// what keeps the breaker from benching a working plugin.
	if got, err := h.Suggest(context.Background(), provider.Query{}); err != nil || len(got) != 0 {
		t.Fatalf("empty buffer: got %+v err=%v", got, err)
	}
	if h.failures != 0 {
		t.Fatalf("a valid empty answer was counted as a failure (%d)", h.failures)
	}
}
