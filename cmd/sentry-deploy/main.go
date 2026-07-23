// Command sentry-deploy is a Claude Code (or Codex) PostToolUse hook that turns
// a deploy/merge command into durable evidence on the Matrix Sentry comms
// channel. It reads the hook payload on stdin and, when the tool was a Bash
// command matching a deploy/merge pattern, POSTs a single JSON-RPC `post` call
// to the sentrymcp server so other agents watching the area see the deploy.
//
// Like the other hook binaries it is best-effort and unkillable from the agent's
// point of view: any failure ends in a clean exit 0, it never blocks the tool
// use, and it is a silent no-op unless SENTRY_MCP_URL is configured (env, or
// ~/.matrix-sentry.env).
//
//	go build -o sentry-deploy ./cmd/sentry-deploy
//	# then register as a PostToolUse hook (matcher "Bash", async:true)
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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

func str(v any) string {
	s, _ := v.(string)
	return s
}

// deployPatterns flag a command as deploy/merge evidence worth posting. Matching
// is substring (unanchored) so a leading `sudo ` or `origin ` never hides a real
// deploy; false positives are acceptable for a best-effort evidence trail.
var deployPatterns = []*regexp.Regexp{
	regexp.MustCompile(`systemctl\s+(restart|reload)\s+sentrymcp`),
	regexp.MustCompile(`gh\s+pr\s+merge`),
	regexp.MustCompile(`git\s+merge`),
	regexp.MustCompile(`scp\s+.*sentrymcp`),
	regexp.MustCompile(`git\s+push\s+.*(main|deploy)`),
}

// isDeployCommand reports whether cmd looks like a deploy or merge worth
// recording. Pure: no I/O, so it is unit-testable without a server.
func isDeployCommand(cmd string) bool {
	for _, re := range deployPatterns {
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}

// buildPostBody frames a stateless JSON-RPC tools/call for the comms `post`
// tool. The sentrymcp server dispatches tools/call without a prior initialize
// handshake, so one POST is enough. kind=info marks it as shared evidence.
func buildPostBody(area, from, text string) []byte {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "post",
			"arguments": map[string]any{
				"area": area,
				"from": from,
				"text": text,
				"kind": "info",
			},
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

// commsArea picks the channel to post into: SENTRY_COMMS_AREA if set, else the
// basename of the working dir, matching the identity model of the sibling hooks.
func commsArea(cwd string) string {
	if a := os.Getenv("SENTRY_COMMS_AREA"); a != "" {
		return a
	}
	if cwd != "" {
		return filepath.Base(cwd)
	}
	return "deploy"
}

// commsAgent picks the `from` label: SENTRY_COMMS_AGENT if set, else the host.
func commsAgent(cwd string) string {
	if a := os.Getenv("SENTRY_COMMS_AGENT"); a != "" {
		return a
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "agent"
}

// firstLine returns the trimmed first line of a (possibly multi-line) command.
func firstLine(cmd string) string {
	if i := strings.IndexByte(cmd, '\n'); i >= 0 {
		cmd = cmd[:i]
	}
	return strings.TrimSpace(cmd)
}

// run holds the pure-ish orchestration so it is testable with a stub transport.
// It acts only on a Bash deploy command and no-ops (no request) when the URL is
// unset. Every path returns cleanly — main() then exits 0.
func run(raw []byte, cfg config, client *http.Client) {
	h, err := parseHook(raw)
	if err != nil {
		return
	}
	if h.ToolName != "Bash" {
		return
	}
	cmd := str(h.ToolInput["command"])
	if !isDeployCommand(cmd) {
		return
	}
	if cfg.url == "" {
		return // no endpoint configured: silent no-op
	}
	cwd := h.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	text := "deploy: " + firstLine(cmd) + " @ " + cwd
	post(client, cfg, buildPostBody(commsArea(cwd), commsAgent(cwd), text))
}

func post(client *http.Client, cfg config, body []byte) {
	req, err := http.NewRequest("POST", cfg.url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return // fire-and-forget: never block or crash the agent
	}
	resp.Body.Close()
}

func main() {
	// Best-effort by design: any failure ends in a clean exit 0 so the agent's
	// tool use is never blocked or broken.
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 8<<20))
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 700 * time.Millisecond}
	run(raw, loadConfig(), client)
}
