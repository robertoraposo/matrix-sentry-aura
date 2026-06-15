package memory

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMistralEmbedHappyPath(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		io.WriteString(w, `{"data":[{"index":0,"embedding":[1,2,3]},{"index":1,"embedding":[4,5,6]}]}`)
	}))
	defer srv.Close()

	e := NewMistralEmbedder("sk-secret", "mistral-embed", 3)
	e.url = srv.URL // override base URL for the test

	out, err := e.Embed([]string{"alpha", "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sk-secret" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"mistral-embed"`) || !strings.Contains(gotBody, `"alpha"`) {
		t.Fatalf("body = %q", gotBody)
	}
	if len(out) != 2 || out[0][0] != 1 || out[1][2] != 6 {
		t.Fatalf("vectors = %v", out)
	}
}

func TestMistralEmbedCountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"embedding":[1,2,3]}]}`) // asked 2, got 1
	}))
	defer srv.Close()
	e := NewMistralEmbedder("k", "mistral-embed", 3)
	e.url = srv.URL
	if _, err := e.Embed([]string{"a", "b"}); err == nil {
		t.Fatal("expected count-mismatch error")
	}
}

func TestMistralEmbedDimMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"embedding":[1,2]}]}`) // dim 2, want 3
	}))
	defer srv.Close()
	e := NewMistralEmbedder("k", "mistral-embed", 3)
	e.url = srv.URL
	_, err := e.Embed([]string{"a"})
	if err == nil || !strings.Contains(err.Error(), "dim") {
		t.Fatalf("expected dim error, got %v", err)
	}
}

func TestMistralEmbedErrorHidesKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"message":"unauthorized: sk-secret"}`)
	}))
	defer srv.Close()
	e := NewMistralEmbedder("sk-secret", "mistral-embed", 3)
	e.url = srv.URL
	_, err := e.Embed([]string{"a"})
	if err == nil {
		t.Fatal("expected non-200 error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error should include status: %v", err)
	}
	if strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("error must NOT leak the api key: %v", err)
	}
}

func TestMistralDim(t *testing.T) {
	if NewMistralEmbedder("k", "mistral-embed", 1024).Dim() != 1024 {
		t.Fatal("Dim mismatch")
	}
}
