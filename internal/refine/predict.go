package refine

import (
	"math/rand"
	"sort"
)

// SelfExcitingStream generates a synthetic agent-access stream that interpolates
// between i.i.d. popularity draws and a deterministic co-access chain. At each
// step, with probability eta the next token is successor[current] (self-exciting
// / sequential structure), otherwise it is drawn uniformly from bag (a popularity
// multiset). eta=0 is pure i.i.d.; eta=1 is a pure successor chain. Deterministic
// for a fixed seed, so the predictive experiment is reproducible.
func SelfExcitingStream(T int, eta float64, bag, successor []int, seed int64) []int {
	rng := rand.New(rand.NewSource(seed))
	out := make([]int, T)
	cur := bag[rng.Intn(len(bag))]
	out[0] = cur
	for t := 1; t < T; t++ {
		if rng.Float64() < eta {
			cur = successor[cur]
		} else {
			cur = bag[rng.Intn(len(bag))]
		}
		out[t] = cur
	}
	return out
}

// Markov is a sparse online first-order transition counter over access tokens.
// It is the predictor that turns "what was just accessed" into "what is about to
// be accessed", driving predictive refinement. Integer counts keep it exact and
// deterministic (no float drift across cores).
type Markov struct {
	counts map[int]map[int]int
}

func NewMarkov() *Markov { return &Markov{counts: make(map[int]map[int]int)} }

// Observe records a from->to transition.
func (m *Markov) Observe(from, to int) {
	row := m.counts[from]
	if row == nil {
		row = make(map[int]int)
		m.counts[from] = row
	}
	row[to]++
}

// Predict returns the up-to-k most likely next tokens after state `from`, by
// observed transition count. Ties break to the lower token id (deterministic).
// An unseen state yields an empty slice.
func (m *Markov) Predict(from, k int) []int {
	row := m.counts[from]
	if len(row) == 0 {
		return nil
	}
	ids := make([]int, 0, len(row))
	for id := range row {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool {
		if row[ids[a]] != row[ids[b]] {
			return row[ids[a]] > row[ids[b]] // higher count first
		}
		return ids[a] < ids[b] // tie -> lower id
	})
	if k > len(ids) {
		k = len(ids)
	}
	return ids[:k]
}
