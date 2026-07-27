package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"matrixsentry/providerbroker"
	"matrixsentry/sentry"
)

func (s *server) handleProviderInvoke(
	id json.RawMessage,
	tenant sentry.TenantID,
	args map[string]any,
) rpcResp {
	providerID, ok := strArg(args, "provider")
	if !ok || strings.TrimSpace(providerID) == "" {
		return s.toolErr(id, "provider is required")
	}

	model, ok := strArg(args, "model")
	if !ok || strings.TrimSpace(model) == "" {
		return s.toolErr(id, "model is required")
	}

	prompt, ok := strArg(args, "prompt")
	if !ok || strings.TrimSpace(prompt) == "" {
		return s.toolErr(id, "prompt is required")
	}

	system, _ := strArg(args, "system")

	provider, found := findProvider(s.providers, providerID)
	if !found {
		return s.toolErr(
			id,
			fmt.Sprintf("unknown provider %q", providerID),
		)
	}

	status, _ := s.providers.Status(tenant, provider.ID)
	if status.State != providerbroker.StateConnected {
		return s.toolErr(
			id,
			fmt.Sprintf("provider %s is not connected", provider.ID),
		)
	}

	if provider.ID != "ollama" {
		return s.toolErr(
			id,
			fmt.Sprintf(
				"provider %s invocation is not implemented",
				provider.ID,
			),
		)
	}

	result, err := providerbroker.InvokeOllama(
		context.Background(),
		s.ollamaClient,
		s.ollamaURL,
		providerbroker.ChatRequest{
			Model:  model,
			System: system,
			Prompt: prompt,
		},
	)
	if err != nil {
		return s.toolErr(
			id,
			"provider invocation failed: "+err.Error(),
		)
	}

	structured := map[string]any{
		"provider":         provider.ID,
		"model":            result.Model,
		"content":          result.Content,
		"done":             result.Done,
		"doneReason":       result.DoneReason,
		"totalDuration":    result.TotalDuration,
		"loadDuration":     result.LoadDuration,
		"promptTokens":     result.PromptEvalCount,
		"completionTokens": result.EvalCount,
	}

	return s.toolStruct(id, result.Content, structured)
}
