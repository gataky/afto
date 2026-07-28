package scoring

import "testing"

// The suite pins relative ordering, not exact floats (plans/phase-1.md §7):
// retuning the constants shouldn't churn tests unless it inverts a behavior
// we rely on.

func TestMoreFrequentWinsAtEqualRecency(t *testing.T) {
	if Frecency(10, 1, 0, 0) <= Frecency(2, 1, 0, 0) {
		t.Fatal("higher count must outscore lower at equal age")
	}
}

func TestFresherWinsAtEqualCount(t *testing.T) {
	if Frecency(5, 1, 0, 0) <= Frecency(5, 24*30, 0, 0) {
		t.Fatal("recent use must outscore a month of disuse at equal count")
	}
}

func TestCwdAffinityBeatsGlobalHabit(t *testing.T) {
	// A command run 3× in THIS directory should outrank one run 10×
	// elsewhere (both fresh). This is the property that makes per-project
	// suggestions feel right.
	local := Frecency(3, 1, 3, 1)
	global := Frecency(10, 1, 0, 0)
	if local <= global {
		t.Fatalf("cwd affinity too weak: local=%f global=%f", local, global)
	}
}

func TestLogDampensRunawayCounts(t *testing.T) {
	// 500 ancient runs must not drown 5 runs from today.
	ancient := Frecency(500, 24*90, 0, 0)
	current := Frecency(5, 2, 0, 0)
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
