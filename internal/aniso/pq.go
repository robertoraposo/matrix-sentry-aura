package aniso

import (
	"math"
	"math/rand"

	"matrixsentry/internal/lab"
)

// APQ is an anisotropic product quantizer: M independent subspace codebooks
// trained with the parallel-error-weighted k-means above. h=1 is standard PQ.
type APQ struct {
	M, K, Dsub int
	H          float64
	Codebooks  [][][]float32 // [M][K][Dsub]
}

// Train fits an APQ on vecs. Subspaces are trained in parallel (independent).
func Train(vecs [][]float32, m, k, iter int, h float64, seed int64) *APQ {
	d := len(vecs[0])
	dsub := d / m
	a := &APQ{M: m, K: k, Dsub: dsub, H: h, Codebooks: make([][][]float32, m)}
	lab.ParallelFor(m, func(sub int) {
		subv := make([][]float32, len(vecs))
		for i, v := range vecs {
			subv[i] = v[sub*dsub : (sub+1)*dsub]
		}
		a.Codebooks[sub] = kmeansAniso(subv, k, iter, h, seed+int64(sub)+1)
	})
	return a
}

func kmeansAniso(pts [][]float32, k, iter int, h float64, seed int64) [][]float32 {
	n := len(pts)
	dsub := len(pts[0])
	rng := rand.New(rand.NewSource(seed))

	cents := make([][]float32, k)
	for c := 0; c < k; c++ {
		cents[c] = append([]float32(nil), pts[rng.Intn(n)]...)
	}
	units := make([][]float64, n) // precomputed datapoint directions (nil if zero)
	for i, p := range pts {
		units[i], _ = unit(p)
	}

	assign := make([]int, n)
	for it := 0; it < iter; it++ {
		for i, p := range pts {
			u := units[i]
			best, bd := 0, math.MaxFloat64
			for c := 0; c < k; c++ {
				cc := cents[c]
				var e2, edotu float64
				for d := 0; d < dsub; d++ {
					e := float64(p[d]) - float64(cc[d])
					e2 += e * e
					if u != nil {
						edotu += e * u[d]
					}
				}
				if dd := e2 + (h-1)*edotu*edotu; dd < bd {
					bd, best = dd, c
				}
			}
			assign[i] = best
		}
		members := make([][][]float32, k)
		for i := range pts {
			members[assign[i]] = append(members[assign[i]], pts[i])
		}
		for c := 0; c < k; c++ {
			if len(members[c]) > 0 {
				cents[c] = weightedCentroid(members[c], h)
			}
		}
	}
	return cents
}

// Encode quantizes v to M codes, each the subspace centroid minimizing the
// anisotropic loss.
func (a *APQ) Encode(v []float32) []uint8 {
	code := make([]uint8, a.M)
	for sub := 0; sub < a.M; sub++ {
		s := v[sub*a.Dsub : (sub+1)*a.Dsub]
		best, bd := 0, math.MaxFloat64
		for c := 0; c < a.K; c++ {
			if dd := weightedSqDist(s, a.Codebooks[sub][c], a.H); dd < bd {
				bd, best = dd, c
			}
		}
		code[sub] = uint8(best)
	}
	return code
}

func (a *APQ) EncodeBatch(vecs [][]float32) [][]uint8 {
	codes := make([][]uint8, len(vecs))
	lab.ParallelFor(len(vecs), func(i int) { codes[i] = a.Encode(vecs[i]) })
	return codes
}

// Reconstruct rebuilds the full vector from its M codes.
func (a *APQ) Reconstruct(code []uint8) []float32 {
	out := make([]float32, a.M*a.Dsub)
	for sub := 0; sub < a.M; sub++ {
		copy(out[sub*a.Dsub:], a.Codebooks[sub][code[sub]])
	}
	return out
}
