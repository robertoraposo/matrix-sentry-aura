package main

import (
	"context"
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
		"accountType": map[string]any{
			"type":        "string",
			"description": "provider account type when available",
		},
		"planType": map[string]any{
			"type":        "string",
			"description": "provider plan label when available",
		},
		"requiresOpenaiAuth": map[string]any{
			"type":        "boolean",
			"description": "whether the official Codex client requires OpenAI authentication",
		},
		"loginId": map[string]any{
			"type":        "string",
			"description": "opaque pending login identifier; never a provider credential",
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
			"name":        "provider_connect",
			"description": "Start an official provider login through Matrix. For Codex this returns the official device-code URL and short-lived user code; no credentials are exposed.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"provider": map[string]any{"type": "string"},
				},
				"required": []any{"provider"},
			},
			"outputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"provider":        map[string]any{"type": "string"},
					"state":           map[string]any{"type": "string"},
					"type":            map[string]any{"type": "string"},
					"loginId":         map[string]any{"type": "string"},
					"verificationUrl": map[string]any{"type": "string"},
					"userCode":        map[string]any{"type": "string"},
				},
				"required": []any{
					"provider",
					"state",
					"type",
					"loginId",
					"verificationUrl",
					"userCode",
				},
			},
		},
		{
			"name":        "provider_connect_cancel",
			"description": "Cancel a pending official provider login for the authenticated tenant.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"provider": map[string]any{"type": "string"},
					"loginId":  map[string]any{"type": "string"},
				},
				"required": []any{"provider", "loginId"},
			},
			"outputSchema": providerActionOutputSchema(),
		},
		{
			"name":        "provider_disconnect",
			"description": "Disconnect the authenticated tenant's official provider session without returning credential material.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"provider": map[string]any{"type": "string"},
				},
				"required": []any{"provider"},
			},
			"outputSchema": providerActionOutputSchema(),
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
			item := s.providerResultForTenant(tenant, provider)
			structured = append(structured, item)

			state, _ := item["state"].(string)
			lines = append(
				lines,
				fmt.Sprintf(
					"- %s (%s): %s",
					provider.Name,
					provider.ID,
					state,
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

		structured := s.providerResultForTenant(tenant, provider)
		state, _ := structured["state"].(string)
		account, _ := structured["account"].(string)

		text := fmt.Sprintf(
			"provider %s (%s): state=%s",
			provider.Name,
			provider.ID,
			state,
		)
		if account != "" {
			text += " account=" + account
		}

		return s.toolStruct(id, text, structured)

	case "provider_connect":
		providerResponse, ok := s.managedCodexProvider(id, args)
		if !ok {
			return providerResponse
		}
		login, err := s.providerDaemon.startLogin(context.Background(), tenant)
		if err != nil {
			return s.toolErr(id, "unable to start official Codex login: "+err.Error())
		}
		_ = s.providers.SetStatus(
			tenant,
			"codex",
			providerbroker.Status{State: login.State},
		)
		return s.toolStruct(
			id,
			"official Codex device login started",
			map[string]any{
				"provider":        login.Provider,
				"state":           string(login.State),
				"type":            login.Type,
				"loginId":         login.LoginID,
				"verificationUrl": login.VerificationURL,
				"userCode":        login.UserCode,
			},
		)

	case "provider_connect_cancel":
		providerResponse, ok := s.managedCodexProvider(id, args)
		if !ok {
			return providerResponse
		}
		loginID, ok := strArg(args, "loginId")
		if !ok || strings.TrimSpace(loginID) == "" {
			return s.toolErr(id, "loginId is required")
		}
		if err := s.providerDaemon.cancelLogin(
			context.Background(),
			tenant,
			loginID,
		); err != nil {
			return s.toolErr(id, "unable to cancel Codex login: "+err.Error())
		}
		_ = s.providers.SetStatus(
			tenant,
			"codex",
			providerbroker.Status{State: providerbroker.StateDisconnected},
		)
		return s.toolStruct(
			id,
			"Codex login cancelled",
			providerActionResult("codex", providerbroker.StateDisconnected),
		)

	case "provider_disconnect":
		providerResponse, ok := s.managedCodexProvider(id, args)
		if !ok {
			return providerResponse
		}
		if err := s.providerDaemon.logout(context.Background(), tenant); err != nil {
			return s.toolErr(id, "unable to disconnect Codex: "+err.Error())
		}
		_ = s.providers.SetStatus(
			tenant,
			"codex",
			providerbroker.Status{State: providerbroker.StateDisconnected},
		)
		return s.toolStruct(
			id,
			"Codex disconnected",
			providerActionResult("codex", providerbroker.StateDisconnected),
		)

	case "provider_invoke":
		return s.handleProviderInvoke(id, tenant, args)
	}

	return s.toolErr(id, "unknown provider tool")
}

func providerActionOutputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string"},
			"state":    map[string]any{"type": "string"},
		},
		"required": []any{"provider", "state"},
	}
}

func providerActionResult(
	provider string,
	state providerbroker.State,
) map[string]any {
	return map[string]any{
		"provider": provider,
		"state":    string(state),
	}
}

func (s *server) managedCodexProvider(
	id json.RawMessage,
	args map[string]any,
) (rpcResp, bool) {
	providerID, ok := strArg(args, "provider")
	if !ok || strings.TrimSpace(providerID) == "" {
		return s.toolErr(id, "provider is required"), false
	}
	provider, found := findProvider(s.providers, providerID)
	if !found {
		return s.toolErr(
			id,
			fmt.Sprintf("unknown provider %q", providerID),
		), false
	}
	if provider.ID != "codex" {
		return s.toolErr(
			id,
			fmt.Sprintf(
				"connection management is not available for provider %q",
				provider.ID,
			),
		), false
	}
	if s.providerDaemon == nil {
		return s.toolErr(
			id,
			"provider session daemon is not configured",
		), false
	}
	return rpcResp{}, true
}

func (s *server) providerResultForTenant(
	tenant sentry.TenantID,
	provider providerbroker.Provider,
) map[string]any {
	status, _ := s.providers.Status(tenant, provider.ID)
	result := providerResult(provider, status)

	if provider.ID != "codex" || s.providerDaemon == nil {
		return result
	}

	remote, err := s.providerDaemon.status(context.Background(), tenant)
	if err != nil {
		result["state"] = string(providerbroker.StateError)
		result["account"] = ""
		result["requiresOpenaiAuth"] = true
		return result
	}

	_ = s.providers.SetStatus(
		tenant,
		provider.ID,
		providerbroker.Status{
			State:   remote.State,
			Account: remote.Account,
		},
	)

	result["state"] = string(remote.State)
	result["account"] = remote.Account
	result["accountType"] = remote.AccountType
	result["planType"] = remote.PlanType
	result["requiresOpenaiAuth"] = remote.RequiresOpenAIAuth
	result["loginId"] = remote.LoginID
	return result
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
