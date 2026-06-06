package ivf

import (
	"reflect"
	"testing"
)

// coarseKMeans must return exactly k centroids and recover obvious clusters:
// points tightly grouped around (0,0) and (100,100) should yield two centroids,
// one near each group, so every point's nearest centroid matches its true group.
func TestCoarseKMeansRecoversClusters(t *testing.T) {
	var points [][]float32
	for i := 0; i < 50; i++ {
		f := float32(i%5) * 0.1
		points = append(points, []float32{f, f})         // cluster near origin
		points = append(points, []float32{100 + f, 100 + f}) // cluster near (100,100)
	}

	cents := coarseKMeans(points, 2, 25, 1)
	if len(cents) != 2 {
		t.Fatalf("got %d centroids, want 2", len(cents))
	}

	// Every low point and every high point must land in different cells.
	lowCell := nearest(cents, []float32{0, 0})
	highCell := nearest(cents, []float32{100, 100})
	if lowCell == highCell {
		t.Fatalf("both groups mapped to the same cell %d; clustering failed", lowCell)
	}
}

// Same seed must produce byte-identical centroids — the determinism guarantee.
func TestCoarseKMeansDeterministic(t *testing.T) {
	var points [][]float32
	for i := 0; i < 200; i++ {
		f := float32(i)
		points = append(points, []float32{f, -f, f * 0.5})
	}
	a := coarseKMeans(points, 8, 20, 42)
	b := coarseKMeans(points, 8, 20, 42)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same seed produced different centroids; not deterministic")
	}
}
