package providerbroker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeOllamaConnected(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/tags" {
			t.Fatalf("path = %s, want /api/tags", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	got := ProbeOllama(context.Background(), srv.Client(), srv.URL+"/")

	if got.State != StateConnected {
		t.Fatalf("state = %q, want %q", got.State, StateConnected)
	}
	if got.Account != "local" {
		t.Fatalf("account = %q, want local", got.Account)
	}
}

func TestProbeOllamaUnavailable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	got := ProbeOllama(context.Background(), srv.Client(), srv.URL)

	if got.State != StateDisconnected {
		t.Fatalf("state = %q, want %q", got.State, StateDisconnected)
	}
	if got.Account != "" {
		t.Fatalf("account = %q, want empty", got.Account)
	}
}

func TestProbeOllamaEmptyURL(t *testing.T) {
	t.Parallel()

	got := ProbeOllama(context.Background(), http.DefaultClient, "")

	if got.State != StateDisconnected {
		t.Fatalf("state = %q, want %q", got.State, StateDisconnected)
	}
}
