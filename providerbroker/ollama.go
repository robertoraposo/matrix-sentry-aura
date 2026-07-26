package providerbroker

import (
	"context"
	"net/http"
	"strings"
)

// ProbeOllama checks the official Ollama HTTP API without invoking a model.
// It returns metadata only and never handles credentials.
func ProbeOllama(
	ctx context.Context,
	client *http.Client,
	baseURL string,
) Status {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return Status{State: StateDisconnected}
	}

	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		baseURL+"/api/tags",
		nil,
	)
	if err != nil {
		return Status{State: StateDisconnected}
	}

	resp, err := client.Do(req)
	if err != nil {
		return Status{State: StateDisconnected}
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK ||
		resp.StatusCode >= http.StatusMultipleChoices {
		return Status{State: StateDisconnected}
	}

	return Status{
		State:   StateConnected,
		Account: "local",
	}
}
