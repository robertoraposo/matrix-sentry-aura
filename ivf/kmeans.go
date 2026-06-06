package ivf

import "math/rand"

func cloneF32(v []float32) []float32 {
	c := make([]float32, len(v))
	copy(c, v)
	return c
}

// coarseKMeans runs Lloyd's algorithm with k-means++ seeding over the full-
// dimensional points and returns k centroids. A fixed seed yields identical
// centroids on every run, so the cell layout — and the whole IVF index — is
// reproducible regardless of how the work is scheduled across cores.
//
// This mirrors pq/kmeans.go's algorithm but operates on full vectors (the coarse
// quantizer) instead of subvectors, and lives here so the pq package stays frozen.
func coarseKMeans(points [][]float32, k, maxIter int, seed int64) [][]float32 {
	n := len(points)
	if n == 0 {
		return nil
	}
	dim := len(points[0])
	if k > n {
		k = n
	}
	rng := rand.New(rand.NewSource(seed))

	centroids := kmeansppInit(points, k, rng)
	assign := make([]int, n)

	for iter := 0; iter < maxIter; iter++ {
		changed := false
		for i, p := range points {
			b := nearest(centroids, p)
			if assign[i] != b {
				assign[i], changed = b, true
			}
		}

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
				centroids[c] = cloneF32(points[rng.Intn(n)]) // reseed empty cell
				continue
			}
			inv := 1.0 / float64(counts[c])
			for j := 0; j < dim; j++ {
				centroids[c][j] = float32(sums[c][j] * inv)
			}
		}

		if !changed && iter > 0 {
			break
		}
	}
	return centroids
}

// kmeansppInit seeds k centroids with probability proportional to squared
// distance from the nearest chosen seed, maintained incrementally (O(n*k)).
func kmeansppInit(points [][]float32, k int, rng *rand.Rand) [][]float32 {
	n := len(points)
	centroids := make([][]float32, 0, k)
	first := cloneF32(points[rng.Intn(n)])
	centroids = append(centroids, first)

	d2 := make([]float64, n)
	var sum float64
	for i, p := range points {
		d2[i] = sqL2(p, first)
		sum += d2[i]
	}

	for len(centroids) < k {
		if sum == 0 {
			centroids = append(centroids, cloneF32(points[rng.Intn(n)]))
			continue
		}
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
