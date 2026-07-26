package main

import (
	"context"
	"net/http"
	"time"

	"matrixsentry/providerbroker"
)

func refreshOllamaStatus(
	ctx context.Context,
	registry *providerbroker.Registry,
	client *http.Client,
	ollamaURL string,
) error {
	status := providerbroker.ProbeOllama(
		ctx,
		client,
		ollamaURL,
	)

	return registry.SetDefaultStatus(
		"ollama",
		status,
	)
}

// monitorOllamaStatus refreshes Ollama immediately and after every tick.
// The caller owns the ticker and cancellation lifecycle.
func monitorOllamaStatus(
	ctx context.Context,
	registry *providerbroker.Registry,
	client *http.Client,
	ollamaURL string,
	ticks <-chan time.Time,
) {
	_ = refreshOllamaStatus(
		ctx,
		registry,
		client,
		ollamaURL,
	)

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticks:
			_ = refreshOllamaStatus(
				ctx,
				registry,
				client,
				ollamaURL,
			)
		}
	}
}
