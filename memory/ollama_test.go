package memory

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOllamaEmbedderEmbedsBatch(t *testing.T) {
	var gotModel string
	var gotInput []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Errorf("path = %s, want /api/embed", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		json.Unmarshal(body, &req)
		gotModel, gotInput = req.Model, req.Input
		out := map[string]any{"embeddings": [][]float32{{1, 2, 3}, {4, 5, 6}}}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	emb := NewOllamaEmbedder(srv.URL, "nomic-embed-text", 3)
	got, err := emb.Embed([]string{"hello", "world"})
	if err != nil {
		t.Fatal(err)
	}
	if gotModel != "nomic-embed-text" || len(gotInput) != 2 {
		t.Fatalf("request mismatch: model=%q input=%v", gotModel, gotInput)
	}
	if len(got) != 2 || got[0][0] != 1 || got[1][2] != 6 {
		t.Fatalf("embeddings wrong: %v", got)
	}
	if emb.Dim() != 3 {
		t.Fatalf("Dim = %d, want 3", emb.Dim())
	}
}

func TestOllamaEmbedderErrorsOnDimMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{1, 2}}})
	}))
	defer srv.Close()
	emb := NewOllamaEmbedder(srv.URL, "m", 3) // expects dim 3, server returns 2
	if _, err := emb.Embed([]string{"x"}); err == nil {
		t.Fatal("expected dim-mismatch error")
	}
}

func TestOllamaEmbedderErrorsOnCountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{1, 2, 3}}})
	}))
	defer srv.Close()
	emb := NewOllamaEmbedder(srv.URL, "m", 3)
	if _, err := emb.Embed([]string{"a", "b"}); err == nil {
		t.Fatal("expected count-mismatch error (asked 2, got 1)")
	}
}
