package access

import (
	"matrixsentry/internal/refine"
	"matrixsentry/sentry"
)

type Report struct {
	Total       int
	Lift        float64
	MarkovHit   float64
	MarginalHit float64
	Coverage    float64
}

// Analyze measures the predictability of an access stream from a specific tenant.
func Analyze(s *sentry.Store, tenant sentry.TenantID) (Report, error) {
	var items []int
	tenantCopy := tenant
	etype := sentry.EventAccess
	err := s.Scan(sentry.Filter{Tenant: &tenantCopy, Type: &etype}, func(r sentry.Record) bool {
		var p sentry.AccessPayload
		if err := sentry.UnmarshalPayload(r.Payload, &p); err == nil {
			items = append(items, int(p.ItemID))
		}
		return true
	})
	if err != nil {
		return Report{}, err
	}

	if len(items) < 2 {
		return Report{Total: len(items)}, nil
	}

	m := refine.NewMarkov()
	marginal := make(map[int]int)
	var markovHits, marginalHits, predictableSteps int

	for t := 1; t < len(items); t++ {
		prev := items[t-1]
		curr := items[t]

		// 1. Predict (online causal: using only data before t)

		// Markov prediction
		markovPred := m.Predict(prev, 1)
		if len(markovPred) > 0 {
			predictableSteps++
			if markovPred[0] == curr {
				markovHits++
			}
		}

		// Marginal prediction (popularity)
		bestMarginal, maxCount := -1, -1
		for id, count := range marginal {
			if count > maxCount {
				bestMarginal, maxCount = id, count
			} else if count == maxCount && id < bestMarginal {
				bestMarginal = id // deterministic tie-break
			}
		}
		if bestMarginal == curr {
			marginalHits++
		}

		// 2. Observe (learn for future steps)
		m.Observe(prev, curr)
		marginal[curr]++
	}

	if predictableSteps == 0 {
		return Report{Total: len(items)}, nil
	}

	r := Report{
		Total:       len(items),
		MarkovHit:   float64(markovHits) / float64(predictableSteps),
		MarginalHit: float64(marginalHits) / float64(len(items)-1),
		Coverage:    float64(predictableSteps) / float64(len(items)-1),
	}
	r.Lift = r.MarkovHit - r.MarginalHit

	return r, nil
}
