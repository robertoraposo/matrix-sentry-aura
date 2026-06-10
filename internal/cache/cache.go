// Package cache simulates access-driven memory caching — the OPERATIONAL use of
// the agent's access log (after the RD-allocation framing was closed on real
// embeddings). It compares eviction/prefetch policies by hit-rate over an access
// stream: the predictive (Markov) prefetch is where the measured next-access
// lift turns into a concrete cache-hit-rate gain.
package cache

import (
	"sort"

	"matrixsentry/internal/refine"
)

// Cache consumes an access stream one item at a time, returning hit/miss.
type Cache interface {
	Access(item int) bool // true on hit
}

// Lister exposes a policy's current membership (sorted ascending) so an exact
// tier can mirror it with real vectors.
type Lister interface {
	Items() []int
}

// Stats is the cost-relevant accounting of a stream replay. Hits avoid the
// expensive miss path; Prefetches measure predictor activity; PrefetchLoads
// are the prefetches that actually moved data (admitted a non-resident item) —
// the bandwidth a prefetching policy pays that a classical one does not.
type Stats struct {
	Accesses      int
	Hits          int
	Prefetches    int
	PrefetchLoads int
}

// HitRatio returns hits/accesses (0 for an empty replay).
func (s Stats) HitRatio() float64 {
	if s.Accesses == 0 {
		return 0
	}
	return float64(s.Hits) / float64(s.Accesses)
}

// prefetchCounter is implemented by policies that issue speculative admissions.
type prefetchCounter interface {
	prefetchStats() (issued, loads int)
}

// Replay runs the whole stream through a fresh policy and returns the full
// accounting (HitRate's richer sibling).
func Replay(c Cache, stream []int) Stats {
	st := Stats{Accesses: len(stream)}
	for _, it := range stream {
		if c.Access(it) {
			st.Hits++
		}
	}
	if p, ok := c.(prefetchCounter); ok {
		st.Prefetches, st.PrefetchLoads = p.prefetchStats()
	}
	return st
}

// HitRate runs the whole stream through a fresh policy and returns hits/total.
func HitRate(c Cache, stream []int) float64 {
	if len(stream) == 0 {
		return 0
	}
	hits := 0
	for _, it := range stream {
		if c.Access(it) {
			hits++
		}
	}
	return float64(hits) / float64(len(stream))
}

// --- LRU ---

type lru struct {
	cap   int
	order []int // front = most-recently used
	set   map[int]bool
}

func NewLRU(capacity int) Cache { return &lru{cap: capacity, set: map[int]bool{}} }

func (c *lru) Access(item int) bool {
	if c.set[item] {
		c.touch(item)
		return true
	}
	c.admit(item)
	return false
}

func (c *lru) touch(item int) {
	for i, x := range c.order {
		if x == item {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	c.order = append([]int{item}, c.order...)
}

// admit inserts item as most-recent (or refreshes it), evicting the LRU tail.
func (c *lru) admit(item int) {
	if c.set[item] {
		c.touch(item)
		return
	}
	c.set[item] = true
	c.order = append([]int{item}, c.order...)
	for len(c.order) > c.cap {
		last := c.order[len(c.order)-1]
		c.order = c.order[:len(c.order)-1]
		delete(c.set, last)
	}
}

func (c *lru) Items() []int {
	out := append([]int(nil), c.order...)
	sort.Ints(out)
	return out
}

// --- LFU (popularity) ---

type lfu struct {
	cap  int
	freq map[int]int
}

func NewLFU(capacity int) Cache { return &lfu{cap: capacity, freq: map[int]int{}} }

func (c *lfu) Access(item int) bool {
	if _, ok := c.freq[item]; ok {
		c.freq[item]++
		return true
	}
	if len(c.freq) >= c.cap {
		c.evict()
	}
	c.freq[item] = 1
	return false
}

func (c *lfu) evict() {
	const maxInt = int(^uint(0) >> 1)
	minItem, minF := maxInt, maxInt
	for it, f := range c.freq {
		if f < minF || (f == minF && it < minItem) {
			minF, minItem = f, it
		}
	}
	delete(c.freq, minItem)
}

func (c *lfu) Items() []int {
	out := make([]int, 0, len(c.freq))
	for it := range c.freq {
		out = append(out, it)
	}
	sort.Ints(out)
	return out
}

// --- Markov prefetch (Mechanism D, operational) ---

type markovPrefetch struct {
	lru           *lru
	mk            *refine.Markov
	prev          int
	started       bool
	prefetches    int // predictions issued
	prefetchLoads int // predictions that admitted a non-resident item
}

// NewMarkovPrefetch is an LRU cache that, after each access, prefetches the item
// the online Markov model predicts will be accessed next.
func NewMarkovPrefetch(capacity int) Cache {
	return &markovPrefetch{lru: &lru{cap: capacity, set: map[int]bool{}}, mk: refine.NewMarkov()}
}

func (c *markovPrefetch) Access(item int) bool {
	hit := c.lru.Access(item) // hit measured against what was cached BEFORE this access
	if c.started {
		c.mk.Observe(c.prev, item)
	}
	c.prev, c.started = item, true
	if pred := c.mk.Predict(item, 1); len(pred) > 0 {
		c.prefetches++
		if !c.lru.set[pred[0]] {
			c.prefetchLoads++
		}
		c.lru.admit(pred[0]) // prefetch (does not count as a hit)
	}
	return hit
}

func (c *markovPrefetch) Items() []int { return c.lru.Items() }

func (c *markovPrefetch) prefetchStats() (issued, loads int) {
	return c.prefetches, c.prefetchLoads
}
