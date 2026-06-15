package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("served"))
	})
}

func TestBasicAuthOpenWhenUnset(t *testing.T) {
	h := basicAuth("", "", okHandler())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("open mode should serve, got %d", rec.Code)
	}
}

func TestBasicAuthRejectsMissingAndWrong(t *testing.T) {
	h := basicAuth("admin", "secret", okHandler())

	// no credentials
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing creds → 401, got %d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("should set WWW-Authenticate challenge")
	}

	// wrong password
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("admin", "nope")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong pass → 401, got %d", rec.Code)
	}
}

func TestBasicAuthAcceptsCorrect(t *testing.T) {
	h := basicAuth("admin", "secret", okHandler())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("admin", "secret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct creds → serve, got %d", rec.Code)
	}
}
