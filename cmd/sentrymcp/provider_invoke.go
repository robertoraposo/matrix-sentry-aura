package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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

	switch provider.ID {
	case "ollama":
		status, _ := s.providers.Status(tenant, provider.ID)
		if status.State != providerbroker.StateConnected {
			return s.toolErr(
				id,
				fmt.Sprintf("provider %s is not connected", provider.ID),
			)
		}

		result, err := providerbroker.InvokeOllama(
			context.Background(),
			s.ollamaClient,
			s.ollamaURL,
			providerbroker.ChatRequest{
				Model:     model,
				System:    system,
				Prompt:    prompt,
				MaxTokens: s.ollamaMaxTokens,
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

	case "codex":
		if s.providerDaemon == nil {
			return s.toolErr(
				id,
				"provider session daemon is not configured",
			)
		}

		statusCtx, statusCancel := context.WithTimeout(
			context.Background(),
			25*time.Second,
		)
		status, err := s.providerDaemon.status(statusCtx, tenant)
		statusCancel()
		if err != nil {
			return s.toolErr(
				id,
				"provider status failed: "+err.Error(),
			)
		}
		if status.State != providerbroker.StateConnected {
			return s.toolErr(id, "provider codex is not connected")
		}

		invokeCtx, invokeCancel := context.WithTimeout(
			context.Background(),
			3*time.Minute,
		)
		defer invokeCancel()

		result, err := s.providerDaemon.invoke(
			invokeCtx,
			tenant,
			providerbroker.CodexInvokeRequest{
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
			"provider":         result.Provider,
			"model":            result.Model,
			"content":          result.Content,
			"done":             result.Done,
			"doneReason":       result.DoneReason,
			"totalDuration":    result.TotalDuration,
			"loadDuration":     result.LoadDuration,
			"promptTokens":     result.PromptTokens,
			"completionTokens": result.CompletionTokens,
		}
		return s.toolStruct(id, result.Content, structured)

	default:
		return s.toolErr(
			id,
			fmt.Sprintf(
				"provider %s invocation is not implemented",
				provider.ID,
			),
		)
	}
}
