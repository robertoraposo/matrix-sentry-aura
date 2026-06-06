package ivf

import "testing"

// makeClusters returns base vectors arranged in `k` well-separated clusters of
// `per` near-identical points each. IDs [c*per, (c+1)*per) belong to cluster c.
func makeClusters(k, per, dim int) [][]float32 {
	var out [][]float32
	for c := 0; c < k; c++ {
		center := float32(c) * 100.0 // clusters 100 apart, impossible to confuse
		for i := 0; i < per; i++ {
			v := make([]float32, dim)
			for d := 0; d < dim; d++ {
				v[d] = center + float32(i%3)*0.01 // tiny intra-cluster jitter
			}
			out = append(out, v)
		}
	}
	return out
}

// With clusters this separated, probing every cell must return only IDs from the
// query's own cluster — proof that coarse routing and per-cell ADC scoring work.
func TestIVFADCRoutesToCorrectCluster(t *testing.T) {
	const k, per, dim = 4, 25, 8
	base := makeClusters(k, per, dim)

	ix, err := Train(base, k /*nlist*/, 2 /*M*/, 16 /*K*/, 25 /*iter*/, 1 /*seed*/)
	if err != nil {
		t.Fatal(err)
	}
	ix.Add(base)

	// Query sitting in cluster 2 (IDs 50..74).
	q := make([]float32, dim)
	for d := range q {
		q[d] = 200.0
	}
	res := ix.Search(q, k /*nprobe=all*/, 5 /*topK*/)
	if len(res) != 5 {
		t.Fatalf("got %d results, want 5", len(res))
	}
	for _, r := range res {
		if r.ID < 2*per || r.ID >= 3*per {
			t.Fatalf("result ID %d is outside cluster 2 [%d,%d)", r.ID, 2*per, 3*per)
		}
	}
}
