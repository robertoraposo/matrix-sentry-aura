package refine

// Lift summarizes how predictable an access stream is: how often a first-order
// Markov model beats the marginal (most-frequent) baseline at calling the next
// access. It is the single source of truth for the "lift" metric, shared by the
// live access analyzer (sentry/access) and the synthetic-stream calibration.
type Lift struct {
	Total       int
	Predictable int
	MarkovHit   float64 // hits / predictable steps
	MarginalHit float64 // hits / (Total-1)
	Lift        float64 // MarkovHit - MarginalHit
	Coverage    float64 // predictable steps / (Total-1)
}

// StreamLift runs the online causal comparison over a sequence of item ids: at
// each step it predicts the next id from history only (Markov vs marginal),
// then observes. Ties break to the lower id, deterministically.
func StreamLift(items []int) Lift {
	if len(items) < 2 {
		return Lift{Total: len(items)}
	}

	m := NewMarkov()
	marginal := make(map[int]int)
	var markovHits, marginalHits, predictableSteps int

	for t := 1; t < len(items); t++ {
		prev, curr := items[t-1], items[t]

		// Markov: predict from the just-seen state (history before t only).
		if pred := m.Predict(prev, 1); len(pred) > 0 {
			predictableSteps++
			if pred[0] == curr {
				markovHits++
			}
		}

		// Marginal: predict the most-frequent id seen so far.
		bestMarginal, maxCount := -1, -1
		for id, count := range marginal {
			if count > maxCount {
				bestMarginal, maxCount = id, count
			} else if count == maxCount && id < bestMarginal {
				bestMarginal = id
			}
		}
		if bestMarginal == curr {
			marginalHits++
		}

		// Observe (learn for future steps).
		m.Observe(prev, curr)
		marginal[curr]++
	}

	r := Lift{Total: len(items), Predictable: predictableSteps}
	if predictableSteps == 0 {
		return r
	}
	r.MarkovHit = float64(markovHits) / float64(predictableSteps)
	r.MarginalHit = float64(marginalHits) / float64(len(items)-1)
	r.Coverage = float64(predictableSteps) / float64(len(items)-1)
	r.Lift = r.MarkovHit - r.MarginalHit
	return r
}
