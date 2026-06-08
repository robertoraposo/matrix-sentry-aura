package aniso

import (
	"math"
	"testing"
)

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// solveSPD must solve A x = b for symmetric positive-definite A (Cholesky).
func TestSolveSPD(t *testing.T) {
	// A = [[4,1],[1,3]], b = [1,2] -> x = [1/11, 7/11]
	A := [][]float64{{4, 1}, {1, 3}}
	b := []float64{1, 2}
	x := solveSPD(A, b)
	if !approx(x[0], 1.0/11, 1e-9) || !approx(x[1], 7.0/11, 1e-9) {
		t.Fatalf("solveSPD wrong: got %v", x)
	}
}

// With h=1 (isotropic), the weighted centroid is exactly the plain mean.
func TestWeightedCentroidIsotropicIsMean(t *testing.T) {
	pts := [][]float32{{1, 2}, {3, 4}, {5, 0}}
	c := weightedCentroid(pts, 1.0)
	if !approx(float64(c[0]), 3.0, 1e-5) || !approx(float64(c[1]), 2.0, 1e-5) {
		t.Fatalf("h=1 centroid should be mean (3,2), got %v", c)
	}
}

// The weighted squared distance reduces to plain L2 when h=1, and adds the
// parallel penalty when h>1.
func TestWeightedSqDist(t *testing.T) {
	x := []float32{3, 0} // direction û = (1,0)
	c := []float32{0, 0}
	// e = (3,0); e_par along û=(1,0) is (3,0); ||e||^2=9, (e·û)^2=9
	if d := weightedSqDist(x, c, 1.0); !approx(d, 9, 1e-5) {
		t.Errorf("h=1 dist = %.4f, want 9", d)
	}
	// h=2: 9 + (2-1)*9 = 18
	if d := weightedSqDist(x, c, 2.0); !approx(d, 18, 1e-5) {
		t.Errorf("h=2 dist = %.4f, want 18", d)
	}
}

// On data spread along one direction, anisotropic centroids (h>1) should reduce
// the parallel error of the assignment vs the isotropic mean — sanity that the
// weighting actually changes the solution when h>1.
func TestAnisotropicCentroidShifts(t *testing.T) {
	// points with a clear parallel structure; û differs per point
	pts := [][]float32{{2, 1}, {1, 2}, {3, 1}}
	ciso := weightedCentroid(pts, 1.0)
	caniso := weightedCentroid(pts, 4.0)
	moved := math.Hypot(float64(ciso[0]-caniso[0]), float64(ciso[1]-caniso[1]))
	if moved < 1e-3 {
		t.Errorf("h=4 centroid should differ from the mean, moved=%.6f", moved)
	}
}
