package access

import (
	"matrixsentry/internal/refine"
	"matrixsentry/sentry"
)

type Report struct {
	Total       int
	Scanned     int
	Skipped     int
	Lift        float64
	MarkovHit   float64
	MarginalHit float64
	Coverage    float64
}

// Analyze measures the predictability of an access stream from a specific tenant.
func Analyze(s *sentry.Store, tenant sentry.TenantID) (Report, error) {
	var items []int
	var scanned, skipped int
	tenantCopy := tenant
	etype := sentry.EventAccess
	err := s.Scan(sentry.Filter{Tenant: &tenantCopy, Type: &etype}, func(r sentry.Record) bool {
		scanned++
		var p sentry.AccessPayload
		if err := sentry.UnmarshalPayload(r.Payload, &p); err != nil {
			skipped++
			return true
		}
		items = append(items, int(p.ItemID))
		return true
	})
	if err != nil {
		return Report{}, err
	}

	// The Markov-vs-marginal lift computation is shared with the synthetic-stream
	// calibration (refine.StreamLift), so the live η and the benchmark η are
	// measured by the exact same code.
	l := refine.StreamLift(items)
	return Report{
		Total:       l.Total,
		Scanned:     scanned,
		Skipped:     skipped,
		Lift:        l.Lift,
		MarkovHit:   l.MarkovHit,
		MarginalHit: l.MarginalHit,
		Coverage:    l.Coverage,
	}, nil
}
