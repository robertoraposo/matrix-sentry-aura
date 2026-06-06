package ivf

import (
	"reflect"
	"runtime"
	"testing"

	"matrixsentry/pq"
)

// pseudoCorpus builds a deterministic, non-trivial set of vectors (no RNG so the
// test itself is reproducible) spread across several loose clusters.
func pseudoCorpus(n, dim int) [][]float32 {
	out := make([][]float32, n)
	for i := 0; i < n; i++ {
		v := make([]float32, dim)
		cluster := i % 7
		for d := 0; d < dim; d++ {
			v[d] = float32(cluster*10) + float32((i*d)%13)*0.3
		}
		out[i] = v
	}
	return out
}

// buildAndSearch runs the full pipeline and returns all query results, so the
// test can compare the entire output across different core counts.
func buildAndSearch(t *testing.T, base, queries [][]float32) [][]pq.Result {
	t.Helper()
	ix, err := Train(base, 16 /*nlist*/, 4 /*M*/, 32 /*K*/, 20 /*iter*/, 7 /*seed*/)
	if err != nil {
		t.Fatal(err)
	}
	ix.Add(base)
	return ix.SearchBatch(queries, 8 /*nprobe*/, 10 /*topK*/)
}

// The entire Train→Add→Search pipeline must yield byte-identical results whether
// it runs on one core or many. This is the guarantee the PQ engine already makes;
// IVFADC must not break it.
func TestPipelineDeterministicAcrossCores(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(0)) // restore on exit

	base := pseudoCorpus(800, 16)
	queries := pseudoCorpus(40, 16)

	runtime.GOMAXPROCS(1)
	serial := buildAndSearch(t, base, queries)

	runtime.GOMAXPROCS(8)
	parallel := buildAndSearch(t, base, queries)

	if !reflect.DeepEqual(serial, parallel) {
		t.Fatal("pipeline produced different results on 1 core vs 8; determinism broken")
	}
}
