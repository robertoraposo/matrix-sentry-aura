package main

import (
	"encoding/json"
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
	for _, name := range []string{"provider_list", "provider_status"} {
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
