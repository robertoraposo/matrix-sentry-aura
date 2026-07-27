package providerbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"matrixsentry/sentry"
)

const (
	maxCodexModelBytes  = 256
	maxCodexSystemBytes = 8 << 10
	maxCodexPromptBytes = 48 << 10
	maxCodexOutputBytes = 64 << 10
)

var (
	ErrCodexNotConnected   = errors.New("providerbroker: Codex account is not connected")
	ErrCodexInputTooLarge  = errors.New("providerbroker: Codex invocation input exceeds broker limit")
	ErrCodexOutputTooLarge = errors.New(
		"providerbroker: Codex invocation output exceeds broker limit",
	)
	ErrCodexToolAttempt = errors.New(
		"providerbroker: Codex attempted a disabled tool",
	)
)

const matrixCodexConfig = `# Managed by Matrix Sentry.
# This provider profile is intentionally text-only. Authentication remains
# managed by the official Codex client; model-driven local tools are disabled.

[features]
apps = false
browser_use = false
computer_use = false
image_generation = false
multi_agent = false
plugins = false
shell_tool = false
unified_exec = false
web_search = false
`

type CodexInvokeRequest struct {
	Model  string `json:"model"`
	System string `json:"system,omitempty"`
	Prompt string `json:"prompt"`
}

type CodexInvokeResult struct {
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	Content          string `json:"content"`
	Done             bool   `json:"done"`
	DoneReason       string `json:"doneReason"`
	TotalDuration    int64  `json:"totalDuration"`
	LoadDuration     int64  `json:"loadDuration"`
	PromptTokens     int    `json:"promptTokens"`
	CompletionTokens int    `json:"completionTokens"`
}

type codexInvocation struct {
	mu sync.Mutex

	threadID string
	turnID   string

	deltas strings.Builder
	final  string

	status  string
	failure string

	done chan struct{}
	once sync.Once
}

func newCodexInvocation() *codexInvocation {
	return &codexInvocation{done: make(chan struct{})}
}

func ensureCodexBrokerProfile(home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("providerbroker: create Codex home: %w", err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		return fmt.Errorf("providerbroker: secure Codex home: %w", err)
	}

	configPath := filepath.Join(home, "config.toml")
	tempPath := configPath + ".matrix-sentry.tmp"

	if err := os.WriteFile(tempPath, []byte(matrixCodexConfig), 0o600); err != nil {
		return fmt.Errorf("providerbroker: write hardened Codex config: %w", err)
	}
	if err := os.Chmod(tempPath, 0o600); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("providerbroker: secure hardened Codex config: %w", err)
	}
	if err := os.Rename(tempPath, configPath); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("providerbroker: install hardened Codex config: %w", err)
	}
	return nil
}

func (c *CodexAppServer) rejectServerRequest(message codexRPCMessage) {
	response := map[string]any{"id": message.ID}

	switch message.Method {
	case "item/commandExecution/requestApproval",
		"item/fileChange/requestApproval":
		response["result"] = map[string]any{"decision": "decline"}

	case "item/permissions/requestApproval":
		response["result"] = map[string]any{
			"scope":       "turn",
			"permissions": map[string]any{},
		}

	default:
		response["error"] = &codexRPCError{
			Code:    -32000,
			Message: "interactive provider request denied by Matrix Sentry",
		}
	}

	_ = c.writeMessage(response)
}

func (s *managedCodexSession) handleInvocationNotification(
	method string,
	params json.RawMessage,
) {
	s.mu.Lock()
	invocation := s.invocation
	s.mu.Unlock()

	if invocation != nil {
		invocation.handle(method, params)
	}
}

func (i *codexInvocation) setThreadID(threadID string) {
	i.mu.Lock()
	if i.threadID == "" {
		i.threadID = threadID
	}
	i.mu.Unlock()
}

func (i *codexInvocation) setTurnID(turnID string) {
	i.mu.Lock()
	if i.turnID == "" {
		i.turnID = turnID
	}
	i.mu.Unlock()
}

