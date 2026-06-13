package main

import "math"

// Depth grids the policy may choose from. nprobe is capped to nlist at use.
var (
	nprobeGrid = []int{1, 2, 4, 8, 16, 32, 64}
	rerankGrid = []int{0, 50, 100, 200, 400}
)

// snap clamps v to [grid[0], grid[last]] and returns the nearest grid value.
func snap(grid []int, v int) int {
	if v <= grid[0] {
		return grid[0]
	}
	if v >= grid[len(grid)-1] {
		return grid[len(grid)-1]
	}
	best, bestd := grid[0], 1<<30
	for _, g := range grid {
		d := g - v
		if d < 0 {
			d = -d
		}
		if d < bestd {
			bestd, best = d, g
		}
	}
	return best
}

// queryProxy is the deterministic per-query cost (float-op units) of a search at
// (nprobe, rerankK): ADC over the probed lists (balanced-list estimate
// n*nprobe/nlist candidates) plus exact L2 (2*dim ops) over rerankK candidates.
// Mirrors cmd/evolve's CostProxy shape; wall-clock is never used in evolution.
func queryProxy(nprobe, rerankK, dim, n, nlist int) float64 {
	scanned := float64(n) * float64(nprobe) / float64(nlist)
	return scanned + float64(rerankK)*2*float64(dim)
}

// recallHit is the repo's recall@10 hitIn semantics for one query: 1.0 if the
// true nearest neighbor (gt0) is among the returned ids, else 0.0.
func recallHit(rankedIDs []int32, gt0 int32) float64 {
	for _, id := range rankedIDs {
		if id == gt0 {
			return 1.0
		}
	}
	return 0.0
}

// pearson returns the Pearson correlation coefficient of xs and ys.
// Returns 0 when the standard deviation of xs or ys is zero (constant slices).
func pearson(xs, ys []float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	var sumX, sumY float64
	for i := 0; i < n; i++ {
		sumX += xs[i]
		sumY += ys[i]
	}
	meanX := sumX / float64(n)
	meanY := sumY / float64(n)

	var cov, varX, varY float64
	for i := 0; i < n; i++ {
		dx := xs[i] - meanX
		dy := ys[i] - meanY
		cov += dx * dy
		varX += dx * dx
		varY += dy * dy
	}
	if varX == 0 || varY == 0 {
		return 0
	}
	return cov / math.Sqrt(varX*varY)
}

// exactNN returns the index of the base vector nearest to q by squared L2.
func exactNN(q []float32, base [][]float32) int32 {
	best := int32(0)
	bestD := sqL2f(q, base[0])
	for i := 1; i < len(base); i++ {
		if d := sqL2f(q, base[i]); d < bestD {
			bestD, best = d, int32(i)
		}
	}
	return best
}

// sqL2f returns the squared L2 distance between a and b (float32 vectors).
func sqL2f(a, b []float32) float64 {
	var s float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		s += d * d
	}
	return s
}

// idsOf extracts base-vector ids from a slice of ivf.Hit via idByHash lookup.
// Hits whose hash has no entry in idByHash are silently skipped.
func idsOf(hits []hit, idByHash map[uint64]int32) []int32 {
	out := make([]int32, 0, len(hits))
	for _, h := range hits {
		if id, ok := idByHash[h.hash]; ok {
			out = append(out, id)
		}
	}
	return out
}

// hit is a minimal wrapper so idsOf can be tested without importing ivf in proxy.go.
// main.go converts ivf.Hit → hit inline.
type hit struct {
	hash uint64
}
