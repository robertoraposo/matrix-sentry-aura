package providerbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxOllamaResponseBytes = 4 << 20

type ChatRequest struct {
	Model  string
	System string
	Prompt string
}

type ChatResult struct {
	Model              string
	Content            string
	Done               bool
	DoneReason         string
	TotalDuration      int64
	LoadDuration       int64
	PromptEvalCount    int
	PromptEvalDuration int64
	EvalCount          int
	EvalDuration       int64
}

// InvokeOllama sends one non-streaming chat request through Ollama's HTTP API.
// It returns only the generated content and operational metrics.
func InvokeOllama(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	input ChatRequest,
) (ChatResult, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	input.Model = strings.TrimSpace(input.Model)
	input.System = strings.TrimSpace(input.System)
	input.Prompt = strings.TrimSpace(input.Prompt)

	if baseURL == "" {
		return ChatResult{}, fmt.Errorf("providerbroker: Ollama URL is required")
	}
	if input.Model == "" {
		return ChatResult{}, fmt.Errorf("providerbroker: model is required")
	}
	if input.Prompt == "" {
		return ChatResult{}, fmt.Errorf("providerbroker: prompt is required")
	}

	if client == nil {
		client = http.DefaultClient
	}

	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}

	messages := make([]message, 0, 2)
	if input.System != "" {
		messages = append(messages, message{
			Role:    "system",
			Content: input.System,
		})
	}
	messages = append(messages, message{
		Role:    "user",
		Content: input.Prompt,
	})

	payload := struct {
		Model    string    `json:"model"`
		Messages []message `json:"messages"`
		Stream   bool      `json:"stream"`
	}{
		Model:    input.Model,
		Messages: messages,
		Stream:   false,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return ChatResult{}, fmt.Errorf(
			"providerbroker: encode Ollama request: %w",
			err,
		)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		baseURL+"/api/chat",
		bytes.NewReader(body),
	)
	if err != nil {
		return ChatResult{}, fmt.Errorf(
			"providerbroker: create Ollama request: %w",
			err,
		)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return ChatResult{}, fmt.Errorf(
			"providerbroker: Ollama request failed: %w",
			err,
		)
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, maxOllamaResponseBytes)

	if resp.StatusCode < http.StatusOK ||
		resp.StatusCode >= http.StatusMultipleChoices {
		detail, _ := io.ReadAll(limited)
		message := strings.TrimSpace(string(detail))
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}

		return ChatResult{}, fmt.Errorf(
			"providerbroker: Ollama returned %d: %s",
			resp.StatusCode,
			message,
		)
	}

	var upstream struct {
		Model   string `json:"model"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		Done               bool   `json:"done"`
		DoneReason         string `json:"done_reason"`
		TotalDuration      int64  `json:"total_duration"`
		LoadDuration       int64  `json:"load_duration"`
		PromptEvalCount    int    `json:"prompt_eval_count"`
		PromptEvalDuration int64  `json:"prompt_eval_duration"`
		EvalCount          int    `json:"eval_count"`
		EvalDuration       int64  `json:"eval_duration"`
	}

	if err := json.NewDecoder(limited).Decode(&upstream); err != nil {
		return ChatResult{}, fmt.Errorf(
			"providerbroker: decode Ollama response: %w",
			err,
		)
	}

	return ChatResult{
		Model:              upstream.Model,
		Content:            upstream.Message.Content,
		Done:               upstream.Done,
		DoneReason:         upstream.DoneReason,
		TotalDuration:      upstream.TotalDuration,
		LoadDuration:       upstream.LoadDuration,
		PromptEvalCount:    upstream.PromptEvalCount,
		PromptEvalDuration: upstream.PromptEvalDuration,
		EvalCount:          upstream.EvalCount,
		EvalDuration:       upstream.EvalDuration,
	}, nil
}
