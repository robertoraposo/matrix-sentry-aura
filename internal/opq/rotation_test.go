package opq

import (
	"math"
	"testing"
)

// A random rotation must be orthonormal (R·Rᵀ = I), which is what makes it a
// pure change of basis: it redistributes variance across PQ subspaces without
// distorting distances.
func TestRandomRotationIsOrthonormal(t *testing.T) {
	d := 32
	R := RandomRotation(d, 7)
	for i := 0; i < d; i++ {
		for j := 0; j < d; j++ {
			var dot float64
			for k := 0; k < d; k++ {
				dot += float64(R[i][k]) * float64(R[j][k])
			}
			want := 0.0
			if i == j {
				want = 1.0
			}
			if math.Abs(dot-want) > 1e-4 {
				t.Fatalf("R·Rᵀ[%d][%d] = %.6f, want %.1f", i, j, dot, want)
			}
		}
	}
}

// Rotation preserves L2 norm (orthonormal transform), so ranking by distance is
// invariant — the win comes purely from better quantization, not changed geometry.
func TestRotatePreservesNorm(t *testing.T) {
	d := 32
	R := RandomRotation(d, 3)
	v := make([]float32, d)
	for i := range v {
		v[i] = float32(math.Sin(float64(i)))
	}
	rv := Rotate(R, v)
	if len(rv) != d {
		t.Fatalf("rotated len = %d, want %d", len(rv), d)
	}
	var n2v, n2rv float64
	for i := 0; i < d; i++ {
		n2v += float64(v[i]) * float64(v[i])
		n2rv += float64(rv[i]) * float64(rv[i])
	}
	if math.Abs(math.Sqrt(n2v)-math.Sqrt(n2rv)) > 1e-3 {
		t.Errorf("norm not preserved: ||v||=%.5f ||Rv||=%.5f", math.Sqrt(n2v), math.Sqrt(n2rv))
	}
}

func TestRandomRotationDeterministic(t *testing.T) {
	a := RandomRotation(16, 42)
	b := RandomRotation(16, 42)
	for i := range a {
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				t.Fatalf("same seed gave different rotation at [%d][%d]", i, j)
			}
		}
	}
}
