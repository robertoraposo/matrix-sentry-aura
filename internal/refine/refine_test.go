package refine

import (
	"math"
	"testing"
)

func TestZipfWeightsIsAPermutationOfRankWeights(t *testing.T) {
	n, s := 100, 1.0
	w := ZipfWeights(n, s, 42)
	if len(w) != n {
		t.Fatalf("len=%d want %d", len(w), n)
	}
	// Exactly one weight equals 1.0 (rank 1 => 1/1^s), all positive.
	ones, sum := 0, 0.0
	maxW := 0.0
	for _, x := range w {
		if x <= 0 {
			t.Fatal("non-positive weight")
		}
		if x == 1.0 {
			ones++
		}
		if x > maxW {
			maxW = x
		}
		sum += x
	}
	if ones != 1 {
		t.Fatalf("expected exactly one weight==1.0, got %d", ones)
	}
	if maxW != 1.0 {
		t.Fatalf("max weight=%v want 1.0", maxW)
	}
	// Deterministic for a fixed seed.
	w2 := ZipfWeights(n, s, 42)
	for i := range w {
		if w[i] != w2[i] {
			t.Fatal("ZipfWeights not deterministic")
		}
	}
	// Different seed -> different assignment (almost surely).
	w3 := ZipfWeights(n, s, 7)
	same := true
	for i := range w {
		if w[i] != w3[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("ZipfWeights ignored the seed")
	}
}

func TestZipfWeightsUniformWhenSZero(t *testing.T) {
	w := ZipfWeights(50, 0, 1)
	for i, x := range w {
		if x != 1.0 {
			t.Fatalf("s=0 must be uniform; w[%d]=%v", i, x)
		}
	}
}

func TestPopularityFollowsQueryWeight(t *testing.T) {
	// query 0 (weight 10) -> NN item 5; query 1 (weight 1) -> NN item 3.
	gt := [][]int32{{5, 9}, {3, 8}}
	weights := []float64{10, 1}
	pop := Popularity(gt, weights, 1 /*Rpop*/, 10 /*ntotal*/)
	if pop[5] <= pop[3] {
		t.Fatalf("hot item 5 (%.1f) should exceed item 3 (%.1f)", pop[5], pop[3])
	}
	if pop[5] != 10 || pop[3] != 1 {
		t.Fatalf("pop[5]=%.1f pop[3]=%.1f want 10 and 1", pop[5], pop[3])
	}
	if pop[0] != 0 {
		t.Fatalf("unreferenced item should have 0 popularity, got %.1f", pop[0])
	}
}

func TestPopularitySpreadsOverTopRpop(t *testing.T) {
	// Rpop=2 -> both gt[0][0] and gt[0][1] get the query's weight.
	gt := [][]int32{{5, 9}}
	pop := Popularity(gt, []float64{4}, 2, 10)
	if pop[5] != 4 || pop[9] != 4 {
		t.Fatalf("Rpop=2 should credit both NNs; pop[5]=%.1f pop[9]=%.1f", pop[5], pop[9])
	}
}

func TestSelectHotPicksHighestFraction(t *testing.T) {
	pop := []float64{0.1, 0.9, 0.5, 0.3, 0.7}
	hot := SelectHot(pop, 0.4) // round(0.4*5)=2 hot
	want := []bool{false, true, false, false, true}
	for i := range want {
		if hot[i] != want[i] {
			t.Fatalf("hot=%v want %v", hot, want)
		}
	}
}

func TestSelectHotTieBreaksToLowerIndex(t *testing.T) {
	pop := []float64{0.5, 0.5, 0.5, 0.1}
	hot := SelectHot(pop, 0.5) // 2 hot, three tied at 0.5 -> lowest two indices
	if !hot[0] || !hot[1] || hot[2] || hot[3] {
		t.Fatalf("tie should pick lowest indices; hot=%v", hot)
	}
}

func TestWeightedRecall(t *testing.T) {
	hit := []bool{true, false, true}
	if g := WeightedRecall(hit, []float64{1, 1, 2}); math.Abs(g-0.75) > 1e-9 {
		t.Fatalf("weighted=%v want 0.75", g)
	}
	if g := WeightedRecall(hit, []float64{1, 1, 1}); math.Abs(g-2.0/3.0) > 1e-9 {
		t.Fatalf("uniform=%v want 0.667", g)
	}
}
