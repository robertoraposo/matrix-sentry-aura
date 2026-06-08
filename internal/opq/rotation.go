// Package opq implements rotation-based quantization (Mechanism F): apply an
// orthonormal rotation to vectors before product quantization so variance is
// spread across the PQ subspaces instead of concentrated in a few axes. On
// anisotropic data (e.g. real text embeddings, where axis-aligned PQ subspaces
// are badly imbalanced) this lowers quantization distortion at the same bytes.
//
// Stage 1 here is a random rotation (RR-PQ): the cheap probe for "does rotating
// help at all?". A learned (parametric OPQ) rotation can replace RandomRotation
// later for the full gain.
package opq

import (
	"math"
	"math/rand"
)

// RandomRotation returns a deterministic d×d orthonormal matrix (rows form an
// orthonormal basis), built by modified Gram-Schmidt on a seeded Gaussian
// matrix. Computed in float64 for numerical stability, stored as float32.
func RandomRotation(d int, seed int64) [][]float32 {
	rng := rand.New(rand.NewSource(seed))
	m := make([][]float64, d)
	for i := range m {
		m[i] = make([]float64, d)
		for j := range m[i] {
			m[i][j] = rng.NormFloat64()
		}
	}

	// Modified Gram-Schmidt on the rows.
	for i := 0; i < d; i++ {
		for j := 0; j < i; j++ {
			var dot float64
			for k := 0; k < d; k++ {
				dot += m[i][k] * m[j][k]
			}
			for k := 0; k < d; k++ {
				m[i][k] -= dot * m[j][k]
			}
		}
		var norm float64
		for k := 0; k < d; k++ {
			norm += m[i][k] * m[i][k]
		}
		norm = math.Sqrt(norm)
		for k := 0; k < d; k++ {
			m[i][k] /= norm
		}
	}

	R := make([][]float32, d)
	for i := range R {
		R[i] = make([]float32, d)
		for j := range R[i] {
			R[i][j] = float32(m[i][j])
		}
	}
	return R
}

// Rotate returns R·v (length len(R)). v must have length len(R[0]).
func Rotate(R [][]float32, v []float32) []float32 {
	out := make([]float32, len(R))
	for i := range R {
		row := R[i]
		var s float64
		for k := 0; k < len(v); k++ {
			s += float64(row[k]) * float64(v[k])
		}
		out[i] = float32(s)
	}
	return out
}

// RotateBatch rotates every vector in vecs by R.
func RotateBatch(R [][]float32, vecs [][]float32) [][]float32 {
	out := make([][]float32, len(vecs))
	for i, v := range vecs {
		out[i] = Rotate(R, v)
	}
	return out
}
