package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"matrixsentry/providerbroker"
	"matrixsentry/sentry"
)

func TestRefreshOllamaStatusUpdatesDefaultProviderState(t *testing.T) {
	registry := defaultProviderRegistry()

	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Fatalf("path = %s, want /api/tags", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[]}`))
	}))
	defer ollama.Close()

	err := refreshOllamaStatus(
		context.Background(),
		registry,
		ollama.Client(),
		ollama.URL,
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, tenant := range []sentry.TenantID{1, 2, 99} {
		got, ok := registry.Status(tenant, "ollama")
		if !ok {
			t.Fatalf("tenant %d: provider not found", tenant)
		}
		if got.State != providerbroker.StateConnected ||
			got.Account != "local" {
			t.Fatalf("tenant %d: status = %+v", tenant, got)
		}
	}
}

func TestRefreshOllamaStatusMarksUnavailableAsDisconnected(t *testing.T) {
	registry := defaultProviderRegistry()

	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ollama.Close()

	err := refreshOllamaStatus(
		context.Background(),
		registry,
		ollama.Client(),
		ollama.URL,
	)
	if err != nil {
		t.Fatal(err)
	}

	got, ok := registry.Status(1, "ollama")
	if !ok {
		t.Fatal("ollama provider not found")
	}
	if got.State != providerbroker.StateDisconnected {
		t.Fatalf("status = %+v, want disconnected", got)
	}
}

func TestMonitorOllamaStatusRefreshesOnTicks(t *testing.T) {
	registry := defaultProviderRegistry()

	var calls atomic.Int32
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"models":[]}`))
			return
		}

		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ollama.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	done := make(chan struct{})

	go func() {
		defer close(done)
		monitorOllamaStatus(
			ctx,
			registry,
			ollama.Client(),
			ollama.URL,
			ticks,
		)
	}()

	waitForOllamaState(
		t,
		registry,
		providerbroker.StateConnected,
	)

	ticks <- time.Now()

	waitForOllamaState(
		t,
		registry,
		providerbroker.StateDisconnected,
	)

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("monitor did not stop after cancellation")
	}
}

func waitForOllamaState(
	t *testing.T,
	registry *providerbroker.Registry,
	want providerbroker.State,
) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got, ok := registry.Status(1, "ollama")
		if ok && got.State == want {
			return
		}
		time.Sleep(time.Millisecond)
	}

	got, _ := registry.Status(1, "ollama")
	t.Fatalf("ollama state = %q, want %q", got.State, want)
}
