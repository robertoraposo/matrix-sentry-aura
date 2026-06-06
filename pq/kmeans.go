package pq

import (
	"math"
	"math/rand"
)

// sqL2 returns the squared Euclidean distance between two equal-length slices.
// Squared (not sqrt'd) because we only ever compare distances — monotonic and
// one multiply cheaper per dimension.
func sqL2(a, b []float32) float64 {
	var s float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		s += d * d
	}
	return s
}

func cloneF32(v []float32) []float32 {
	c := make([]float32, len(v))
	copy(c, v)
	return c
}

// kmeans runs Lloyd's algorithm with k-means++ seeding over `points` (n x dim).
// Returns k centroids. This is the per-subspace quantizer that PQ trains M times.
func kmeans(points [][]float32, k, maxIter int, rng *rand.Rand) [][]float32 {
	n := len(points)
	if n == 0 {
		return nil
	}
	dim := len(points[0])
	if k > n {
		k = n
	}

	centroids := kmeansppInit(points, k, rng)
	assign := make([]int, n)

	for iter := 0; iter < maxIter; iter++ {
		changed := false

		// Assignment step: each point to its nearest centroid.
		for i, p := range points {
			best, bestD := 0, math.MaxFloat64
			for c := 0; c < k; c++ {
				if d := sqL2(p, centroids[c]); d < bestD {
					bestD, best = d, c
				}
			}
			if assign[i] != best {
				assign[i], changed = best, true
			}
		}

		// Update step: each centroid becomes the mean of its members.
		sums := make([][]float64, k)
		counts := make([]int, k)
		for c := range sums {
			sums[c] = make([]float64, dim)
		}
		for i, p := range points {
			c := assign[i]
			counts[c]++
			for j := 0; j < dim; j++ {
				sums[c][j] += float64(p[j])
			}
		}
		for c := 0; c < k; c++ {
			if counts[c] == 0 {
				// Empty cluster: reseed to a random point so we never waste a code.
				centroids[c] = cloneF32(points[rng.Intn(n)])
				continue
			}
			inv := 1.0 / float64(counts[c])
			for j := 0; j < dim; j++ {
				centroids[c][j] = float32(sums[c][j] * inv)
			}
		}

		if !changed && iter > 0 {
			break // converged
		}
	}
	return centroids
}

// kmeansppInit picks k seeds with probability proportional to D^2 distance from
// the nearest already-chosen seed. d2 is maintained incrementally: when a new
// centroid is added we only compare each point against that one centroid, so the
// whole init is O(n*k) instead of the naive O(n*k^2).
func kmeansppInit(points [][]float32, k int, rng *rand.Rand) [][]float32 {
	n := len(points)
	centroids := make([][]float32, 0, k)
	first := cloneF32(points[rng.Intn(n)])
	centroids = append(centroids, first)

	// d2[i] = squared distance from point i to its nearest chosen centroid.
	d2 := make([]float64, n)
	var sum float64
	for i, p := range points {
		d2[i] = sqL2(p, first)
		sum += d2[i]
	}

	for len(centroids) < k {
		if sum == 0 {
			// Remaining points coincide with chosen centroids: pad randomly.
			centroids = append(centroids, cloneF32(points[rng.Intn(n)]))
			continue
		}
		// Weighted pick.
		target := rng.Float64() * sum
		idx, acc := n-1, 0.0
		for i := 0; i < n; i++ {
			if acc += d2[i]; acc >= target {
				idx = i
				break
			}
		}
		nc := cloneF32(points[idx])
		centroids = append(centroids, nc)

		// Refresh d2 against only the new centroid; recompute sum.
		sum = 0
		for i, p := range points {
			if dd := sqL2(p, nc); dd < d2[i] {
				d2[i] = dd
			}
			sum += d2[i]
		}
	}
	return centroids
}
