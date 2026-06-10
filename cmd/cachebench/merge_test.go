package main

import (
	"testing"

	"matrixsentry/pq"
)

// A tier item that also appears in the ADC shortlist must collapse to ONE
// entry carrying the exact distance (exact wins over the ADC estimate).
func TestMergeTieredReplacesADCWithExact(t *testing.T) {
	base := []pq.Result{{ID: 5, Dist: 1.0}, {ID: 7, Dist: 2.0}, {ID: 9, Dist: 3.0}}
	tier := map[int]float32{7: 0.5, 11: 2.5}

	got := mergeTiered(base, tier, 3)

	want := []pq.Result{{ID: 7, Dist: 0.5}, {ID: 5, Dist: 1.0}, {ID: 11, Dist: 2.5}}
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Dist != want[i].Dist {
			t.Errorf("got[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// topK truncates after the merge, and equal distances break ties to the lower
// ID (determinism, the engine-wide convention).
func TestMergeTieredTopKAndTies(t *testing.T) {
	base := []pq.Result{{ID: 3, Dist: 1.0}, {ID: 1, Dist: 1.0}}
	tier := map[int]float32{2: 1.0}

	got := mergeTiered(base, tier, 2)

	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Errorf("ties must break to lower ID: got %v, want IDs [1 2]", got)
	}
}

// An empty tier must return the base shortlist unchanged (no-tier == baseline).
func TestMergeTieredEmptyTierIsIdentity(t *testing.T) {
	base := []pq.Result{{ID: 4, Dist: 0.5}, {ID: 2, Dist: 1.5}}

	got := mergeTiered(base, nil, 2)

	if len(got) != 2 || got[0].ID != 4 || got[1].ID != 2 {
		t.Errorf("empty tier must be identity: got %v", got)
	}
}
