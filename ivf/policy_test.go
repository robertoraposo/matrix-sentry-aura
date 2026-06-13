package ivf

import "testing"

func TestFeatures(t *testing.T) {
	d := []float32{2, 4, 6, 8, 10, 12, 14, 16}
	f := Features(d)
	if f[0] != 2 {
		t.Fatalf("f0 = %v, want 2 (nearest)", f[0])
	}
	if f[2] != 2 { // d1-d0 = 4-2
		t.Fatalf("f2 = %v, want 2 (gap)", f[2])
	}
	if f[1] < 1.99 || f[1] > 2.01 { // d1/d0 ≈ 2
		t.Fatalf("f1 = %v, want ~2", f[1])
	}
	if f[3] < 4.4 || f[3] > 4.6 { // mean(top8)/d0 = 9/2 = 4.5
		t.Fatalf("f3 = %v, want ~4.5", f[3])
	}
}

func TestFeaturesEmpty(t *testing.T) {
	if Features(nil) != [4]float32{} {
		t.Fatal("empty coarse should yield zero features")
	}
}

type constPolicy struct{ np, rk int }

func (c constPolicy) Plan(_ [4]float32) (int, int) { return c.np, c.rk }

// buildPolicyTestIndex reuses rerankFixture (rerank_test.go): clustered data,
// trained+populated index, and a query from the same distribution.
func buildPolicyTestIndex(t *testing.T) (*Index, []float32) {
	t.Helper()
	ix, _, queries := rerankFixture(t)
	return ix, queries[0]
}

// TestSearchPolicyEqualsSearchRerankAtConstantDepth: SearchPolicy with a
// constant policy must equal SearchRerank at the same depth.
func TestSearchPolicyEqualsSearchRerankAtConstantDepth(t *testing.T) {
	ix, query := buildPolicyTestIndex(t)
	fetch := func(uint64) []float32 { return nil } // graceful: keeps ADC estimate
	want := ix.SearchRerank(query, 4, 5, 50, fetch)
	got := ix.SearchPolicy(query, constPolicy{np: 4, rk: 50}, 5, fetch)
	if len(got) != len(want) {
		t.Fatalf("len %d != %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Handle.Hash != want[i].Handle.Hash || got[i].Dist != want[i].Dist {
			t.Fatalf("hit %d differs: %+v vs %+v", i, got[i], want[i])
		}
	}
}

func TestQueryFeaturesFinite(t *testing.T) {
	ix, query := buildPolicyTestIndex(t)
	f := ix.QueryFeatures(query)
	if f[0] < 0 {
		t.Fatalf("f0 (nearest dist) negative: %v", f[0])
	}
}
