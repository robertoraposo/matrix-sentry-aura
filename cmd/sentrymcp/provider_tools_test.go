package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"matrixsentry/providerbroker"
	"matrixsentry/sentry"
)

func callProviderTool(
	t *testing.T,
	s *server,
	tenant sentry.TenantID,
	name string,
	args map[string]any,
) rpcResp {
	t.Helper()

	params, err := json.Marshal(map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		t.Fatal(err)
	}

	return s.callTool(rpcReq{
		ID:     json.RawMessage("1"),
		Params: params,
	}, tenant)
}

func newProviderTestServer(t *testing.T) *server {
	t.Helper()

	s := newTestServer(t)
	s.providers = providerbroker.NewRegistry()

	for _, p := range []providerbroker.Provider{
		{
			ID:           "codex",
			Name:         "Codex CLI",
			Auth:         providerbroker.AuthCLI,
			Capabilities: []string{"chat", "tools"},
		},
		{
			ID:           "ollama",
			Name:         "Ollama local",
			Auth:         providerbroker.AuthNone,
			Capabilities: []string{"chat"},
		},
	} {
		if err := s.providers.Register(p); err != nil {
			t.Fatal(err)
		}
	}

	return s
}

func TestProviderToolsAdvertiseOutputSchemas(t *testing.T) {
	for _, name := range []string{
		"provider_list",
		"provider_status",
		"provider_invoke",
	} {
		if outputSchemaOf(t, name) == nil {
			t.Fatalf("%s must advertise an outputSchema", name)
		}
	}
}

func TestProviderListReturnsStructuredProviders(t *testing.T) {
	s := newProviderTestServer(t)

	resp := callProviderTool(
		t,
		s,
		sentry.TenantID(1),
		"provider_list",
		map[string]any{},
	)

	sc := respStruct(t, resp)

	if sc["count"] != 2 {
		t.Fatalf("provider count = %#v, want 2", sc["count"])
	}

	providers, ok := sc["providers"].([]map[string]any)
	if !ok {
		t.Fatalf("providers has unexpected type: %#v", sc["providers"])
	}

	if len(providers) != 2 {
		t.Fatalf("providers length = %d, want 2", len(providers))
	}

	if providers[0]["id"] != "codex" || providers[1]["id"] != "ollama" {
		t.Fatalf("providers not sorted: %#v", providers)
	}
}

func TestProviderStatusIsTenantScoped(t *testing.T) {
	s := newProviderTestServer(t)

	tenantA := sentry.TenantID(1)
	tenantB := sentry.TenantID(2)

	if err := s.providers.SetStatus(
		tenantA,
		"codex",
		providerbroker.Status{
			State:   providerbroker.StateConnected,
			Account: "Roberto",
		},
	); err != nil {
		t.Fatal(err)
	}

	a := respStruct(t, callProviderTool(
		t,
		s,
		tenantA,
		"provider_status",
		map[string]any{"provider": "codex"},
	))

	if a["state"] != "connected" || a["account"] != "Roberto" {
		t.Fatalf("tenant A status = %#v", a)
	}

	b := respStruct(t, callProviderTool(
		t,
		s,
		tenantB,
		"provider_status",
		map[string]any{"provider": "codex"},
	))

	if b["state"] != "disconnected" || b["account"] != "" {
		t.Fatalf("tenant B inherited tenant A status: %#v", b)
	}
}

func TestProviderStatusRejectsUnknownProvider(t *testing.T) {
	s := newProviderTestServer(t)

	resp := callProviderTool(
		t,
		s,
		sentry.TenantID(1),
		"provider_status",
		map[string]any{"provider": "missing"},
	)

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result not a map: %#v", resp.Result)
	}

	if result["isError"] != true {
		t.Fatalf("unknown provider did not return tool error: %#v", result)
	}
}

func TestProviderInvokeOllamaReturnsStructuredResult(t *testing.T) {
	s := newProviderTestServer(t)

	upstream := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Fatalf("method = %s, want POST", r.Method)
			}
			if r.URL.Path != "/api/chat" {
				t.Fatalf("path = %s, want /api/chat", r.URL.Path)
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"model": "qwen3:8b",
				"message": map[string]any{
					"role":    "assistant",
					"content": "Hola desde Matrix.",
				},
				"done":                 true,
				"done_reason":          "stop",
				"total_duration":       9000,
				"load_duration":        1000,
				"prompt_eval_count":    8,
				"prompt_eval_duration": 2000,
				"eval_count":           4,
				"eval_duration":        3000,
			})
		},
	))
	defer upstream.Close()

	s.ollamaClient = upstream.Client()
	s.ollamaURL = upstream.URL

	if err := s.providers.SetDefaultStatus(
		"ollama",
		providerbroker.Status{
			State:   providerbroker.StateConnected,
			Account: "local",
		},
	); err != nil {
		t.Fatal(err)
	}

	resp := callProviderTool(
		t,
		s,
		sentry.TenantID(1),
		"provider_invoke",
		map[string]any{
			"provider": "ollama",
			"model":    "qwen3:8b",
			"system":   "Responde brevemente.",
			"prompt":   "Di hola.",
		},
	)

	sc := respStruct(t, resp)

	if sc["provider"] != "ollama" {
		t.Fatalf("provider = %#v", sc["provider"])
	}
	if sc["model"] != "qwen3:8b" {
		t.Fatalf("model = %#v", sc["model"])
	}
	if sc["content"] != "Hola desde Matrix." {
		t.Fatalf("content = %#v", sc["content"])
	}
	if sc["done"] != true || sc["doneReason"] != "stop" {
		t.Fatalf("completion state = %#v", sc)
	}
	if sc["promptTokens"] != 8 || sc["completionTokens"] != 4 {
		t.Fatalf("token metrics = %#v", sc)
	}
}

func TestProviderInvokeRejectsDisconnectedProvider(t *testing.T) {
	s := newProviderTestServer(t)

	resp := callProviderTool(
		t,
		s,
		sentry.TenantID(1),
		"provider_invoke",
		map[string]any{
			"provider": "ollama",
			"model":    "qwen3:8b",
			"prompt":   "Di hola.",
		},
	)

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result not a map: %#v", resp.Result)
	}
	if result["isError"] != true {
		t.Fatalf("disconnected provider was invoked: %#v", result)
	}
}
