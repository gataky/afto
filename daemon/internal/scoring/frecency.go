// Package scoring holds afto's ranking math as pure functions — no I/O, no
// clock reads (callers pass ages in), so every behavior is table-testable.
//
// The frecency model (DESIGN.md §3.1, plans/phase-1.md §7):
//
//	score = ln(1+count_all)·2^(-age_all/H)  +  W·ln(1+count_cwd)·2^(-age_cwd/H)
//
// First term: how often this command runs anywhere, decayed by how long
// since it last ran. Second term: the cwd-affinity boost — the same shape,
// weighted by W, computed from runs in the querying shell's directory. The
// log dampens raw counts so an ancient command run 500 times cannot
// permanently drown a current one; the exponential halves a term's weight
// every H hours of disuse.
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

	// CwdWeight is W above: how strongly "you run this HERE" outranks "you
	// run this everywhere". 2.0 means a directory-local habit at equal
	// count/recency scores 3× a global one (1 + 2).
	CwdWeight = 2.0
)

// Decay is the exponential recency factor for one term.
func Decay(ageHours float64) float64 {
	if ageHours < 0 {
		ageHours = 0 // clock skew: a "future" event just counts as fresh
	}
	return math.Exp2(-ageHours / HalfLifeHours)
}

// Frecency scores one command. countCwd/ageCwdHours describe runs in the
// querying shell's cwd; pass countCwd=0 when there are none (the cwd term
// vanishes).
func Frecency(countAll int64, ageAllHours float64, countCwd int64, ageCwdHours float64) float64 {
	s := math.Log1p(float64(countAll)) * Decay(ageAllHours)
	if countCwd > 0 {
		s += CwdWeight * math.Log1p(float64(countCwd)) * Decay(ageCwdHours)
	}
	return s
}
