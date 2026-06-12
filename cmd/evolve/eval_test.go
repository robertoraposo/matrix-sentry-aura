package main

import (
	"math/rand"
	"testing"

	"matrixsentry/internal/evolve"
	"matrixsentry/internal/lab"
)

// tiny clustered dataset, dim 16, with exact ground truth computed in-test.
func tinyData(n, nq int) (base, queries [][]float32, gt [][]int32) {
	rng := rand.New(rand.NewSource(99))
	centers := make([][]float32, 8)
	for i := range centers {
		c := make([]float32, 16)
		for j := range c {
			c[j] = rng.Float32() * 10
		}
		centers[i] = c
	}
	mk := func() []float32 {
		c := centers[rng.Intn(len(centers))]
		v := make([]float32, 16)
		for j := range v {
			v[j] = c[j] + float32(rng.NormFloat64())*0.5
		}
		return v
	}
	for i := 0; i < n; i++ {
		base = append(base, mk())
	}
	for i := 0; i < nq; i++ {
		queries = append(queries, mk())
	}
	for _, q := range queries {
		bestID, bestD := -1, 0.0
		for i, b := range base {
			if d := lab.SqL2(q, b); bestID == -1 || d < bestD {
				bestID, bestD = i, d
			}
		}
		gt = append(gt, []int32{int32(bestID)})
	}
	return
}

func tinyTuner(lmax float64) *tuner {
	base, queries, gt := tinyData(600, 50)
	idx := make([]int, len(queries))
	for i := range idx {
		idx[i] = i
	}
	grid := []evolve.Gene{
		{Name: "nlist", Values: []int{4}},
		{Name: "m", Values: []int{4}},
		{Name: "k", Values: []int{16}},
		{Name: "probeDen", Values: []int{1}}, // nprobe = nlist: scan everything
		{Name: "rerankk", Values: []int{0, 600}},
	}
	return newTuner(grid, base, base, queries, gt, idx, 16, lmax)
}

func TestRerankEverythingIsExactSearch(t *testing.T) {
	tn := tinyTuner(1e18)
	// rerankK=600 >= N: every ADC candidate is exact-re-ranked, so the true NN
	// must occupy rank 1 and recall@10 == 1.0 regardless of PQ distortion.
	fit := tn.fitness([]int{0, 0, 0, 0, 1})
	if fit != 1.0 {
		t.Fatalf("exact re-rank of the full base must give recall 1.0, got %f", fit)
	}
}

func TestRerankNeverHurtsPureADC(t *testing.T) {
	tn := tinyTuner(1e18)
	adc := tn.fitness([]int{0, 0, 0, 0, 0})
	rr := tn.fitness([]int{0, 0, 0, 0, 1})
	if adc < 0 || adc > 1 {
		t.Fatalf("ADC recall out of range: %f", adc)
	}
	if rr < adc {
		t.Fatalf("exact re-rank degraded recall: %f < %f", rr, adc)
	}
}

func TestFitnessDeterministic(t *testing.T) {
	a := tinyTuner(1e18).fitness([]int{0, 0, 0, 0, 1})
	b := tinyTuner(1e18).fitness([]int{0, 0, 0, 0, 1})
	if a != b {
		t.Fatalf("fitness not deterministic: %f vs %f", a, b)
	}
}

func TestInfeasibleScoredWithoutTraining(t *testing.T) {
	tn := tinyTuner(1) // impossible latency budget
	fit := tn.fitness([]int{0, 0, 0, 0, 0})
	if fit >= 0 {
		t.Fatalf("infeasible genome must score negative violation, got %f", fit)
	}
	if tn.trainings != 0 {
		t.Fatalf("infeasible genome trained geometry %d times, want 0", tn.trainings)
	}
}

func TestGeometryCachedAcrossSearchKnobs(t *testing.T) {
	tn := tinyTuner(1e18)
	tn.fitness([]int{0, 0, 0, 0, 0})
	tn.fitness([]int{0, 0, 0, 0, 1}) // same (nlist,M,K): must reuse geometry
	if tn.trainings != 1 {
		t.Fatalf("geometry trained %d times for one geomKey, want 1", tn.trainings)
	}
}
