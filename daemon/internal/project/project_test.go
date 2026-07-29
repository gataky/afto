package project

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mkdirs(t *testing.T, base string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Join(base, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRootFindsNearestMarker(t *testing.T) {
	base := t.TempDir()
	mkdirs(t, base, "repo/.git", "repo/api/handlers", "repo/vendor/mod")
	// A nested module marker makes the nearer directory the project.
	if err := os.WriteFile(filepath.Join(base, "repo/vendor/mod/go.mod"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := New(nil)

	cases := []struct{ dir, want string }{
		{filepath.Join(base, "repo"), filepath.Join(base, "repo")},
		{filepath.Join(base, "repo/api/handlers"), filepath.Join(base, "repo")},
		{filepath.Join(base, "repo/vendor/mod"), filepath.Join(base, "repo/vendor/mod")},
	}
	for _, c := range cases {
		if got := r.Root(c.dir); got != c.want {
			t.Errorf("Root(%q) = %q, want %q", c.dir, got, c.want)
		}
	}
}

func TestRootAbsentIsEmpty(t *testing.T) {
	base := t.TempDir() // no markers anywhere up the tree from here
	mkdirs(t, base, "plain/deeper")
	r := New(nil)
	if got := r.Root(filepath.Join(base, "plain/deeper")); got != "" {
		t.Fatalf("want no project, got %q", got)
	}
	// Non-absolute and empty inputs are answered without touching disk.
	if got := r.Root("relative/path"); got != "" {
		t.Fatalf("want %q for a relative path, got %q", "", got)
	}
	if got := r.Root(""); got != "" {
		t.Fatalf("want %q for empty, got %q", "", got)
	}
}

func TestCustomMarkers(t *testing.T) {
	base := t.TempDir()
	mkdirs(t, base, "tree/sub")
	if err := os.WriteFile(filepath.Join(base, "tree/WORKSPACE"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := New(nil).Root(filepath.Join(base, "tree/sub")); got != "" {
		t.Fatalf("WORKSPACE is not a default marker; got %q", got)
	}
	if got := New([]string{"WORKSPACE"}).Root(filepath.Join(base, "tree/sub")); got != filepath.Join(base, "tree") {
		t.Fatalf("custom marker not honored: got %q", got)
	}
}

func TestCacheHitAndExpiry(t *testing.T) {
	base := t.TempDir()
	mkdirs(t, base, "repo/.git", "repo/sub")
	dir := filepath.Join(base, "repo/sub")

	clock := time.Unix(1_700_000_000, 0)
	r := New(nil)
	r.now = func() time.Time { return clock }

	if got := r.Root(dir); got != filepath.Join(base, "repo") {
		t.Fatalf("got %q", got)
	}
	// Remove the marker: a cached answer must still be served.
	if err := os.RemoveAll(filepath.Join(base, "repo/.git")); err != nil {
		t.Fatal(err)
	}
	if got := r.Root(dir); got != filepath.Join(base, "repo") {
		t.Fatalf("cache miss inside the TTL: got %q", got)
	}
	// Past the TTL the tree is consulted again.
	clock = clock.Add(2 * cacheTTL)
	if got := r.Root(dir); got != "" {
		t.Fatalf("stale entry served past TTL: got %q", got)
	}
}

func TestCacheOverflowKeepsAnswering(t *testing.T) {
	base := t.TempDir()
	mkdirs(t, base, "repo/.git")
	r := New(nil)
	for i := 0; i < maxCacheSize+10; i++ {
		sub := filepath.Join(base, "repo", "d"+string(rune('a'+i%26)), string(rune('a'+i/26)))
		mkdirs(t, "", sub)
		if got := r.Root(sub); got != filepath.Join(base, "repo") {
			t.Fatalf("Root(%q) = %q after %d entries", sub, got, i)
		}
	}
	if len(r.cache) > maxCacheSize {
		t.Fatalf("cache grew unbounded: %d entries", len(r.cache))
	}
}
