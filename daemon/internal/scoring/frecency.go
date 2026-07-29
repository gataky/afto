// Package scoring holds afto's ranking math as pure functions — no I/O, no
// clock reads (callers pass ages in), so every behavior is table-testable.
//
// The frecency model (DESIGN.md §3.1, plans/phase-1.md §7, extended by
// plans/phase-3.md §3):
//
//	score = ln(1+count_all)·2^(-age_all/H)
//	      + W_cwd ·ln(1+count_cwd) ·2^(-age_cwd/H)
//	      + W_proj·ln(1+count_proj)·2^(-age_proj/H)
//
// First term: how often this command runs anywhere, decayed by how long
// since it last ran. Second and third: affinity boosts of the same shape
// for runs in the querying shell's directory and anywhere inside its
// project. The log dampens raw counts so an ancient command run 500 times
// cannot permanently drown a current one; the exponential halves a term's
// weight every H hours of disuse.
//
// Why three terms rather than two: the useful unit of "here" is usually the
// repository, but not always. `make test` belongs to the whole project;
// `terraform apply` belongs to the one directory you run it in. Keeping the
// directory term strictly stronger than the project term lets both be true
// at once — a command earns the project boost anywhere in the tree and an
// additional boost in the exact directory where it is a habit.
package scoring

import "math"

// Tunables. Deliberately few, deliberately in one place. Changing them
// re-ranks suggestions but never breaks correctness; tests pin relative
// ordering rather than exact float values so reasonable retuning doesn't
// churn the suite.
const (
	// HalfLifeHours is H above: a term loses half its weight per week of
	// disuse. Long enough that a weekly release script stays ranked, short
	// enough that last month's project fades behind this week's.
	HalfLifeHours = 168.0

	// CwdWeight is W_cwd: how strongly "you run this HERE" outranks "you
	// run this everywhere". 2.0 means a directory-local habit at equal
	// count/recency scores 3× a global one (1 + 2).
	CwdWeight = 2.0

	// ProjectWeight is W_proj: the boost for "you run this somewhere in
	// this project". Deliberately below CwdWeight — a project habit should
	// beat an unrelated global one but lose to the directory you are
	// standing in. At equal count/recency a command run in this exact
	// directory scores 4× global (1+2+1), elsewhere in the project 2×.
	ProjectWeight = 1.0
)

// Decay is the exponential recency factor for one term.
func Decay(ageHours float64) float64 {
	if ageHours < 0 {
		ageHours = 0 // clock skew: a "future" event just counts as fresh
	}
	return math.Exp2(-ageHours / HalfLifeHours)
}

// Term is one (count, age) pair feeding the score. A zero Count makes the
// term vanish, which is how "no runs in this directory" and "no project
// here" are expressed — callers never special-case them.
type Term struct {
	Count    int64
	AgeHours float64
}

// Frecency scores one command from its global, directory and project terms.
func Frecency(all, cwd, project Term) float64 {
	s := weighted(1.0, all)
	s += weighted(CwdWeight, cwd)
	s += weighted(ProjectWeight, project)
	return s
}

func weighted(w float64, t Term) float64 {
	if t.Count <= 0 {
		return 0
	}
	return w * math.Log1p(float64(t.Count)) * Decay(t.AgeHours)
}
