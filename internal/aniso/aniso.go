// Package aniso implements anisotropic product quantization (ScaNN-style,
// Mechanism F for real embeddings): the quantization k-means penalizes the
// component of the error PARALLEL to the datapoint direction more than the
// orthogonal component (loss = ||e_⊥||² + h·||e_∥||², h≥1). On data whose
// quantization error is aligned with the datapoint (high ρ̄·D, real text
// embeddings) this reduces the ranking-harmful error that plain isotropic PQ —
// and rotation/OPQ — cannot. h=1 reduces exactly to standard PQ.
package aniso

import "math"

// solveSPD solves A x = b for symmetric positive-definite A via Cholesky.
func solveSPD(A [][]float64, b []float64) []float64 {
	n := len(A)
	L := make([][]float64, n)
	for i := range L {
		L[i] = make([]float64, n)
	}
	for i := 0; i < n; i++ {
		for j := 0; j <= i; j++ {
			sum := A[i][j]
			for k := 0; k < j; k++ {
				sum -= L[i][k] * L[j][k]
			}
			if i == j {
				L[i][j] = math.Sqrt(sum)
			} else {
				L[i][j] = sum / L[j][j]
			}
		}
	}
	// forward solve L y = b
	y := make([]float64, n)
	for i := 0; i < n; i++ {
		sum := b[i]
		for k := 0; k < i; k++ {
			sum -= L[i][k] * y[k]
		}
		y[i] = sum / L[i][i]
	}
	// back solve Lᵀ x = y
	x := make([]float64, n)
	for i := n - 1; i >= 0; i-- {
		sum := y[i]
		for k := i + 1; k < n; k++ {
			sum -= L[k][i] * x[k]
		}
		x[i] = sum / L[i][i]
	}
	return x
}

// unit returns x/||x|| (float64) and ok=false for a zero vector.
func unit(x []float32) ([]float64, bool) {
	var n2 float64
	for _, v := range x {
		n2 += float64(v) * float64(v)
	}
	if n2 == 0 {
		return nil, false
	}
	inv := 1.0 / math.Sqrt(n2)
	u := make([]float64, len(x))
	for i, v := range x {
		u[i] = float64(v) * inv
	}
	return u, true
}

// weightedSqDist = ||e||² + (h-1)(e·û)², where e = x - c and û = x/||x||.
func weightedSqDist(x, c []float32, h float64) float64 {
	u, ok := unit(x)
	var e2, edotu float64
	for i := range x {
		e := float64(x[i]) - float64(c[i])
		e2 += e * e
		if ok {
			edotu += e * u[i]
		}
	}
	return e2 + (h-1)*edotu*edotu
}

// weightedCentroid returns the centroid minimizing Σ weightedSqDist, i.e. the
// solution of (Σ Wₓ) c = Σ Wₓ x with Wₓ = I + (h-1) û ûᵀ. h=1 → plain mean.
func weightedCentroid(pts [][]float32, h float64) []float32 {
	if len(pts) == 0 {
		return nil
	}
	d := len(pts[0])
	if h == 1.0 {
		c := make([]float32, d)
		for _, p := range pts {
			for i := range p {
				c[i] += p[i]
			}
		}
		inv := float32(1.0 / float64(len(pts)))
		for i := range c {
			c[i] *= inv
		}
		return c
	}

	A := make([][]float64, d)
	for i := range A {
		A[i] = make([]float64, d)
	}
	b := make([]float64, d)
	for _, p := range pts {
		for i := range p {
			b[i] += float64(p[i]) // Σ x
		}
		u, ok := unit(p)
		if !ok {
			continue
		}
		var udotx float64
		for i := range p {
			udotx += u[i] * float64(p[i])
		}
		for i := 0; i < d; i++ {
			b[i] += (h - 1) * u[i] * udotx // (h-1) Σ û(û·x)
			for j := 0; j < d; j++ {
				A[i][j] += (h - 1) * u[i] * u[j] // (h-1) Σ û ûᵀ
			}
		}
	}
	n := float64(len(pts))
	for i := 0; i < d; i++ {
		A[i][i] += n // + n I
	}
	sol := solveSPD(A, b)
	c := make([]float32, d)
	for i := range c {
		c[i] = float32(sol[i])
	}
	return c
}
