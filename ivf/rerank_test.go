package ivf

import (
	"math/rand"
	"sort"
	"testing"
)

// rerank test fixture: clustered data so the coarse quantizer has structure,
// and a brute-force oracle for exact expectations.
func rerankFixture(t *testing.T) (*Index, [][]float32, [][]float32) {
	t.Helper()
	rng := rand.New(rand.NewSource(11))
	var vecs [][]float32
	centers := make([][]float32, 4)
	for i := range centers {
		c := make([]float32, 16)
		for j := range c {
			c[j] = rng.Float32() * 8
		}
		centers[i] = c
	}
	for i := 0; i < 300; i++ {
		c := centers[rng.Intn(4)]
		v := make([]float32, 16)
		for j := range v {
			v[j] = c[j] + float32(rng.NormFloat64())*0.4
		}
		vecs = append(vecs, v)
	}
	ix, err := New(Config{Dim: 16, Nlist: 4, M: 4, K: 16, Iter: 10, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.Train(vecs); err != nil {
		t.Fatal(err)
	}
	ix.Add(vecs)
	var queries [][]float32
	for i := 0; i < 20; i++ {
		c := centers[rng.Intn(4)]
		q := make([]float32, 16)
		for j := range q {
			q[j] = c[j] + float32(rng.NormFloat64())*0.4
		}
		queries = append(queries, q)
	}
	return ix, vecs, queries
}

// byHashLookup builds the fetch callback every test uses: original vectors
// keyed by content hash, as a CAS or RAM registry would serve them.
func byHashLookup(ix *Index, vecs [][]float32) func(uint64) []float32 {
	m := map[uint64][]float32{}
	for _, v := range vecs {
		m[hashVec(v)] = v
	}
	return func(h uint64) []float32 { return m[h] }
}

func bruteTop(query []float32, vecs [][]float32, k int) []uint64 {
	type pair struct {
		h uint64
		d float32
	}
	seen := map[uint64]bool{}
	var ps []pair
	for _, v := range vecs {
		h := hashVec(v)
		if seen[h] {
			continue
		}
		seen[h] = true
		ps = append(ps, pair{h, float32(sqL2(query, v))})
	}
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].d != ps[j].d {
			return ps[i].d < ps[j].d
		}
		return ps[i].h < ps[j].h
	})
	if len(ps) > k {
		ps = ps[:k]
	}
	out := make([]uint64, len(ps))
	for i, p := range ps {
		out[i] = p.h
	}
	return out
}

func TestSearchRerankZeroIsPlainSearch(t *testing.T) {
	ix, vecs, queries := rerankFixture(t)
	fetch := byHashLookup(ix, vecs)
	for _, q := range queries {
		plain := ix.Search(q, 4, 10)
		rr := ix.SearchRerank(q, 4, 10, 0, fetch)
		if len(plain) != len(rr) {
			t.Fatalf("len mismatch %d vs %d", len(plain), len(rr))
		}
		for i := range plain {
			if plain[i].Handle.Hash != rr[i].Handle.Hash || plain[i].Dist != rr[i].Dist {
				t.Fatalf("rerankK=0 diverged from Search at rank %d", i)
			}
		}
	}
}

func TestSearchRerankEverythingMatchesExact(t *testing.T) {
	ix, vecs, queries := rerankFixture(t)
	fetch := byHashLookup(ix, vecs)
	for qi, q := range queries {
		// nprobe = all cells, rerank the whole base: must equal brute force.
		rr := ix.SearchRerank(q, 4, 10, ix.Ntotal(), fetch)
		want := bruteTop(q, vecs, 10)
		if len(rr) != len(want) {
			t.Fatalf("q%d: got %d results, want %d", qi, len(rr), len(want))
		}
		for i := range want {
			if rr[i].Handle.Hash != want[i] {
				t.Fatalf("q%d rank %d: got %x want %x", qi, i, rr[i].Handle.Hash, want[i])
			}
		}
	}
}

func TestSearchRerankDistancesAreExactForFetched(t *testing.T) {
	ix, vecs, queries := rerankFixture(t)
	fetch := byHashLookup(ix, vecs)
	q := queries[0]
	for _, h := range ix.SearchRerank(q, 4, 10, 50, fetch) {
		orig := fetch(h.Handle.Hash)
		if orig == nil {
			continue
		}
		if exact := float32(sqL2(q, orig)); h.Dist != exact {
			t.Fatalf("re-ranked Dist %f != exact %f", h.Dist, exact)
		}
	}
}

func TestSearchRerankMissingOriginalKeepsADCEstimate(t *testing.T) {
	ix, _, queries := rerankFixture(t)
	// fetch that knows nothing: re-rank must degrade gracefully to plain ADC
	// ordering, never drop candidates.
	none := func(uint64) []float32 { return nil }
	for _, q := range queries {
		plain := ix.Search(q, 4, 10)
		rr := ix.SearchRerank(q, 4, 10, 100, none)
		if len(rr) != len(plain) {
			t.Fatalf("missing originals dropped candidates: %d vs %d", len(rr), len(plain))
		}
		// ADC ties make order among equals unspecified; the invariant is that
		// the per-rank distances are untouched (no distortion, no drops).
		for i := range plain {
			if plain[i].Dist != rr[i].Dist {
				t.Fatalf("all-miss re-rank changed Dist at rank %d: %f vs %f", i, plain[i].Dist, rr[i].Dist)
			}
		}
	}
}
