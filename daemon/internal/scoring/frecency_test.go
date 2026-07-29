package scoring

import "testing"

// The suite pins relative ordering, not exact floats (plans/phase-1.md §7):
// retuning the constants shouldn't churn tests unless it inverts a behavior
// we rely on.

// global/local/project build the three-term arguments readably.
func global(count int64, age float64) (Term, Term, Term) {
	return Term{count, age}, Term{}, Term{}
}

func TestMoreFrequentWinsAtEqualRecency(t *testing.T) {
	if Frecency(global(10, 1)) <= Frecency(global(2, 1)) {
		t.Fatal("higher count must outscore lower at equal age")
	}
}

func TestFresherWinsAtEqualCount(t *testing.T) {
	if Frecency(global(5, 1)) <= Frecency(global(5, 24*30)) {
		t.Fatal("recent use must outscore a month of disuse at equal count")
	}
}

func TestCwdAffinityBeatsGlobalHabit(t *testing.T) {
	// A command run 3× in THIS directory should outrank one run 10×
	// elsewhere (both fresh). This is the property that makes per-project
	// suggestions feel right.
	local := Frecency(Term{3, 1}, Term{3, 1}, Term{})
	if local <= Frecency(global(10, 1)) {
		t.Fatalf("cwd affinity too weak: local=%f", local)
	}
}

func TestProjectAffinityBeatsUnrelatedGlobalHabit(t *testing.T) {
	// Run 3× elsewhere in this project vs 6× scattered across unrelated
	// trees: being in the project should win.
	inProject := Frecency(Term{3, 1}, Term{}, Term{3, 1})
	if inProject <= Frecency(global(6, 1)) {
		t.Fatalf("project affinity too weak: project=%f", inProject)
	}
}

func TestDirectoryAffinityOutranksProjectAffinity(t *testing.T) {
	// Same command, same counts: the one that is a habit in THIS directory
	// must rank above the one that is merely a habit in the project.
	here := Frecency(Term{4, 1}, Term{4, 1}, Term{4, 1})
	elsewhereInProject := Frecency(Term{4, 1}, Term{}, Term{4, 1})
	if here <= elsewhereInProject {
		t.Fatalf("cwd term must dominate project term: here=%f project=%f", here, elsewhereInProject)
	}
}

func TestProjectTermVanishesOutsideAnyProject(t *testing.T) {
	// A directory in no project scores exactly as it did before Phase 3.
	if got, want := Frecency(Term{7, 3}, Term{2, 3}, Term{}), Frecency(Term{7, 3}, Term{2, 3}, Term{0, 99}); got != want {
		t.Fatalf("zero-count project term changed the score: %f vs %f", got, want)
	}
}

func TestLogDampensRunawayCounts(t *testing.T) {
	// 500 ancient runs must not drown 5 runs from today.
	ancient := Frecency(global(500, 24*90))
	current := Frecency(global(5, 2))
	if ancient >= current {
		t.Fatalf("staleness insufficiently penalized: ancient=%f current=%f", ancient, current)
	}
}

func TestDecayHalvesAtHalfLife(t *testing.T) {
	if got := Decay(HalfLifeHours); got < 0.499 || got > 0.501 {
		t.Fatalf("Decay(half-life) = %f, want 0.5", got)
	}
	if Decay(0) != 1.0 {
		t.Fatalf("Decay(0) = %f, want 1", Decay(0))
	}
}

func TestNegativeAgeTreatedAsFresh(t *testing.T) {
	if Decay(-5) != 1.0 {
		t.Fatal("clock skew must not produce weight > 1")
	}
}
