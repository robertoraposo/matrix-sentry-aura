package refine

import (
	"reflect"
	"testing"
)

// A self-exciting stream interpolates between i.i.d. draws from a popularity bag
// (eta=0) and a deterministic successor chain (eta=1). The two limits must hold
// exactly so the experiment's branching knob means what it claims.
func TestSelfExcitingStreamLimits(t *testing.T) {
	bag := []int{2, 2, 5, 7} // popularity multiset (token 2 is "popular")
	successor := []int{1, 2, 3, 4, 5, 6, 7, 0}

	// eta=0: pure i.i.d. — every token is drawn from the bag's value set.
	in := map[int]bool{2: true, 5: true, 7: true}
	s0 := SelfExcitingStream(200, 0, bag, successor, 1)
	if len(s0) != 200 {
		t.Fatalf("len=%d want 200", len(s0))
	}
	for _, x := range s0 {
		if !in[x] {
			t.Fatalf("eta=0 produced %d not in bag", x)
		}
	}

	// eta=1: after the first token, each is the successor of the previous.
	s1 := SelfExcitingStream(50, 1, bag, successor, 1)
	for i := 1; i < len(s1); i++ {
		if s1[i] != successor[s1[i-1]] {
			t.Fatalf("eta=1 step %d: %d != successor[%d]=%d", i, s1[i], s1[i-1], successor[s1[i-1]])
		}
	}

	// deterministic for a fixed seed.
	if !reflect.DeepEqual(SelfExcitingStream(80, 0.5, bag, successor, 9), SelfExcitingStream(80, 0.5, bag, successor, 9)) {
		t.Fatal("SelfExcitingStream not deterministic")
	}
}

// The online Markov predictor must rank a state's observed successors by how
// often they followed it — the basis of "refine what's about to be accessed."
func TestMarkovPredictsByTransitionFrequency(t *testing.T) {
	m := NewMarkov()
	for i := 0; i < 5; i++ {
		m.Observe(0, 5)
	}
	m.Observe(0, 3)
	m.Observe(9, 8)

	if got := m.Predict(0, 1); !reflect.DeepEqual(got, []int{5}) {
		t.Fatalf("Predict(0,1)=%v want [5]", got)
	}
	if got := m.Predict(0, 2); !reflect.DeepEqual(got, []int{5, 3}) {
		t.Fatalf("Predict(0,2)=%v want [5 3]", got)
	}
	// Unseen state yields no prediction.
	if got := m.Predict(42, 3); len(got) != 0 {
		t.Fatalf("Predict(unseen)=%v want empty", got)
	}
	// Ties broken by lower token id for determinism.
	m.Observe(1, 7)
	m.Observe(1, 4)
	if got := m.Predict(1, 2); !reflect.DeepEqual(got, []int{4, 7}) {
		t.Fatalf("Predict(1,2)=%v want [4 7] (tie->lower id)", got)
	}
}
