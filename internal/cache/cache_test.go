package cache

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// LRU: capacity 2 over [1,2,1,2] → miss,miss,hit,hit = 0.5 hit-rate.
func TestLRUBasicHitRate(t *testing.T) {
	if hr := HitRate(NewLRU(2), []int{1, 2, 1, 2}); !approx(hr, 0.5) {
		t.Errorf("LRU hit-rate = %.3f, want 0.5", hr)
	}
}

// LRU evicts the least-recently-used: [1,2,3,1] cap 2 → all misses (1 evicted
// before its reuse).
func TestLRUEviction(t *testing.T) {
	if hr := HitRate(NewLRU(2), []int{1, 2, 3, 1}); !approx(hr, 0.0) {
		t.Errorf("LRU hit-rate = %.3f, want 0.0", hr)
	}
}

// LFU keeps frequently-used items: [1,1,1,2,3,1] cap 2 — item 1 is hot and
// should survive, so the final access to 1 is a hit.
func TestLFUKeepsHot(t *testing.T) {
	// accesses: 1(m)1(h)1(h)2(m)3(m,evict 2)1(h) → hits=3/6=0.5
	if hr := HitRate(NewLFU(2), []int{1, 1, 1, 2, 3, 1}); !approx(hr, 0.5) {
		t.Errorf("LFU hit-rate = %.3f, want 0.5", hr)
	}
}

// Replay reports the full cost-relevant accounting, not just the hit ratio:
// LRU cap 2 over [1,2,1,3,1] → m,m,h,m,h and no prefetch traffic.
func TestReplayStatsLRU(t *testing.T) {
	st := Replay(NewLRU(2), []int{1, 2, 1, 3, 1})
	if st.Accesses != 5 || st.Hits != 2 || st.Prefetches != 0 {
		t.Errorf("Replay = %+v, want Accesses=5 Hits=2 Prefetches=0", st)
	}
}

// Markov-prefetch pays bandwidth only for NON-resident prefetch admissions.
// Alternating stream [1,2,1,2,1,2] cap 2: 4 prefetches issued (t2..t5) but the
// predicted item is always already resident → zero prefetch loads.
func TestReplayStatsMarkovCountsPrefetches(t *testing.T) {
	st := Replay(NewMarkovPrefetch(2), []int{1, 2, 1, 2, 1, 2})
	if st.Accesses != 6 || st.Hits != 4 {
		t.Errorf("Replay = %+v, want Accesses=6 Hits=4", st)
	}
	if st.Prefetches != 4 {
		t.Errorf("Prefetches = %d, want 4 (one per step from the 3rd on)", st.Prefetches)
	}
	if st.PrefetchLoads != 0 {
		t.Errorf("PrefetchLoads = %d, want 0 (predictions always resident)", st.PrefetchLoads)
	}
}

// When the predicted item was evicted, the prefetch is real traffic.
// Stream [1,2,1,2,3,1] cap 2: prefetches issued at t2,t3,t5; only t5's
// (predict 2 after 1, but 2 was evicted by 3 at t4) loads a non-resident item.
func TestReplayStatsMarkovPrefetchLoads(t *testing.T) {
	st := Replay(NewMarkovPrefetch(2), []int{1, 2, 1, 2, 3, 1})
	if st.Hits != 3 {
		t.Errorf("Hits = %d, want 3 (t2,t3,t5)", st.Hits)
	}
	if st.Prefetches != 3 {
		t.Errorf("Prefetches = %d, want 3 (issued at t2,t3,t5)", st.Prefetches)
	}
	if st.PrefetchLoads != 1 {
		t.Errorf("PrefetchLoads = %d, want 1 (only t5 admits a non-resident item)", st.PrefetchLoads)
	}
}

// Every policy exposes its current membership (sorted) so an exact tier can
// mirror it with real vectors.
func TestItemsMembership(t *testing.T) {
	lru := NewLRU(2)
	for _, it := range []int{1, 2, 3} {
		lru.Access(it)
	}
	if got := lru.(Lister).Items(); !equalInts(got, []int{2, 3}) {
		t.Errorf("LRU Items = %v, want [2 3]", got)
	}

	lfu := NewLFU(2)
	for _, it := range []int{1, 1, 2, 3} {
		lfu.Access(it)
	}
	if got := lfu.(Lister).Items(); !equalInts(got, []int{1, 3}) {
		t.Errorf("LFU Items = %v, want [1 3]", got)
	}

	mk := NewMarkovPrefetch(2)
	for _, it := range []int{1, 2, 1} {
		mk.Access(it)
	}
	if got := mk.(Lister).Items(); !equalInts(got, []int{1, 2}) {
		t.Errorf("Markov Items = %v, want [1 2]", got)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// On a perfectly sequential stream with a cache too small for the working set,
// Markov prefetch should beat LRU: it predicts the next item and pre-loads it.
func TestMarkovPrefetchBeatsLRU(t *testing.T) {
	var stream []int
	for i := 0; i < 50; i++ {
		stream = append(stream, 1, 2, 3, 4, 5) // period-5 chain
	}
	cap := 2 // far smaller than the working set of 5
	lru := HitRate(NewLRU(cap), stream)
	mk := HitRate(NewMarkovPrefetch(cap), stream)
	if mk <= lru {
		t.Errorf("Markov-prefetch (%.3f) should beat LRU (%.3f) on a sequential stream", mk, lru)
	}
	if mk < 0.8 {
		t.Errorf("Markov-prefetch should approach perfect on a clean chain, got %.3f", mk)
	}
}
