// Package ivf implements an IVFADC index (inverted file with residual Product
// Quantization) on top of the frozen pq package. The coarse quantizer here
// partitions full-dimensional vectors into nlist cells; pq encodes the residual
// of each vector against its cell centroid. Search probes only the nprobe
// nearest cells, turning the linear PQ scan into a fraction-of-the-index scan.
package ivf

import "math"

// sqL2 is the squared Euclidean distance. Squared because we only compare
// distances — monotonic and one multiply cheaper than the true norm.
func sqL2(a, b []float32) float64 {
	var s float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		s += d * d
	}
	return s
}

// nearest returns the index of the centroid closest to v under squared-L2.
// Ties resolve to the lower index, which keeps assignment deterministic.
func nearest(centroids [][]float32, v []float32) int {
	best, bestD := 0, math.MaxFloat64
	for c, cen := range centroids {
		if d := sqL2(cen, v); d < bestD {
			bestD, best = d, c
		}
	}
	return best
}
