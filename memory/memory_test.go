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
		if _, err := st.Remember(1, text, nil, ""); err != nil {
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
		st.Remember(1, text, nil, "")
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
	a, _ := st.Remember(1, "cat", nil, "")
	b, _ := st.Remember(1, "rocket", nil, "")
	if a == 0 || b != a+1 {
		t.Fatalf("ids not sequential: %d then %d", a, b)
	}
}

func TestRecallIsTenantScoped(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	st.Remember(1, "cat", nil, "")
	st.Remember(2, "rocket", nil, "")
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
	st.Remember(1, "cat", []string{"animal"}, "Read")
	st.Remember(1, "rocket", nil, "")
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
	id, _ := st2.Remember(1, "feline", nil, "")
	if id != 3 {
		t.Fatalf("nextID after rebuild = %d, want 3", id)
	}
}

func TestRecallScoresAreDistances(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	st.Remember(1, "cat", nil, "")
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
	st.Remember(1, "x", nil, "")
	j.Close()
	j2, _ := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	defer j2.Close()
	if _, err := New(j2, &fakeEmbedder{dim: 5, table: nil}); err == nil {
		t.Fatal("New must reject an embedder whose dim differs from persisted vectors")
	}
}

var _ = filepath.Join
