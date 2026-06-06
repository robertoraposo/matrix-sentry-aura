package ivf

import "testing"

func TestNewValidatesConfig(t *testing.T) {
	good := Config{Dim: 16, Nlist: 8, M: 4, K: 32, Iter: 10, Seed: 1}
	if _, err := New(good); err != nil {
		t.Fatalf("New rejected a valid config: %v", err)
	}

	bad := []Config{
		{Dim: 0, Nlist: 8, M: 4, K: 32, Iter: 10},   // Dim<=0
		{Dim: 15, Nlist: 8, M: 4, K: 32, Iter: 10},  // Dim%M != 0
		{Dim: 16, Nlist: 0, M: 4, K: 32, Iter: 10},  // Nlist<=0
		{Dim: 16, Nlist: 8, M: 0, K: 32, Iter: 10},  // M<=0
		{Dim: 16, Nlist: 8, M: 4, K: 257, Iter: 10}, // K>256
		{Dim: 16, Nlist: 8, M: 4, K: 0, Iter: 10},   // K<=0
		{Dim: 16, Nlist: 8, M: 4, K: 32, Iter: 0},   // Iter<=0
	}
	for i, cfg := range bad {
		if _, err := New(cfg); err == nil {
			t.Fatalf("New accepted invalid config #%d: %+v", i, cfg)
		}
	}
}

func TestTrainPopulatesCoarseAndPQ(t *testing.T) {
	cfg := Config{Dim: 16, Nlist: 8, M: 4, K: 32, Iter: 10, Seed: 3}
	ix, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	learn := pseudoCorpus(400, 16)
	if err := ix.Train(learn); err != nil {
		t.Fatal(err)
	}
	if len(ix.coarse) != 8 {
		t.Fatalf("coarse cells = %d, want 8", len(ix.coarse))
	}
	if ix.pq == nil {
		t.Fatal("pq not trained")
	}
	if len(ix.lists) != 8 {
		t.Fatalf("lists not sized to nlist: %d", len(ix.lists))
	}
}

func TestTrainRejectsEmptyLearn(t *testing.T) {
	ix, _ := New(Config{Dim: 16, Nlist: 8, M: 4, K: 32, Iter: 10, Seed: 3})
	if err := ix.Train(nil); err == nil {
		t.Fatal("Train must reject an empty learn set")
	}
}

func TestAddInsertsAndDedupsExact(t *testing.T) {
	ix, _ := New(Config{Dim: 16, Nlist: 8, M: 4, K: 32, Iter: 10, Seed: 4})
	if err := ix.Train(pseudoCorpus(400, 16)); err != nil {
		t.Fatal(err)
	}
	base := pseudoCorpus(50, 16)

	res := ix.Add(base)
	if len(res) != 50 {
		t.Fatalf("got %d results, want 50", len(res))
	}
	for i, r := range res {
		if r.Status != Inserted {
			t.Fatalf("result %d: status %v, want Inserted", i, r.Status)
		}
	}
	if ix.Ntotal() != 50 {
		t.Fatalf("Ntotal = %d, want 50", ix.Ntotal())
	}

	// Re-adding the exact same vectors must be recognized, not stored again.
	res2 := ix.Add(base)
	for i, r := range res2 {
		if r.Status != ExactDuplicate {
			t.Fatalf("re-add %d: status %v, want ExactDuplicate", i, r.Status)
		}
		if r.Prior != res[i].Handle.Hash {
			t.Fatalf("re-add %d: Prior %d, want %d", i, r.Prior, res[i].Handle.Hash)
		}
	}
	if ix.Ntotal() != 50 {
		t.Fatalf("Ntotal after dup re-add = %d, want 50", ix.Ntotal())
	}
}

func TestSearchFindsAddedVectorByHandle(t *testing.T) {
	ix, _ := New(Config{Dim: 16, Nlist: 8, M: 4, K: 32, Iter: 15, Seed: 5})
	base := pseudoCorpus(200, 16)
	if err := ix.Train(base); err != nil {
		t.Fatal(err)
	}
	res := ix.Add(base)

	// Querying with an exact stored vector should return its own handle first.
	hits := ix.Search(base[7], len(ix.coarse), 5) // nprobe = nlist => exhaustive
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].Handle.Hash != res[7].Handle.Hash {
		t.Fatalf("top hit hash = %d, want %d (self)", hits[0].Handle.Hash, res[7].Handle.Hash)
	}
}

func TestRecallExactAndTolerant(t *testing.T) {
	ix, _ := New(Config{Dim: 16, Nlist: 8, M: 4, K: 32, Iter: 15, Seed: 6})
	base := pseudoCorpus(200, 16)
	if err := ix.Train(base); err != nil {
		t.Fatal(err)
	}
	res := ix.Add(base)

	// A stored vector is recalled at tol=0 (its own code matches).
	got, ok := ix.Recall(base[10], 0)
	if !ok || got.Hash != res[10].Handle.Hash {
		t.Fatalf("Recall(stored, 0) = (%v,%v), want self %d", got.Hash, ok, res[10].Handle.Hash)
	}

	// White-box: build a query whose code differs from a stored neighbor and
	// assert Recall's success tracks the actual Hamming tolerance exactly.
	q := cloneF32(base[10])
	q[0] += 50 // perturb subspace 0 enough to likely flip its code byte
	c := nearest(ix.coarse, q)
	qcode := ix.pq.Encode(sub(q, ix.coarse[c]))

	// Find the minimum Hamming distance from qcode to any code in q's cell.
	minH := ix.cfg.M + 1
	for _, pos := range ix.lists[c] {
		if e := ix.entries[pos]; e.Cell == uint32(c) {
			if d := hammingBytes(qcode, e.Code); d < minH {
				minH = d
			}
		}
	}
	if minH > ix.cfg.M {
		t.Skip("perturbed query landed in an empty cell; rerun is deterministic")
	}
	// Below the true min Hamming: no recall. At/above: recall succeeds.
	if minH > 0 {
		if _, ok := ix.Recall(q, minH-1); ok {
			t.Fatalf("Recall succeeded at tol=%d below true min Hamming %d", minH-1, minH)
		}
	}
	if _, ok := ix.Recall(q, minH); !ok {
		t.Fatalf("Recall failed at tol=%d == true min Hamming", minH)
	}
}
