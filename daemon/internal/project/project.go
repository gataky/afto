// Package project answers one question: which project does this directory
// belong to?
//
// It exists because the useful unit of "where you are" is the repository,
// not the directory. A command you ran in ~/work/app/api is a good
// suggestion in ~/work/app/web — same project, same habits — while the same
// command from an unrelated tree is not. The frecency score gets a project
// term from this (scoring.Frecency), and the answer is just the nearest
// ancestor directory holding a marker like .git.
//
// The resolution runs on the keystroke path, so results are cached: a miss
// costs a handful of stat calls up the tree, a hit costs a map lookup. The
// cache is time-bounded rather than invalidated, since the only thing that
// can change an answer is creating or removing a marker — rare, and being
// briefly stale costs a slightly worse ranking, nothing more.
package project

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultMarkers are the files/directories that mark a project root. Order
// is irrelevant — the nearest ancestor holding ANY of them wins, so a Go
// module inside a git repo resolves to the module directory.
var DefaultMarkers = []string{".git", ".hg", "go.mod", "package.json", "Cargo.toml", ".project-root"}

const (
	cacheTTL     = 5 * time.Minute
	maxCacheSize = 512 // plenty for a human's directory habits; see evict
	maxDepth     = 64  // stop walking pathological trees / symlink games
)

// Resolver maps directories to project roots, with a small TTL cache.
// Safe for concurrent use: every suggest request may hit it.
type Resolver struct {
	mu      sync.Mutex
	markers []string
	cache   map[string]entry
	now     func() time.Time // injectable for tests
}

type entry struct {
	root string
	at   time.Time
}

// New returns a resolver for the given markers; empty markers means
// DefaultMarkers. Passing an explicitly empty list in config therefore
// cannot silently disable the feature — use the provider toggle for that.
func New(markers []string) *Resolver {
	m := markers
	if len(m) == 0 {
		m = DefaultMarkers
	}
	return &Resolver{markers: m, cache: map[string]entry{}, now: time.Now}
}

// Root returns the project root containing dir, or "" if there is none.
// A "" result is cached too: directories outside any project are exactly
// the ones that would otherwise re-walk to / on every keystroke.
func (r *Resolver) Root(dir string) string {
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	if root, ok := r.lookup(dir); ok {
		return root
	}
	root := r.walk(dir)
	r.store(dir, root)
	return root
}

func (r *Resolver) lookup(dir string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.cache[dir]
	if !ok || r.now().Sub(e.at) > cacheTTL {
		return "", false
	}
	return e.root, true
}

func (r *Resolver) store(dir, root string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cache) >= maxCacheSize {
		// Crude but adequate: the cache is a latency optimization, not a
		// correctness mechanism, so dropping everything on overflow costs
		// one slow keystroke rather than deserving an LRU.
		r.cache = map[string]entry{}
	}
	r.cache[dir] = entry{root: root, at: r.now()}
}

func (r *Resolver) walk(dir string) string {
	d := filepath.Clean(dir)
	for i := 0; i < maxDepth; i++ {
		for _, m := range r.markers {
			if _, err := os.Lstat(filepath.Join(d, m)); err == nil {
				return d
			}
		}
		parent := filepath.Dir(d)
		if parent == d { // reached the filesystem root
			return ""
		}
		d = parent
	}
	return ""
}
