package cache

import "sort"

// ExactTier mirrors a cache policy's membership with UNCOMPRESSED vectors:
// whatever the policy decides to keep (including prefetched items) is held at
// full precision, so a search that consults the tier scores those items with
// EXACT distances — a cache hit is recall-perfect, not just fast. The fetch
// function is the vector source (base matrix in the lab, CAS in production);
// Fetches() counts every load it performed (miss admissions + prefetch loads),
// i.e. the tier's total bandwidth.
type ExactTier struct {
	policy  Cache
	lister  Lister
	fetch   func(item int) []float32
	vecs    map[int][]float32
	fetches int
}

// NewExactTier wraps a policy (which must expose membership via Lister).
func NewExactTier(policy Cache, fetch func(item int) []float32) *ExactTier {
	l, ok := policy.(Lister)
	if !ok {
		panic("cache: ExactTier policy must implement Lister")
	}
	return &ExactTier{policy: policy, lister: l, fetch: fetch, vecs: map[int][]float32{}}
}

// Access records an access on the policy and reconciles the vector mirror.
// Returns true when the item was resident (with its exact vector) BEFORE the
// access — the same hit semantics as the underlying policy.
func (t *ExactTier) Access(item int) bool {
	hit := t.policy.Access(item)
	t.reconcile()
	return hit
}

// reconcile makes vecs match the policy membership exactly: fetch what was
// admitted (by miss or prefetch), drop what was evicted. O(capacity) per call.
func (t *ExactTier) reconcile() {
	cur := t.lister.Items()
	in := make(map[int]bool, len(cur))
	for _, it := range cur {
		in[it] = true
		if _, ok := t.vecs[it]; !ok {
			t.vecs[it] = t.fetch(it)
			t.fetches++
		}
	}
	for it := range t.vecs {
		if !in[it] {
			delete(t.vecs, it)
		}
	}
}

// Members returns the resident item ids, sorted ascending.
func (t *ExactTier) Members() []int {
	out := make([]int, 0, len(t.vecs))
	for it := range t.vecs {
		out = append(out, it)
	}
	sort.Ints(out)
	return out
}

// Vec returns the resident exact vector for item, or nil if not in the tier.
func (t *ExactTier) Vec(item int) []float32 { return t.vecs[item] }

// Fetches returns the total number of vector loads the tier performed.
func (t *ExactTier) Fetches() int { return t.fetches }

// Range visits resident (item, vector) pairs in ascending item order
// (deterministic, so tiered search results are reproducible).
func (t *ExactTier) Range(f func(item int, vec []float32)) {
	for _, it := range t.Members() {
		f(it, t.vecs[it])
	}
}
