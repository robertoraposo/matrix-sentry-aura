// Command sentry-record is a Claude Code PostToolUse hook that turns natural
// agent work into a real access stream for Matrix Sentry. It reads the hook
// payload on stdin, extracts the existing files the tool touched, and POSTs them
// to the sentrymcp server's record_access tool (one batched JSON-RPC call).
//
// It is deliberately unkillable from the agent's point of view: it never blocks
// for long, never errors out the tool use, and is a silent no-op unless
// SENTRY_MCP_URL is configured (env, or ~/.matrix-sentry.env).
//
//	go build -o sentry-record ./cmd/sentry-record
//	# then register as a PostToolUse hook in .claude/settings.json
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type hookInput struct {
	ToolName     string         `json:"tool_name"`
	ToolInput    map[string]any `json:"tool_input"`
	ToolResponse any            `json:"tool_response"`
	Cwd          string         `json:"cwd"`
}

func parseHook(b []byte) (hookInput, error) {
	var h hookInput
	err := json.Unmarshal(b, &h)
	return h, err
}

// buildCallBody frames a stateless JSON-RPC tools/call for record_access. The
// sentrymcp server dispatches tools/call without a prior initialize handshake,
// so one POST is enough.
func buildCallBody(paths []string, src string) []byte {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "record_access",
			"arguments": map[string]any{"paths": paths, "src": src},
		},
	}
	b, _ := json.Marshal(msg)
	return b
}

type config struct{ url, token string }

// loadConfig reads SENTRY_MCP_URL/SENTRY_MCP_TOKEN from the environment, falling
// back to ~/.matrix-sentry.env so secrets stay out of git-tracked settings.json.
func loadConfig() config {
	c := config{url: os.Getenv("SENTRY_MCP_URL"), token: os.Getenv("SENTRY_MCP_TOKEN")}
	if c.url == "" {
		if home, err := os.UserHomeDir(); err == nil {
			loadEnvFile(filepath.Join(home, ".matrix-sentry.env"), &c)
		}
	}
	return c
}

func loadEnvFile(path string, c *config) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch k {
		case "SENTRY_MCP_URL":
			if c.url == "" {
				c.url = v
			}
		case "SENTRY_MCP_TOKEN":
			if c.token == "" {
				c.token = v
			}
		}
	}
}

func main() {
	// Best-effort by design: any failure ends in a clean exit 0 so the agent's
	// tool use is never blocked or broken.
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 8<<20))
	if err != nil {
		return
	}
	h, err := parseHook(raw)
	if err != nil {
		return
	}
	cwd := h.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	paths := extractPaths(h.ToolName, h.ToolInput, h.ToolResponse, cwd)
	if len(paths) == 0 {
		return
	}
	cfg := loadConfig()
	if cfg.url == "" {
		return // no endpoint configured: silent no-op
	}
	post(cfg, buildCallBody(paths, h.ToolName))
}

func post(cfg config, body []byte) {
	req, err := http.NewRequest("POST", cfg.url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	client := &http.Client{Timeout: 700 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return // fire-and-forget: never block or crash the agent
	}
	resp.Body.Close()
}
