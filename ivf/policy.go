package ivf

import "sort"

// Features derives per-query policy features from the coarse-distance vector
// (distances to every centroid, ascending):
//
//	f0 = d0                  nearest-centroid distance (query density)
//	f1 = d1/(d0+eps)         routing-ambiguity ratio (~1 ambiguous, large = clear)
//	f2 = d1 - d0             absolute margin/gap
//	f3 = mean(top8)/(d0+eps) spread across the top cells
func Features(sorted []float32) [4]float32 {
	if len(sorted) == 0 {
		return [4]float32{}
	}
	const eps = float32(1e-9)
	d0 := sorted[0]
	d1 := d0
	if len(sorted) > 1 {
		d1 = sorted[1]
	}
	n := 8
	if n > len(sorted) {
		n = len(sorted)
	}
	var s float32
	for i := 0; i < n; i++ {
		s += sorted[i]
	}
	mean := s / float32(n)
	return [4]float32{d0, d1 / (d0 + eps), d1 - d0, mean / (d0 + eps)}
}

// coarseSorted returns the centroid distances to query, ascending.
func (ix *Index) coarseSorted(query []float32) []float32 {
	dists := make([]float32, len(ix.coarse))
	for c := range ix.coarse {
		dists[c] = float32(sqL2(ix.coarse[c], query))
	}
	sort.Slice(dists, func(i, j int) bool { return dists[i] < dists[j] })
	return dists
}

// QueryFeatures returns the per-query margin features for q — the same features
// SearchPolicy feeds its policy. Exposed so experiment/runner code can analyze
// or replay policy decisions.
func (ix *Index) QueryFeatures(q []float32) [4]float32 {
	return Features(ix.coarseSorted(q))
}

// Policy maps per-query features to a search depth.
type Policy interface {
	Plan(feats [4]float32) (nprobe, rerankK int)
}

// SearchPolicy is SearchRerank at a per-query depth chosen by p from the query's
// margin features. With a constant policy it is exactly SearchRerank.
func (ix *Index) SearchPolicy(query []float32, p Policy, topK int, fetch func(uint64) []float32) []Hit {
	np, rk := p.Plan(ix.QueryFeatures(query))
	return ix.SearchRerank(query, np, topK, rk, fetch)
}
