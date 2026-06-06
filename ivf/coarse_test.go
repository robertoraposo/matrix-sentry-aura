package ivf

import "testing"

// nearest must return the index of the closest centroid by squared-L2, and on a
// tie it must return the LOWER index. The tie rule is what keeps assignment (and
// therefore the whole index) deterministic regardless of core count.
func TestNearestPicksClosest(t *testing.T) {
	centroids := [][]float32{
		{0, 0},
		{10, 10},
		{5, 5},
	}
	if got := nearest(centroids, []float32{4, 4}); got != 2 {
		t.Fatalf("nearest = %d, want 2 (centroid {5,5})", got)
	}
	if got := nearest(centroids, []float32{9, 9}); got != 1 {
		t.Fatalf("nearest = %d, want 1 (centroid {10,10})", got)
	}
}

func TestNearestTieGoesToLowerIndex(t *testing.T) {
	centroids := [][]float32{
		{0, 0},
		{2, 0}, // {1,0} is equidistant to centroid 0 and centroid 1
	}
	if got := nearest(centroids, []float32{1, 0}); got != 0 {
		t.Fatalf("nearest on a tie = %d, want 0 (lower index)", got)
	}
}
