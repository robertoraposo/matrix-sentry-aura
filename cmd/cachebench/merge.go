package main

import (
	"sort"

	"matrixsentry/pq"
)

// mergeTiered combines the ADC shortlist with the exact tier's distances:
// a tier item replaces its ADC entry (exact distance wins), tier items absent
// from the shortlist are inserted as candidates, and the merged list is
// re-ranked by distance (ties to lower ID) and truncated to topK. Tier items
// are therefore in the candidate set REGARDLESS of cell routing — a resident
// true neighbor cannot be lost to a coarse-quantizer miss.
func mergeTiered(base []pq.Result, tier map[int]float32, topK int) []pq.Result {
	merged := make([]pq.Result, 0, len(base)+len(tier))
	for _, r := range base {
		if _, ok := tier[r.ID]; ok {
			continue // replaced by its exact entry below
		}
		merged = append(merged, r)
	}
	for id, d := range tier {
		merged = append(merged, pq.Result{ID: id, Dist: d})
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Dist != merged[j].Dist {
			return merged[i].Dist < merged[j].Dist
		}
		return merged[i].ID < merged[j].ID
	})
	if len(merged) > topK {
		merged = merged[:topK]
	}
	return merged
}
