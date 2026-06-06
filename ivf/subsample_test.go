package ivf

import (
	"reflect"
	"runtime"
	"testing"

	"matrixsentry/pq"
)

// subsample must be a pure function of (input, m, seed): the same call twice
// yields byte-identical output, and the result is a genuine size-m subset drawn
// from the input (no synthesized vectors). This is what lets a subsampled build
// stay reproducible across runs and across core counts.
func TestSubsampleDeterministicAndExact(t *testing.T) {
	pts := pseudoCorpus(500, 8)

	a := subsample(pts, 100, 42)
	b := subsample(pts, 100, 42)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("subsample not deterministic for a fixed seed")
	}
	if len(a) != 100 {
		t.Fatalf("subsample size = %d, want 100", len(a))
	}

	// Every returned vector must be one of the originals (identity, not a copy
	// of different contents) — reservoir sampling keeps real points.
	orig := make(map[*float32]bool, len(pts))
	for i := range pts {
		orig[&pts[i][0]] = true
	}
	for _, v := range a {
		if !orig[&v[0]] {
			t.Fatal("subsample returned a vector that is not from the input set")
		}
	}

	// A different seed should generally pick a different subset.
	c := subsample(pts, 100, 7)
	if reflect.DeepEqual(a, c) {
		t.Fatal("subsample ignored the seed (same subset for different seeds)")
	}
}

// When m >= n there is nothing to drop: subsample returns the full set unchanged,
// so small learn sets (like the determinism test's 800 vectors) are never altered.
func TestSubsampleNoOpWhenLargerThanInput(t *testing.T) {
	pts := pseudoCorpus(50, 8)
	got := subsample(pts, 100, 1)
	if !reflect.DeepEqual(got, pts) {
		t.Fatal("subsample with m >= n must return the input unchanged")
	}
}

// The subsampled training path (TrainWithOpts) must keep the cross-core
// determinism guarantee: building on 1 core vs 8 cores yields identical results.
// This guards the new coarse-subsample / coarse-iter levers.
func TestTrainWithOptsDeterministicAcrossCores(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(0)) // restore on exit

	base := pseudoCorpus(800, 16)
	queries := pseudoCorpus(40, 16)
	opts := TrainOpts{CoarseIter: 5, CoarseSample: 300, PQSample: 300}

	build := func() [][]pq.Result {
		ix, err := TrainWithOpts(base, 16, 4, 32, 20, 7, opts)
		if err != nil {
			t.Fatal(err)
		}
		ix.Add(base)
		return ix.SearchBatch(queries, 8, 10)
	}

	runtime.GOMAXPROCS(1)
	serial := build()
	runtime.GOMAXPROCS(8)
	parallel := build()

	if !reflect.DeepEqual(serial, parallel) {
		t.Fatal("subsampled pipeline differs across core counts; determinism broken")
	}
}
