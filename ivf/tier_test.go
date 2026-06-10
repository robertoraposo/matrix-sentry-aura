package ivf

import (
	"math"
	"math/rand"
	"testing"
)

// twoClusters builds a deterministic corpus split between a cluster at the
// origin and one at offset, so the coarse quantizer separates them cleanly.
func twoClusters(nPer, dim int, offset float32, seed int64) (near, far [][]float32) {
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < nPer; i++ {
		a := make([]float32, dim)
		b := make([]float32, dim)
		for d := 0; d < dim; d++ {
			a[d] = rng.Float32() * 0.5
			b[d] = offset + rng.Float32()*0.5
		}
		near = append(near, a)
		far = append(far, b)
	}
	return near, far
}

func tieredFixture(t *testing.T) (*Index, []AddResult, [][]float32, [][]float32) {
	t.Helper()
	cfg := Config{Dim: 16, Nlist: 2, M: 4, K: 16, Iter: 10, Seed: 7}
	ix, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	nearL, farL := twoClusters(200, 16, 10, 1)
	if err := ix.Train(append(nearL, farL...)); err != nil {
		t.Fatal(err)
	}
	nearB, farB := twoClusters(8, 16, 10, 2)
	res := ix.Add(append(nearB, farB...))
	return ix, res, nearB, farB
}

func exactSq(a, b []float32) float32 {
	var s float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		s += d * d
	}
	return float32(s)
}

// A nil/empty tier must leave SearchTiered identical to Search.
func TestSearchTieredEmptyTierIsSearch(t *testing.T) {
	ix, _, nearB, _ := tieredFixture(t)
	q := nearB[0]
	plain := ix.Search(q, 1, 5)
	tiered := ix.SearchTiered(q, 1, 5, nil)
	if len(plain) != len(tiered) {
		t.Fatalf("lengths differ: %d vs %d", len(plain), len(tiered))
	}
	for i := range plain {
		if plain[i].Handle.Hash != tiered[i].Handle.Hash || plain[i].Dist != tiered[i].Dist {
			t.Errorf("result %d differs: %+v vs %+v", i, plain[i], tiered[i])
		}
	}
}

// A resident item is scored with its EXACT distance, replacing the ADC
// estimate, and appears exactly once.
func TestSearchTieredExactReplacesADC(t *testing.T) {
	ix, res, nearB, _ := tieredFixture(t)
	q := nearB[0]
	target := res[1].Handle.Hash // another near-cluster item, surely in the probed cell
	tier := map[uint64][]float32{target: nearB[1]}

	hits := ix.SearchTiered(q, 2, len(res), tier)
	seen := 0
	for _, h := range hits {
		if h.Handle.Hash == target {
			seen++
			want := exactSq(q, nearB[1])
			if math.Abs(float64(h.Dist-want)) > 1e-4 {
				t.Errorf("resident dist = %g, want exact %g", h.Dist, want)
			}
		}
	}
	if seen != 1 {
		t.Errorf("resident appeared %d times, want exactly 1", seen)
	}
}

// THE recall-perfect property: a resident item is a candidate even when its
// cell is NOT probed. With nprobe=1 a far-cluster vector is invisible to plain
// Search, but resident in the tier it must come back (with exact distance).
func TestSearchTieredImmuneToRoutingMiss(t *testing.T) {
	ix, res, nearB, farB := tieredFixture(t)
	q := nearB[0]
	farHash := res[8].Handle.Hash // farB[0] (Add order: 8 near then 8 far)

	for _, h := range ix.Search(q, 1, 16) {
		if h.Handle.Hash == farHash {
			t.Fatal("fixture broken: far vector reachable with nprobe=1")
		}
	}

	tier := map[uint64][]float32{farHash: farB[0]}
	found := false
	for _, h := range ix.SearchTiered(q, 1, 16, tier) {
		if h.Handle.Hash == farHash {
			found = true
			want := exactSq(q, farB[0])
			if math.Abs(float64(h.Dist-want)) > 1e-2 {
				t.Errorf("far resident dist = %g, want exact %g", h.Dist, want)
			}
		}
	}
	if !found {
		t.Error("resident far vector must be returned despite the routing miss")
	}
}

// When a resident's exact distance demotes it out of the top-K, the vacated
// slot must backfill from deeper ADC ranks — SearchTiered has to gather
// topK+len(tier) candidates, not topK. (Vehicle: the tier vector is
// authoritative, so mapping the ADC-best item's hash to a far vector forces
// the demotion deterministically.)
func TestSearchTieredBackfillsDemotedResident(t *testing.T) {
	ix, _, nearB, farB := tieredFixture(t)
	q := nearB[0]

	top2 := ix.Search(q, 2, 2)
	if len(top2) < 2 {
		t.Fatal("fixture too small")
	}
	bestHash, runnerHash := top2[0].Handle.Hash, top2[1].Handle.Hash

	// resident = the ADC-best item, but its authoritative vector is far away
	tier := map[uint64][]float32{bestHash: farB[3]}
	out := ix.SearchTiered(q, 2, 1, tier)
	if len(out) != 1 {
		t.Fatalf("got %d results, want 1", len(out))
	}
	if out[0].Handle.Hash == bestHash {
		t.Error("demoted resident must not keep rank 1")
	}
	if out[0].Handle.Hash != runnerHash {
		t.Errorf("vacated slot must backfill with the ADC runner-up")
	}
}

// Hashes unknown to the index are ignored (the tier mirrors indexed items; a
// stale entry must not fabricate results).
func TestSearchTieredIgnoresUnknownHash(t *testing.T) {
	ix, _, nearB, _ := tieredFixture(t)
	q := nearB[0]
	tier := map[uint64][]float32{0xdeadbeef: nearB[1]}
	plain := ix.Search(q, 1, 5)
	tiered := ix.SearchTiered(q, 1, 5, tier)
	if len(plain) != len(tiered) {
		t.Fatalf("unknown hash changed result count: %d vs %d", len(plain), len(tiered))
	}
	for i := range plain {
		if plain[i].Handle.Hash != tiered[i].Handle.Hash {
			t.Errorf("result %d differs after unknown-hash tier", i)
		}
	}
}
