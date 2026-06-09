// Package cache simulates access-driven memory caching — the OPERATIONAL use of
// the agent's access log (after the RD-allocation framing was closed on real
// embeddings). It compares eviction/prefetch policies by hit-rate over an access
// stream: the predictive (Markov) prefetch is where the measured next-access
// lift turns into a concrete cache-hit-rate gain.
package cache

import "matrixsentry/internal/refine"

// Cache consumes an access stream one item at a time, returning hit/miss.
type Cache interface {
	Access(item int) bool // true on hit
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

// --- Markov prefetch (Mechanism D, operational) ---

type markovPrefetch struct {
	lru     *lru
	mk      *refine.Markov
	prev    int
	started bool
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
		c.lru.admit(pred[0]) // prefetch (does not count as a hit)
	}
	return hit
}
