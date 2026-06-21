package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"matrixsentry/memory"
	"matrixsentry/sentry"
)

func fakeEmbedServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Input []string `json:"input"` }
		json.NewDecoder(r.Body).Decode(&in)
		out := struct{ Embeddings [][]float32 `json:"embeddings"` }{}
		for range in.Input {
			out.Embeddings = append(out.Embeddings, []float32{1, 2, 3, 4})
		}
		json.NewEncoder(w).Encode(out)
	}))
}

func TestReembedRewritesMemoriesPreservesRest(t *testing.T) {
	srv := fakeEmbedServer()
	defer srv.Close()

	srcDir := filepath.Join(t.TempDir(), "src")
	src, err := sentry.Open(srcDir, sentry.Options{})
	if err != nil { t.Fatal(err) }
	src.Append(1, memory.EventMemory, memory.MemoryPayload{ID: 1, Text: "alpha", Vector: []float32{9, 9}, Tags: []string{"x"}})
	src.Append(1, memory.EventMemory, memory.MemoryPayload{ID: 2, Text: "beta", Vector: []float32{8, 8}, Source: "s"})
	src.Append(1, sentry.EventAccess, sentry.AccessPayload{ItemID: 42, Source: "Read"})
	src.Append(1, memory.EventForget, memory.ForgetPayload{ID: 1})
	src.Close()

	src2, _ := sentry.Open(srcDir, sentry.Options{})
	defer src2.Close()
	dstDir := filepath.Join(t.TempDir(), "dst")
	dst, _ := sentry.Open(dstDir, sentry.Options{})

	emb := memory.NewOllamaEmbedder(srv.URL, "bge-m3", 4)
	st, err := reembed(src2, dst, emb, 64)
	if err != nil { t.Fatal(err) }
	if st.records != 4 || st.memories != 2 {
		t.Fatalf("want 4 records / 2 memories, got %+v", st)
	}
	dst.Close()

	dst2, _ := sentry.Open(dstDir, sentry.Options{})
	defer dst2.Close()
	mem, err := memory.New(dst2, emb)
	if err != nil { t.Fatalf("memory.New on re-embedded journal failed: %v", err) }
	live := mem.List(1)
	if len(live) != 1 || live[0].ID != 2 {
		t.Fatalf("live set wrong after reembed: %+v", live)
	}
	if len(live[0].Vector) != 4 {
		t.Fatalf("re-embedded vector dim = %d, want 4", len(live[0].Vector))
	}
	et := sentry.EventAccess
	found := false
	dst2.Scan(sentry.Filter{Type: &et}, func(r sentry.Record) bool {
		var ap sentry.AccessPayload
		if sentry.UnmarshalPayload(r.Payload, &ap) == nil && ap.ItemID == 42 && ap.Source == "Read" {
			found = true
		}
		return true
	})
	if !found { t.Fatal("non-memory (access) record was not copied verbatim") }
}
