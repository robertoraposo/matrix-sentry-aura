// Command sentry-recall is a Claude Code SessionStart hook that fights agent
// amnesia: at session start it asks Matrix Sentry to recall the memories most
// relevant to the current project and injects them into the session as context.
// It is the active counterpart to sentry-record (which passively logs accesses).
//
// It MUST run synchronously (async:false) for its stdout to be injected. It is
// best-effort: any failure ends in a clean exit 0 with no output, so a slow or
// down memory server never blocks the session from starting.
//
//	go build -o sentry-recall ./cmd/sentry-recall
//	# register as a SessionStart hook (async:false) in ~/.claude/settings.json
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
	Cwd    string `json:"cwd"`
	Source string `json:"source"`
}

func parseHook(b []byte) (hookInput, error) {
	var h hookInput
	err := json.Unmarshal(b, &h)
	return h, err
}

// queryFromCwd uses the project directory name as the recall query — the best
// signal available at session start about what this session is about.
func queryFromCwd(cwd string) string {
	cwd = strings.TrimRight(cwd, "/")
	if cwd == "" {
		return ""
	}
	return filepath.Base(cwd)
}

// maxQueryLen caps the recall query so a long README can't blow past the
// embedder's input or dominate the request.
const maxQueryLen = 500

// buildQuery composes the recall query from the richest locally-available
// signal: the git remote slug (identity) plus the README intro (semantics that
// embed near the project's stored memories), with graceful fallbacks. It always
// returns a usable string — the cwd basename is the last resort.
func buildQuery(cwd string) string {
	slug := gitSlug(cwd)
	intro := readmeIntro(cwd)
	var q string
	switch {
	case slug != "" && intro != "":
		q = slug + ": " + intro
	case slug != "":
		q = slug
	case intro != "":
		q = intro
	default:
		q = queryFromCwd(cwd)
	}
	if len(q) > maxQueryLen {
		q = q[:maxQueryLen]
	}
	return q
}

// gitSlug reads cwd/.git/config and returns the origin remote's "org/repo"
// slug, or "" if there is no config, no origin url, or it can't be parsed. It
// parses the file directly (no git binary) so the hook stays dependency-free.
func gitSlug(cwd string) string {
	data, err := os.ReadFile(filepath.Join(cwd, ".git", "config"))
	if err != nil {
		return ""
	}
	inOrigin := false
	var url string
	for _, ln := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			inOrigin = t == `[remote "origin"]`
			continue
		}
		if inOrigin {
			if k, v, ok := strings.Cut(t, "="); ok && strings.TrimSpace(k) == "url" {
				url = strings.TrimSpace(v)
				break
			}
		}
	}
	return slugFromURL(url)
}

// slugFromURL extracts "org/repo" from an https or ssh git remote url.
func slugFromURL(url string) string {
	if url == "" {
		return ""
	}
	url = strings.TrimSuffix(url, ".git")
	if strings.HasPrefix(url, "git@") { // ssh: git@host:org/repo
		if i := strings.LastIndex(url, ":"); i >= 0 {
			url = url[i+1:]
		}
	} else { // https://host/org/repo (tolerate a port on host)
		if i := strings.Index(url, "://"); i >= 0 {
			url = url[i+3:]
		}
		if i := strings.Index(url, "/"); i >= 0 {
			url = url[i+1:] // drop host
		}
	}
	parts := strings.Split(strings.Trim(url, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1]
}

// readmeIntro returns the first heading plus the first prose paragraph of a
// README in cwd, cleaned to a single line, or "" if none is found.
func readmeIntro(cwd string) string {
	entries, err := os.ReadDir(cwd)
	if err != nil {
		return ""
	}
	var path string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(strings.ToUpper(e.Name()), "README") {
			path = filepath.Join(cwd, e.Name())
			break
		}
	}
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, 1024))

	clean := func(s string) string {
		return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(s), "#> "))
	}
	lines := strings.Split(string(b), "\n")
	i := 0
	for i < len(lines) && clean(lines[i]) == "" { // skip leading blanks
		i++
	}
	var parts []string
	if i < len(lines) { // heading
		parts = append(parts, clean(lines[i]))
		i++
	}
	for i < len(lines) && clean(lines[i]) == "" { // skip blanks before paragraph
		i++
	}
	for i < len(lines) && clean(lines[i]) != "" { // first paragraph
		parts = append(parts, clean(lines[i]))
		i++
	}
	out := strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
	if len(out) > maxQueryLen {
		out = out[:maxQueryLen]
	}
	return out
}

// shouldInject injects on a fresh start or resume, but not on compact/clear
// where the session already carries context.
func shouldInject(source string) bool {
	switch source {
	case "compact", "clear":
		return false
	default:
		return true
	}
}

func buildRecallBody(query string, k int) []byte {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "recall",
			"arguments": map[string]any{"query": query, "k": k},
		},
	}
	b, _ := json.Marshal(msg)
	return b
}

// parseRecallText pulls the human-readable recall text out of the JSON-RPC
// tool response, or "" on any malformed/empty result.
func parseRecallText(b []byte) string {
	var r struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return ""
	}
	if len(r.Result.Content) == 0 {
		return ""
	}
	return r.Result.Content[0].Text
}

const maxContext = 10000

// formatOutput wraps the recall text as a SessionStart additionalContext payload,
// or returns "" when there is nothing worth injecting (empty or a "no memories"
// result). The context is truncated to the 10k-char hook limit.
func formatOutput(recallText string) string {
	t := strings.TrimSpace(recallText)
	if t == "" || strings.HasPrefix(strings.ToLower(t), "no ") {
		return ""
	}
	ctx := "## Matrix Sentry — relevant memories for this project\n" +
		"(retrieved automatically; use what's relevant, ignore the rest)\n\n" + t
	if len(ctx) > maxContext {
		ctx = ctx[:maxContext]
	}
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "SessionStart",
			"additionalContext": ctx,
		},
	}
	b, _ := json.Marshal(out)
	return string(b)
}

type config struct{ url, token string }

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
	// Best-effort: any problem → clean exit 0, no output, session unaffected.
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return
	}
	h, err := parseHook(raw)
	if err != nil || !shouldInject(h.Source) {
		return
	}
	cwd := h.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	query := buildQuery(cwd)
	if query == "" {
		return
	}
	cfg := loadConfig()
	if cfg.url == "" {
		return // no endpoint configured: silent no-op
	}
	text := recall(cfg, query)
	if out := formatOutput(text); out != "" {
		os.Stdout.WriteString(out)
	}
}

func recall(cfg config, query string) string {
	req, err := http.NewRequest("POST", cfg.url, bytes.NewReader(buildRecallBody(query, 5)))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	// Synchronous hook: bound the wait so a slow embed never stalls startup.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	return parseRecallText(body)
}
