package ivf

import "testing"

func TestRecommendedIsTheEvolvedConfig(t *testing.T) {
	cfg, s, err := Recommended(768)
	if err != nil {
		t.Fatalf("Recommended(768) errored: %v", err)
	}
	// Pinned to the GA-validated frontier config 64-96-64-32-200
	// (cross-confirmed at lmax 1.3x and 2.0x; see results/evolve-*).
	if cfg.Nlist != 64 || cfg.M != 96 || cfg.K != 64 {
		t.Fatalf("geometry = nlist %d M %d K %d, want 64/96/64", cfg.Nlist, cfg.M, cfg.K)
	}
	if s.Nprobe != 32 || s.RerankK != 200 {
		t.Fatalf("search = nprobe %d rerankK %d, want 32/200", s.Nprobe, s.RerankK)
	}
}

func TestRecommendedConfigIsValid(t *testing.T) {
	cfg, _, err := Recommended(768)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg); err != nil {
		t.Fatalf("New rejected the recommended config: %v", err)
	}
}

func TestRecommendedRejectsIncompatibleDim(t *testing.T) {
	// M=96 does not divide 100; must error rather than hand back a config New
	// will reject downstream.
	if _, _, err := Recommended(100); err == nil {
		t.Fatal("Recommended(100) must error: 100 not divisible by M=96")
	}
}

func TestRecommendedTrainOptsMatchValidatedRun(t *testing.T) {
	cfg, _, _ := Recommended(768)
	if cfg.Train.CoarseIter != 10 || cfg.Train.PQSample != 65536 {
		t.Fatalf("train opts %+v differ from the validated run (coarseIter 10, pqSample 65536)", cfg.Train)
	}
	if cfg.Iter != 25 {
		t.Fatalf("Iter %d, want 25 (validated run)", cfg.Iter)
	}
}
