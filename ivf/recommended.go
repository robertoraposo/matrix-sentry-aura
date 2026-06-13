package ivf

import "fmt"

// SearchParams are the query-time knobs that pair with a built Index: how many
// cells to probe and how deep to re-rank exactly. They are not part of Config
// because they can change per query without rebuilding.
type SearchParams struct {
	Nprobe  int
	RerankK int // 0 = pure ADC (plain Search); >0 = SearchRerank depth
}

// Recommended returns the engine configuration the GA auto-tuner selected and
// validated for dim-dimensional embeddings, together with its search params.
//
// The values are the frontier config 64-96-64-32-200, cross-confirmed at two
// independent latency budgets (lmax 1.3x and 2.0x; see results/evolve-*.json):
// worst-seed holdout recall@10 0.985 vs the hand-tuned baseline's 0.916
// (+6.9pp) at ~1.18x latency on the real 768-d set. The non-obvious choice is
// K=64 (a coarser, cheaper PQ): it beats K=256 here only because the exact
// re-rank stage (RerankK=200, served from the originals via SearchRerank)
// repairs the added quantization error — a co-adaptation hand-tuning misses.
//
// It errors rather than returning an invalid Config when dim is not divisible
// by M (the geometry was validated at dim=768).
func Recommended(dim int) (Config, SearchParams, error) {
	const m = 96
	if dim <= 0 || dim%m != 0 {
		return Config{}, SearchParams{}, fmt.Errorf("ivf: recommended config needs dim divisible by M=%d, got %d", m, dim)
	}
	cfg := Config{
		Dim:   dim,
		Nlist: 64,
		M:     m,
		K:     64,
		Iter:  25,
		Seed:  1,
		Train: TrainOpts{CoarseIter: 10, CoarseSample: 0, PQSample: 65536},
	}
	return cfg, SearchParams{Nprobe: 32, RerankK: 200}, nil
}