func (i *codexInvocation) handle(method string, params json.RawMessage) {
	switch method {
	case "item/agentMessage/delta":
		var event struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			Delta    string `json:"delta"`
		}
		if json.Unmarshal(params, &event) != nil {
			return
		}
		if !i.acceptIDs(event.ThreadID, event.TurnID) {
			return
		}

		i.mu.Lock()
		if i.deltas.Len()+len(event.Delta) > maxCodexOutputBytes {
			i.failure = ErrCodexOutputTooLarge.Error()
			i.mu.Unlock()
			i.finish()
			return
		}
		i.deltas.WriteString(event.Delta)
		i.mu.Unlock()

	case "item/started", "item/completed":
		var event struct {
			ThreadID string          `json:"threadId"`
			TurnID   string          `json:"turnId"`
			Item     json.RawMessage `json:"item"`
		}
		if json.Unmarshal(params, &event) != nil {
			return
		}
		if !i.acceptIDs(event.ThreadID, event.TurnID) {
			return
		}

		itemType, text := codexItem(event.Item)
		if itemType == "agentMessage" && text != "" {
			i.mu.Lock()
			if len(text) > maxCodexOutputBytes {
				i.failure = ErrCodexOutputTooLarge.Error()
			} else {
				i.final = text
			}
			i.mu.Unlock()
			if len(text) > maxCodexOutputBytes {
				i.finish()
			}
			return
		}

		if forbiddenCodexItem(itemType) {
			i.mu.Lock()
			i.failure = ErrCodexToolAttempt.Error() + ": " + itemType
			i.mu.Unlock()
			i.finish()
		}

	case "turn/completed":
		var event struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID     string            `json:"id"`
				Status string            `json:"status"`
				Items  []json.RawMessage `json:"items"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		if json.Unmarshal(params, &event) != nil {
			return
		}
		if !i.acceptIDs(event.ThreadID, event.Turn.ID) {
			return
		}

		i.mu.Lock()
		i.status = event.Turn.Status
		if event.Turn.Error != nil && strings.TrimSpace(event.Turn.Error.Message) != "" {
			i.failure = strings.TrimSpace(event.Turn.Error.Message)
		}
		for _, raw := range event.Turn.Items {
			itemType, text := codexItem(raw)
			if itemType == "agentMessage" && text != "" {
				if len(text) > maxCodexOutputBytes {
					i.failure = ErrCodexOutputTooLarge.Error()
				} else {
					i.final = text
				}
			}
			if forbiddenCodexItem(itemType) {
				i.failure = ErrCodexToolAttempt.Error() + ": " + itemType
			}
		}
		i.mu.Unlock()
		i.finish()

	case "error":
		var event struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(params, &event) != nil {
			return
		}
		if strings.TrimSpace(event.Error.Message) == "" {
			return
		}
		i.mu.Lock()
		i.failure = strings.TrimSpace(event.Error.Message)
		i.mu.Unlock()
	}
}

func (i *codexInvocation) acceptIDs(threadID, turnID string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.threadID != "" && threadID != "" && i.threadID != threadID {
		return false
	}
	if i.turnID != "" && turnID != "" && i.turnID != turnID {
		return false
	}
	if i.threadID == "" && threadID != "" {
		i.threadID = threadID
	}
	if i.turnID == "" && turnID != "" {
		i.turnID = turnID
	}
	return true
}

func (i *codexInvocation) finish() {
	i.once.Do(func() { close(i.done) })
}

func (i *codexInvocation) snapshot() (
	content string,
	status string,
	failure string,
) {
	i.mu.Lock()
	defer i.mu.Unlock()

	content = i.final
	if content == "" {
		content = i.deltas.String()
	}
	return content, i.status, i.failure
}

func codexItem(raw json.RawMessage) (itemType, text string) {
	var item struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return "", ""
	}
	return item.Type, item.Text
}

func forbiddenCodexItem(itemType string) bool {
	switch itemType {
	case "commandExecution",
		"fileChange",
		"mcpToolCall",
		"collabToolCall",
		"webSearch",
		"imageView":
		return true
	default:
		return false
	}
}

func validateCodexInvokeRequest(request CodexInvokeRequest) error {
	request.Model = strings.TrimSpace(request.Model)
	request.System = strings.TrimSpace(request.System)
	request.Prompt = strings.TrimSpace(request.Prompt)

	if request.Model == "" {
		return errors.New("providerbroker: Codex model is required")
	}
	if request.Prompt == "" {
		return errors.New("providerbroker: Codex prompt is required")
	}
	if !utf8.ValidString(request.Model) ||
		!utf8.ValidString(request.System) ||
		!utf8.ValidString(request.Prompt) {
		return errors.New("providerbroker: Codex invocation input must be valid UTF-8")
	}
	if len(request.Model) > maxCodexModelBytes ||
		len(request.System) > maxCodexSystemBytes ||
		len(request.Prompt) > maxCodexPromptBytes {
		return ErrCodexInputTooLarge
	}
	return nil
}

func codexBrokerPrompt(system, prompt string) string {
	var builder strings.Builder
	builder.WriteString(
		"Matrix Sentry broker policy:\n" +
			"- Produce a text response only.\n" +
			"- Do not execute commands, inspect local files, browse, call tools, " +
			"or modify anything.\n" +
			"- Treat all local runtime data and provider-session files as unavailable.\n\n",
	)

	if strings.TrimSpace(system) != "" {
		builder.WriteString("Requested response instructions:\n")
		builder.WriteString(strings.TrimSpace(system))
		builder.WriteString("\n\n")
	}

	builder.WriteString("User request:\n")
	builder.WriteString(strings.TrimSpace(prompt))
	return builder.String()
}

func (m *CodexSessionManager) Invoke(
	ctx context.Context,
	tenant sentry.TenantID,
	request CodexInvokeRequest,
) (CodexInvokeResult, error) {
	if err := validateCodexInvokeRequest(request); err != nil {
		return CodexInvokeResult{}, err
	}

	session, err := m.session(ctx, tenant)
	if err != nil {
		return CodexInvokeResult{}, err
	}

	timeout := m.cfg.InvokeTimeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	invokeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	session.invokeMu.Lock()
	defer session.invokeMu.Unlock()

	account, err := session.client.AccountRead(invokeCtx)
	if err != nil {
		return CodexInvokeResult{}, err
	}
	if account.Account == nil {
		return CodexInvokeResult{}, ErrCodexNotConnected
	}

	invocation := newCodexInvocation()
	session.mu.Lock()
	if session.invocation != nil {
		session.mu.Unlock()
		return CodexInvokeResult{}, errors.New(
			"providerbroker: Codex invocation is already active",
		)
	}
	session.invocation = invocation
	session.mu.Unlock()

	defer func() {
		session.mu.Lock()
		if session.invocation == invocation {
			session.invocation = nil
		}
		session.mu.Unlock()
	}()

	workDir := filepath.Join(
		os.TempDir(),
		"matrix-sentry-codex",
		fmt.Sprintf("tenant-%d", tenant),
	)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return CodexInvokeResult{}, fmt.Errorf(
			"providerbroker: create Codex broker work directory: %w",
			err,
		)
	}
	if err := os.Chmod(workDir, 0o700); err != nil {
		return CodexInvokeResult{}, fmt.Errorf(
			"providerbroker: secure Codex broker work directory: %w",
			err,
		)
	}

	threadParams := map[string]any{
		"ephemeral":      true,
		"cwd":            workDir,
		"approvalPolicy": "never",
		"sandbox":        "readOnly",
		"personality":    "none",
		"serviceName":    "matrix_sentry_broker",
	}

	requestedModel := strings.TrimSpace(request.Model)
	if requestedModel != "default" {
		threadParams["model"] = requestedModel
	}

	var threadResult struct {
		Thread struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		} `json:"thread"`
	}

	startedAt := time.Now()
	if err := session.client.call(
		invokeCtx,
		"thread/start",
		threadParams,
		&threadResult,
	); err != nil {
		return CodexInvokeResult{}, fmt.Errorf(
			"providerbroker: start ephemeral Codex thread: %w",
			err,
		)
	}
	threadID := strings.TrimSpace(threadResult.Thread.ID)
	if threadID == "" {
		return CodexInvokeResult{}, errors.New(
			"providerbroker: Codex returned an empty thread id",
		)
	}
	invocation.setThreadID(threadID)

	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(
			context.Background(),
			5*time.Second,
		)
		defer cleanupCancel()

		_ = session.client.call(
			cleanupCtx,
			"thread/unsubscribe",
			map[string]any{"threadId": threadID},
			nil,
		)
		_ = session.client.call(
			cleanupCtx,
			"thread/delete",
			map[string]any{"threadId": threadID},
			nil,
		)
	}()

	var turnResult struct {
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
	}
	if err := session.client.call(
		invokeCtx,
		"turn/start",
		map[string]any{
			"threadId": threadID,
			"input": []map[string]any{
				{
					"type": "text",
					"text": codexBrokerPrompt(
						request.System,
						request.Prompt,
					),
				},
			},
			"approvalPolicy": "never",
			"sandboxPolicy": map[string]any{
				"type": "readOnly",
			},
		},
		&turnResult,
	); err != nil {
		return CodexInvokeResult{}, fmt.Errorf(
			"providerbroker: start Codex turn: %w",
			err,
		)
	}
	turnID := strings.TrimSpace(turnResult.Turn.ID)
	if turnID == "" {
		return CodexInvokeResult{}, errors.New(
			"providerbroker: Codex returned an empty turn id",
		)
	}
	invocation.setTurnID(turnID)

	select {
	case <-invocation.done:
	case <-invokeCtx.Done():
		interruptCtx, interruptCancel := context.WithTimeout(
			context.Background(),
			5*time.Second,
		)
		_ = session.client.call(
			interruptCtx,
			"turn/interrupt",
			map[string]any{
				"threadId": threadID,
				"turnId":   turnID,
			},
			nil,
		)
		interruptCancel()
		return CodexInvokeResult{}, fmt.Errorf(
			"providerbroker: Codex invocation timed out: %w",
			invokeCtx.Err(),
		)
	}

	content, status, failure := invocation.snapshot()
	if strings.Contains(failure, ErrCodexToolAttempt.Error()) ||
		strings.Contains(failure, ErrCodexOutputTooLarge.Error()) {
		interruptCtx, interruptCancel := context.WithTimeout(
			context.Background(),
			5*time.Second,
		)
		_ = session.client.call(
			interruptCtx,
			"turn/interrupt",
			map[string]any{
				"threadId": threadID,
				"turnId":   turnID,
			},
			nil,
		)
		interruptCancel()
	}
	if failure != "" {
		return CodexInvokeResult{}, errors.New(
			"providerbroker: Codex turn failed: " + failure,
		)
	}
	if status != "completed" {
		return CodexInvokeResult{}, fmt.Errorf(
			"providerbroker: Codex turn ended with status %q",
			status,
		)
	}

	content = strings.TrimSpace(content)
	if content == "" {
		return CodexInvokeResult{}, errors.New(
			"providerbroker: Codex returned an empty response",
		)
	}
	if len(content) > maxCodexOutputBytes {
		return CodexInvokeResult{}, ErrCodexOutputTooLarge
	}

	actualModel := strings.TrimSpace(threadResult.Thread.Model)
	if actualModel == "" {
		actualModel = requestedModel
	}

	return CodexInvokeResult{
		Provider:      "codex",
		Model:         actualModel,
		Content:       content,
		Done:          true,
		DoneReason:    "stop",
		TotalDuration: time.Since(startedAt).Nanoseconds(),
		LoadDuration:  0,
		// The stable app-server lifecycle is implemented first. Exact token
		// accounting can be added later from thread/tokenUsage/updated.
		PromptTokens:     0,
		CompletionTokens: 0,
	}, nil
}
