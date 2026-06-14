package memory

import (
	"path/filepath"
	"testing"

	"matrixsentry/sentry"
)

// fakeEmbedder maps known texts to fixed vectors so tests control geometry;
// unknown texts get a deterministic but distant vector.
type fakeEmbedder struct {
	dim   int
	table map[string][]float32
}

func (f *fakeEmbedder) Dim() int { return f.dim }

func (f *fakeEmbedder) Embed(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if v, ok := f.table[t]; ok {
			out[i] = append([]float32(nil), v...)
			continue
		}
		v := make([]float32, f.dim)
		for j := range v {
			v[j] = float32((len(t)*7+j*13)%97) * 0.01
		}
		out[i] = v
	}
	return out, nil
}

func newTestStore(t *testing.T, emb Embedder) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	j, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	st, err := New(j, emb)
	if err != nil {
		t.Fatal(err)
	}
	return st, dir
}

// 2-D embedder: "cat"/"kitten" near each other, "rocket" far away.
func geoEmbedder() *fakeEmbedder {
	return &fakeEmbedder{dim: 2, table: map[string][]float32{
		"cat":    {0, 0},
		"kitten": {0.1, 0},
		"feline": {0.5, 0},
		"rocket": {10, 10},
		"engine": {10.1, 10},
	}}
}

