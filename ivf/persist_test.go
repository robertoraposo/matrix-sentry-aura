package ivf

import (
	"bytes"
	"reflect"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	ix, _ := New(Config{Dim: 16, Nlist: 8, M: 4, K: 32, Iter: 15, Seed: 7})
	base := pseudoCorpus(200, 16)
	if err := ix.Train(base); err != nil {
		t.Fatal(err)
	}
	ix.Add(base)
	queries := pseudoCorpus(20, 16)
	before := ix.SearchBatch(queries, 4, 10)

	var buf bytes.Buffer
	if err := ix.Save(&buf); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(&buf)
	if err != nil {
		t.Fatal(err)
	}

	if loaded.Ntotal() != ix.Ntotal() {
		t.Fatalf("Ntotal after load = %d, want %d", loaded.Ntotal(), ix.Ntotal())
	}
	after := loaded.SearchBatch(queries, 4, 10)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("search results differ after Save/Load round-trip")
	}

	// Dedup maps must be rebuilt: re-adding a stored vector is recognized.
	r := loaded.Add([][]float32{base[0]})
	if r[0].Status != ExactDuplicate {
		t.Fatalf("post-load re-add status = %v, want ExactDuplicate", r[0].Status)
	}
}
