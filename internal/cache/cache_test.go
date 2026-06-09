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
