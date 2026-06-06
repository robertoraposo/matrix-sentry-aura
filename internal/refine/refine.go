// Package refine implements the access-gated residual-refinement experiment's
// novel core: a synthetic power-law access model over queries, per-item access
// popularity, hot-set gating by access budget, and the access-weighted recall
// metric. The base IVFADC geometry and PQ reconstruction live in internal/lab;
// this package is the part that has no analog in general ANN libraries — bits
// allocated by logged access frequency rather than geometry.
package refine

import (
	"math"
	"math/rand"
	"sort"
)

// ZipfWeights assigns each of n queries a weight 1/rank^s after a deterministic
// random permutation of ranks (seeded). The result is a permutation of the rank
// weights {1, 1/2^s, ...}: one query has weight 1.0, the rest taper. s=0 yields
// uniform weights (every query weight 1.0).
func ZipfWeights(n int, s float64, seed int64) []float64 {
	perm := rand.New(rand.NewSource(seed)).Perm(n)
	w := make([]float64, n)
	for q, rank := range perm {
		w[q] = math.Pow(float64(rank+1), -s) // rank 0 -> 1/1^s = 1
	}
	return w
}

// Popularity returns per-item access popularity: pop[i] = sum over queries q of
// weight(q) for every query whose true top-Rpop neighbours include item i. This
// is the synthetic "hit_count" an agent-memory access log would provide.
func Popularity(gt [][]int32, weights []float64, Rpop, ntotal int) []float64 {
	pop := make([]float64, ntotal)
	for q := range gt {
		w := weights[q]
		lim := Rpop
		if lim > len(gt[q]) {
			lim = len(gt[q])
		}
		for r := 0; r < lim; r++ {
			pop[gt[q][r]] += w
		}
	}
	return pop
}

// SelectHot marks the round(frac*n) highest-popularity items as hot (the items
// that get the refinement code under the access budget). Ties break to the lower
// index so the selection is deterministic.
func SelectHot(pop []float64, frac float64) []bool {
	n := len(pop)
	k := int(math.Round(frac * float64(n)))
	if k < 0 {
		k = 0
	}
	if k > n {
		k = n
	}
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		if pop[idx[a]] != pop[idx[b]] {
			return pop[idx[a]] > pop[idx[b]] // higher popularity first
		}
		return idx[a] < idx[b] // tie -> lower index
	})
	hot := make([]bool, n)
	for i := 0; i < k; i++ {
		hot[idx[i]] = true
	}
	return hot
}

// WeightedRecall is sum(weight*hit) / sum(weight) — realized recall under the
// access distribution. With uniform weights it reduces to plain recall.
func WeightedRecall(hit []bool, weights []float64) float64 {
	var num, den float64
	for i, h := range hit {
		den += weights[i]
		if h {
			num += weights[i]
		}
	}
	if den == 0 {
		return 0
	}
	return num / den
}
