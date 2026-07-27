package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"matrixsentry/providerbroker"
	"matrixsentry/sentry"
)

func defaultProviderRegistry() *providerbroker.Registry {
	registry := providerbroker.NewRegistry()

	providers := []providerbroker.Provider{
		{
			ID:           "ollama",
			Name:         "Ollama local",
			Auth:         providerbroker.AuthNone,
			Capabilities: []string{"chat", "embeddings"},
		},
		{
			ID:           "codex",
			Name:         "Codex CLI",
			Auth:         providerbroker.AuthCLI,
			Capabilities: []string{"chat", "code", "tools"},
		},
		{
			ID:           "claude",
			Name:         "Claude Code",
			Auth:         providerbroker.AuthCLI,
			Capabilities: []string{"chat", "code", "tools"},
		},
	}

	for _, provider := range providers {
		if err := registry.Register(provider); err != nil {
			panic(fmt.Sprintf(
				"register default provider %q: %v",
				provider.ID,
				err,
			))
		}
	}

	return registry
}

func providerToolDefinitions() []map[string]any {
	providerProperties := map[string]any{
		"id": map[string]any{
			"type":        "string",
			"description": "stable provider identifier",
		},
		"name": map[string]any{
			"type":        "string",
			"description": "human-readable provider name",
		},
		"auth": map[string]any{
			"type":        "string",
			"description": "authentication method: none | cli | oauth",
		},
		"capabilities": map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "string"},
		},
		"state": map[string]any{
			"type":        "string",
			"description": "disconnected | connecting | connected | error",
		},
		"account": map[string]any{
			"type":        "string",
			"description": "connected account label; empty when disconnected",
		},
	}

	return []map[string]any{
		{
			"name":        "provider_list",
			"description": "List AI providers known to Matrix and their connection state for the authenticated tenant. Returns metadata only and never exposes credentials.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			"outputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"count": map[string]any{"type": "integer"},
					"providers": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type":       "object",
							"properties": providerProperties,
							"required": []any{
								"id",
								"name",
								"auth",
								"capabilities",
								"state",
								"account",
							},
						},
					},
				},
				"required": []any{"count", "providers"},
			},
		},
		{
			"name":        "provider_status",
			"description": "Return the tenant-isolated connection state of one AI provider. No credential material is returned.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"provider": map[string]any{
						"type":        "string",
						"description": "provider identifier, for example ollama, codex, or claude",
					},
				},
				"required": []any{"provider"},
			},
			"outputSchema": map[string]any{
				"type":       "object",
				"properties": providerProperties,
				"required": []any{
					"id",
					"name",
					"auth",
					"capabilities",
					"state",
					"account",
				},
			},
		},

		{
			"name":        "provider_invoke",
			"description": "Invoke a connected AI provider through Matrix without exposing credentials or internal endpoints.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"provider": map[string]any{"type": "string"},
					"model":    map[string]any{"type": "string"},
					"system":   map[string]any{"type": "string"},
					"prompt":   map[string]any{"type": "string"},
				},
				"required": []any{
					"provider",
					"model",
					"prompt",
				},
			},
			"outputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"provider":         map[string]any{"type": "string"},
					"model":            map[string]any{"type": "string"},
					"content":          map[string]any{"type": "string"},
					"done":             map[string]any{"type": "boolean"},
					"doneReason":       map[string]any{"type": "string"},
					"totalDuration":    map[string]any{"type": "integer"},
					"loadDuration":     map[string]any{"type": "integer"},
					"promptTokens":     map[string]any{"type": "integer"},
					"completionTokens": map[string]any{"type": "integer"},
				},
				"required": []any{
					"provider",
					"model",
					"content",
					"done",
					"doneReason",
					"totalDuration",
					"loadDuration",
					"promptTokens",
					"completionTokens",
				},
			},
		},
	}
}

func (s *server) handleProviderTool(
	id json.RawMessage,
	tenant sentry.TenantID,
	name string,
	args map[string]any,
) rpcResp {
	if s.providers == nil {
		return s.toolErr(id, "provider registry is not initialized")
	}

	switch name {
	case "provider_list":
		providers := s.providers.List()
		structured := make([]map[string]any, 0, len(providers))
		lines := make([]string, 0, len(providers)+1)

		lines = append(
			lines,
			fmt.Sprintf(
				"providers available to tenant %d: %d",
				tenant,
				len(providers),
			),
		)

		for _, provider := range providers {
			status, _ := s.providers.Status(tenant, provider.ID)
			item := providerResult(provider, status)
			structured = append(structured, item)

			lines = append(
				lines,
				fmt.Sprintf(
					"- %s (%s): %s",
					provider.Name,
					provider.ID,
					status.State,
				),
			)
		}

		return s.toolStruct(
			id,
			strings.Join(lines, "\n"),
			map[string]any{
				"count":     len(structured),
				"providers": structured,
			},
		)

	case "provider_status":
		providerID, ok := strArg(args, "provider")
		if !ok || strings.TrimSpace(providerID) == "" {
			return s.toolErr(id, "provider is required")
		}

		provider, found := findProvider(s.providers, providerID)
		if !found {
			return s.toolErr(
				id,
				fmt.Sprintf("unknown provider %q", providerID),
			)
		}

		status, _ := s.providers.Status(tenant, provider.ID)
		structured := providerResult(provider, status)

		text := fmt.Sprintf(
			"provider %s (%s): state=%s",
			provider.Name,
			provider.ID,
			status.State,
		)
		if status.Account != "" {
			text += " account=" + status.Account
		}

		return s.toolStruct(id, text, structured)
	case "provider_invoke":
		return s.handleProviderInvoke(id, tenant, args)
	}

	return s.toolErr(id, "unknown provider tool")
}

func findProvider(
	registry *providerbroker.Registry,
	providerID string,
) (providerbroker.Provider, bool) {
	providerID = strings.TrimSpace(providerID)

	for _, provider := range registry.List() {
		if provider.ID == providerID {
			return provider, true
		}
	}

	return providerbroker.Provider{}, false
}

func providerResult(
	provider providerbroker.Provider,
	status providerbroker.Status,
) map[string]any {
	return map[string]any{
		"id":           provider.ID,
		"name":         provider.Name,
		"auth":         string(provider.Auth),
		"capabilities": append([]string(nil), provider.Capabilities...),
		"state":        string(status.State),
		"account":      status.Account,
	}
}
