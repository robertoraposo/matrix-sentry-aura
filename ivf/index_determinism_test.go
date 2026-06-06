package ivf

import (
	"reflect"
	"runtime"
	"testing"
)

func TestIndexPipelineDeterministicAcrossCores(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(0))

	build := func() ([]AddResult, [][]Hit) {
		ix, err := New(Config{Dim: 16, Nlist: 16, M: 4, K: 32, Iter: 20, Seed: 9,
			Train: TrainOpts{CoarseIter: 5, CoarseSample: 300, PQSample: 300}})
		if err != nil {
			t.Fatal(err)
		}
		base := pseudoCorpus(800, 16)
		if err := ix.Train(base); err != nil {
			t.Fatal(err)
		}
		added := ix.Add(base)
		hits := ix.SearchBatch(pseudoCorpus(40, 16), 8, 10)
		return added, hits
	}

	runtime.GOMAXPROCS(1)
	a1, h1 := build()
	runtime.GOMAXPROCS(8)
	a2, h2 := build()

	if !reflect.DeepEqual(a1, a2) {
		t.Fatal("Add results differ across core counts; determinism broken")
	}
	if !reflect.DeepEqual(h1, h2) {
		t.Fatal("Search results differ across core counts; determinism broken")
	}
}
