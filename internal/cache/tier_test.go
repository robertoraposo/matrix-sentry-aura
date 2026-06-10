package cache

import "testing"

// fetchSpy returns a recognizable vector per item and counts fetches per item.
func fetchSpy(calls map[int]int) func(int) []float32 {
	return func(item int) []float32 {
		calls[item]++
		return []float32{float32(item)}
	}
}

// The tier mirrors the policy's membership with real vectors: fetch on admit,
// drop on evict, no fetch on hit.
func TestExactTierMirrorsLRU(t *testing.T) {
	calls := map[int]int{}
	tier := NewExactTier(NewLRU(2), fetchSpy(calls))

	if tier.Access(1) {
		t.Fatal("first access to 1 must miss")
	}
	tier.Access(2)
	tier.Access(3) // evicts 1
	if got := tier.Members(); !equalInts(got, []int{2, 3}) {
		t.Errorf("Members = %v, want [2 3]", got)
	}
	if tier.Vec(1) != nil {
		t.Error("Vec(1) must be dropped after eviction")
	}
	if v := tier.Vec(3); v == nil || v[0] != 3 {
		t.Errorf("Vec(3) = %v, want [3]", v)
	}
	if tier.Fetches() != 3 {
		t.Errorf("Fetches = %d, want 3", tier.Fetches())
	}
	if !tier.Access(2) {
		t.Error("re-access of 2 must hit")
	}
	if tier.Fetches() != 3 {
		t.Errorf("a hit must not fetch; Fetches = %d, want 3", tier.Fetches())
	}
}

// With a Markov policy the tier also fetches PREFETCHED vectors (that is the
// point: the predicted-next item is resident, uncompressed, before its access)
// and drops vectors evicted by prefetch admissions.
//
// Hand trace, cap 2, stream [1,2,1,2,3,1]:
//
//	t0 1 miss fetch(1) · t1 2 miss fetch(2) · t2 1 hit, prefetch 2 (resident)
//	t3 2 hit, prefetch 1 (resident) · t4 3 miss fetch(3), evicts 2
//	t5 1 hit, Markov predicts 2 → prefetch FETCHES 2, evicting 3
//
// → 4 fetches total, final members {1,2}.
func TestExactTierFetchesPrefetchedVectors(t *testing.T) {
	calls := map[int]int{}
	tier := NewExactTier(NewMarkovPrefetch(2), fetchSpy(calls))

	hits := 0
	for _, it := range []int{1, 2, 1, 2, 3, 1} {
		if tier.Access(it) {
			hits++
		}
	}
	if hits != 3 {
		t.Errorf("hits = %d, want 3 (t2,t3,t5)", hits)
	}
	if tier.Fetches() != 4 {
		t.Errorf("Fetches = %d, want 4 (1,2,3 on miss + prefetched 2)", tier.Fetches())
	}
	if got := tier.Members(); !equalInts(got, []int{1, 2}) {
		t.Errorf("Members = %v, want [1 2]", got)
	}
	if calls[2] != 2 {
		t.Errorf("item 2 fetched %d times, want 2 (miss at t1, prefetch at t5)", calls[2])
	}
}
