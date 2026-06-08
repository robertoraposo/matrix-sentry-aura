package refine

import "testing"

// A periodic chain 1→2→3→1… is perfectly predictable by a first-order Markov
// model once warmed up, but the marginal (most-frequent) predictor cannot beat
// 1/3 — so lift is large and coverage approaches 1.
func TestStreamLiftPeriodicChain(t *testing.T) {
	var seq []int
	for i := 0; i < 40; i++ {
		seq = append(seq, 1, 2, 3)
	}
	r := StreamLift(seq)
	if r.MarkovHit < 0.95 {
		t.Errorf("expected near-perfect Markov hit on periodic chain, got %.3f", r.MarkovHit)
	}
	if r.Lift < 0.5 {
		t.Errorf("expected large lift on periodic chain, got %.3f", r.Lift)
	}
	if r.Coverage < 0.95 {
		t.Errorf("expected near-full coverage, got %.3f", r.Coverage)
	}
}

// A strictly increasing stream of distinct ids never revisits a state, so
// nothing is predictable from history: coverage and lift are ~0.
func TestStreamLiftNoStructure(t *testing.T) {
	var seq []int
	for i := 0; i < 50; i++ {
		seq = append(seq, i)
	}
	r := StreamLift(seq)
	if r.Coverage != 0 {
		t.Errorf("expected zero coverage on all-distinct stream, got %.3f", r.Coverage)
	}
	if r.Lift != 0 {
		t.Errorf("expected zero lift, got %.3f", r.Lift)
	}
}

func TestStreamLiftTooShort(t *testing.T) {
	if r := StreamLift([]int{7}); r.Total != 1 || r.Lift != 0 {
		t.Errorf("single-element stream should be inert, got %+v", r)
	}
}