func TestRecallReturnsNearestNeighborFirst(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	for _, text := range []string{"cat", "rocket", "feline"} {
		if _, _, _, err := st.Remember(1, text, RememberOpts{}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.Recall(1, "kitten", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("recall returned %d, want 3", len(got))
	}
	if got[0].Text != "cat" {
		t.Fatalf("nearest to 'kitten' = %q, want 'cat'", got[0].Text)
	}
	if got[len(got)-1].Text != "rocket" {
		t.Fatalf("farthest = %q, want 'rocket'", got[len(got)-1].Text)
	}
}

func TestRecallKLimitsResults(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	for _, text := range []string{"cat", "kitten", "feline", "rocket", "engine"} {
		st.Remember(1, text, RememberOpts{})
	}
	got, err := st.Recall(1, "cat", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("k=2 returned %d", len(got))
	}
}

func TestRecallEmptyStoreIsEmptyNotError(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	got, err := st.Recall(1, "cat", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("empty store recall returned %d", len(got))
	}
}

func TestRememberAssignsSequentialIDs(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	a, _, _, _ := st.Remember(1, "cat", RememberOpts{})
	b, _, _, _ := st.Remember(1, "rocket", RememberOpts{})
	if a == 0 || b != a+1 {
		t.Fatalf("ids not sequential: %d then %d", a, b)
	}
}

func TestRecallIsTenantScoped(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	st.Remember(1, "cat", RememberOpts{})
	st.Remember(2, "rocket", RememberOpts{})
	got, _ := st.Recall(1, "kitten", 10)
	if len(got) != 1 || got[0].Text != "cat" {
		t.Fatalf("tenant 1 recall leaked or missed: %+v", got)
	}
}

func TestRememberPersistsAcrossReopenWithoutReembedding(t *testing.T) {
	emb := geoEmbedder()
	dir := t.TempDir()
	j, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	st, err := New(j, emb)
	if err != nil {
		t.Fatal(err)
	}
	st.Remember(1, "cat", RememberOpts{Tags: []string{"animal"}, Src: "Read"})
	st.Remember(1, "rocket", RememberOpts{})
	j.Close()

	// reopen: rebuild from the journal. A nil embedder proves recall over
	// EXISTING memories needs no re-embedding (vectors are persisted); only the
	// incoming query is embedded, so we pass the same embedder for the query.
	j2, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	st2, err := New(j2, emb)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st2.Recall(1, "kitten", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Text != "cat" {
		t.Fatalf("after reopen, recall = %+v", got)
	}
	if len(got[0].Tags) != 1 || got[0].Tags[0] != "animal" {
		t.Fatalf("tags not persisted: %+v", got[0].Tags)
	}
	// next id must continue after the rebuilt max
	id, _, _, _ := st2.Remember(1, "feline", RememberOpts{})
	if id != 3 {
		t.Fatalf("nextID after rebuild = %d, want 3", id)
	}
}

func TestRecallScoresAreDistances(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	st.Remember(1, "cat", RememberOpts{})
	got, _ := st.Recall(1, "cat", 1)
	if got[0].Score != 0 {
		t.Fatalf("self-distance should be 0, got %f", got[0].Score)
	}
}

func TestNewRejectsDimMismatchOnRebuild(t *testing.T) {
	// A persisted memory's vector length must match the embedder dim, or recall
	// math is silently wrong. Rebuild should surface it.
	dir := t.TempDir()
	j, _ := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	st, _ := New(j, &fakeEmbedder{dim: 2, table: nil})
	st.Remember(1, "x", RememberOpts{})
	j.Close()
	j2, _ := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	defer j2.Close()
	if _, err := New(j2, &fakeEmbedder{dim: 5, table: nil}); err == nil {
		t.Fatal("New must reject an embedder whose dim differs from persisted vectors")
	}
}

var _ = filepath.Join

func TestRememberDedupsNearDuplicate(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	st.DedupThreshold = 0.05 // kitten(0.01) < tau < feline(0.25)

	id1, dup1, _, err := st.Remember(1, "cat", RememberOpts{})
	if err != nil || dup1 {
		t.Fatalf("first store: id=%d dup=%v err=%v", id1, dup1, err)
	}
	// "kitten" is within tau of "cat" -> deduped, not persisted, returns id1.
	id2, dup2, _, err := st.Remember(1, "kitten", RememberOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !dup2 {
		t.Fatalf("near-duplicate not deduped")
	}
	if id2 != id1 {
		t.Fatalf("dedup returned id %d, want existing %d", id2, id1)
	}
	if n := st.Count(1); n != 1 {
		t.Fatalf("store grew to %d on a duplicate, want 1", n)
	}
	// "feline" is beyond tau -> genuinely new, persisted.
	_, dup3, _, err := st.Remember(1, "feline", RememberOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if dup3 {
		t.Fatalf("distinct fact wrongly deduped (tau false positive)")
	}
	if n := st.Count(1); n != 2 {
		t.Fatalf("distinct fact not stored: count=%d want 2", n)
	}
}

func TestRememberNoDedupWhenThresholdZero(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	// DedupThreshold defaults to 0 -> feature off, every call persists.
	st.Remember(1, "cat", RememberOpts{})
	_, dup, _, _ := st.Remember(1, "cat", RememberOpts{})
	if dup {
		t.Fatalf("dedup fired with threshold 0 (should be disabled)")
	}
	if n := st.Count(1); n != 2 {
		t.Fatalf("count=%d want 2 (no dedup)", n)
	}
}

func TestRememberDedupIsTenantScoped(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	st.DedupThreshold = 0.05
	if _, _, _, err := st.Remember(2, "cat", RememberOpts{}); err != nil { // tenant 2 stores "cat"
		t.Fatal(err)
	}
	// tenant 1 stores "kitten" — near tenant 2's "cat" but a different tenant,
	// so it must NOT be deduped across the tenant boundary.
	_, dup, _, err := st.Remember(1, "kitten", RememberOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Fatal("dedup fired across tenant boundary")
	}
	if n := st.Count(1); n != 1 {
		t.Fatalf("tenant 1 count = %d, want 1", n)
	}
}

func TestSupersedeReplacesInIndex(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	id1, _, _, err := st.Remember(1, "cat", RememberOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.Remember(1, "rocket", RememberOpts{}); err != nil {
		t.Fatal(err)
	}
	newID, dup, superseded, err := st.Remember(1, "feline", RememberOpts{Supersedes: id1})
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Fatalf("supersede must not report deduped")
	}
	if superseded != id1 {
		t.Fatalf("superseded = %d, want %d", superseded, id1)
	}
	if newID == id1 {
		t.Fatalf("supersede must mint a new id, got the old %d", newID)
	}
	if n := st.Count(1); n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}
	got, _ := st.Recall(1, "kitten", 3)
	for _, m := range got {
		if m.Text == "cat" {
			t.Fatalf("superseded 'cat' still recallable")
		}
	}
	if got[0].Text != "feline" {
		t.Fatalf("nearest = %q, want feline", got[0].Text)
	}
}

func TestSupersedeBypassesDedup(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	st.DedupThreshold = 0.05
	id1, _, _, _ := st.Remember(1, "cat", RememberOpts{})
	newID, dup, superseded, err := st.Remember(1, "kitten", RememberOpts{Supersedes: id1})
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Fatalf("supersede path must not dedup")
	}
	if superseded != id1 || newID == id1 {
		t.Fatalf("expected replace: superseded=%d newID=%d (old %d)", superseded, newID, id1)
	}
	if n := st.Count(1); n != 1 {
		t.Fatalf("count = %d, want 1 (replaced)", n)
	}
}

func TestSupersedeInvalidIDStoresAsNew(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	st.Remember(1, "cat", RememberOpts{})
	_, _, superseded, err := st.Remember(1, "rocket", RememberOpts{Supersedes: 999})
	if err != nil {
		t.Fatal(err)
	}
	if superseded != 0 {
		t.Fatalf("superseded = %d, want 0 (id not found)", superseded)
	}
	if n := st.Count(1); n != 2 {
		t.Fatalf("count = %d, want 2 (stored as new)", n)
	}
}

func TestSupersedeIsTenantScoped(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	id1, _, _, _ := st.Remember(2, "cat", RememberOpts{})
	_, _, superseded, err := st.Remember(1, "kitten", RememberOpts{Supersedes: id1})
	if err != nil {
		t.Fatal(err)
	}
	if superseded != 0 {
		t.Fatalf("superseded = %d, want 0 (cross-tenant)", superseded)
	}
	if n := st.Count(2); n != 1 {
		t.Fatalf("tenant 2 count = %d, want 1 (untouched)", n)
	}
	if n := st.Count(1); n != 1 {
		t.Fatalf("tenant 1 count = %d, want 1", n)
	}
}

func TestSupersedeSurvivesReopen(t *testing.T) {
	emb := geoEmbedder()
	dir := t.TempDir()
	j, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	st, err := New(j, emb)
	if err != nil {
		t.Fatal(err)
	}
	id1, _, _, _ := st.Remember(1, "cat", RememberOpts{})
	st.Remember(1, "feline", RememberOpts{Supersedes: id1})
	j.Close()

	j2, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	st2, err := New(j2, emb)
	if err != nil {
		t.Fatal(err)
	}
	if n := st2.Count(1); n != 1 {
		t.Fatalf("after reopen count = %d, want 1", n)
	}
	got, _ := st2.Recall(1, "kitten", 3)
	if len(got) != 1 || got[0].Text != "feline" {
		t.Fatalf("after reopen recall = %+v, want only feline", got)
	}
	id3, _, _, _ := st2.Remember(1, "rocket", RememberOpts{})
	if id3 != 3 {
		t.Fatalf("next id = %d, want 3 (no reuse)", id3)
	}
}

func TestRememberForceStoresNearDuplicate(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	st.DedupThreshold = 0.05 // kitten(0.01) is within tau of cat
	st.Remember(1, "cat", RememberOpts{})
	_, dup, _, err := st.Remember(1, "kitten", RememberOpts{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Fatalf("force must not dedup")
	}
	if n := st.Count(1); n != 2 {
		t.Fatalf("count = %d, want 2 (forced store)", n)
	}
}

func TestSupersedeTakesPrecedenceOverForce(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	st.DedupThreshold = 0.05
	id1, _, _, _ := st.Remember(1, "cat", RememberOpts{})
	_, _, superseded, err := st.Remember(1, "kitten", RememberOpts{Supersedes: id1, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if superseded != id1 {
		t.Fatalf("superseded = %d, want %d (supersede precedence)", superseded, id1)
	}
	if n := st.Count(1); n != 1 {
		t.Fatalf("count = %d, want 1 (replaced, not added)", n)
	}
}
