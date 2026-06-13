// Command sentry-reflect is a Claude Code Stop hook that closes the last leg of
// Matrix Sentry's memory cycle: auto-remember. Every K new tool-uses in a
// session it returns {"decision":"block","reason":…}, making the already-running
// model pause and distill durable knowledge (decisions, conventions, gotchas)
// into the semantic store via the recall/remember tools. It reuses the model's
// own judgment — no extra classifier, no extra generative model.
//
// It is best-effort and unkillable from the agent's point of view: any problem
// ends in a clean exit 0 with no output (the stop proceeds normally), it is a
// no-op without SENTRY_MCP_URL, and stop_hook_active prevents reflection loops.
//
//	go build -o sentry-reflect ./cmd/sentry-reflect
//	# register as a Stop hook (async:false) in ~/.claude/settings.json
package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"matrixsentry/internal/transcript"
)

// reflectEvery is the activity threshold (tool-uses per reflection). Set from
// the Phase-0 yield measurement: the K whose window holds ~1 durable fact.
// Confirmed K=40 in results/auto-remember-yield.md.
const reflectEvery = 40

type hookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	StopHookActive bool   `json:"stop_hook_active"`
	Cwd            string `json:"cwd"`
}

func parseHook(b []byte) (hookInput, error) {
	var h hookInput
	err := json.Unmarshal(b, &h)
	return h, err
}

// shouldReflect fires when at least k new tool-uses have accrued AND we are not
// already inside a reflection pass we triggered (the loop guard).
func shouldReflect(delta, k int, stopActive bool) bool {
	return !stopActive && delta >= k
}

// stateDir is where per-session activity counters live.
func stateDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "matrix-sentry", "reflect")
}

// safeName reduces a session id to a single path-safe file name so a crafted id
// can never write outside the state dir.
func safeName(sid string) string {
	if sid == "" {
		return "default"
	}
	name := filepath.Base(filepath.Clean("/" + strings.ReplaceAll(sid, string(os.PathSeparator), "_")))
	if name == "/" || name == "." || name == "" {
		return "default"
	}
	return name
}

func readCount(dir, sid string) int {
	b, err := os.ReadFile(filepath.Join(dir, safeName(sid)))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return n
}

func writeCount(dir, sid string, n int) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, safeName(sid)), []byte(strconv.Itoa(n)), 0o644)
}

func blockOutput(reason string) string {
	b, _ := json.Marshal(map[string]any{"decision": "block", "reason": reason})
	return string(b)
}

// reflectionPrompt is the instruction injected as the Stop "reason". It mirrors
// the project's memory guidance: durable knowledge only, dedup first, be terse.
const reflectionPrompt = "Pause before finishing. Reflect on the work since your last memory checkpoint. " +
	"If — and only if — you learned durable knowledge (a decision made, a convention adopted, a gotcha " +
	"discovered) that a future session would benefit from, persist it: first call the recall tool to avoid " +
	"duplicating what is already stored, then call the remember tool once per genuinely-new fact, each fact " +
	"self-contained and concise. If recall surfaces a memory that is now outdated or wrong, call remember " +
	"with supersedes set to that memory's id to replace it instead of storing a contradicting duplicate. " +
	"Do NOT store transient state, file contents, task progress, or anything already in the code or git. " +
	"If nothing durable was learned, store nothing. Be terse — do not narrate this to the user. Then finish."

// decide computes the hook's action against an explicit transcript + state dir.
// Returns the stdout to emit (empty if none) and whether it fired. Pure w.r.t.
// the network; main() adds config gating around it.
func decide(h hookInput, stateDirPath string, k int) (string, bool) {
	f, err := os.Open(h.TranscriptPath)
	if err != nil {
		return "", false
	}
	defer f.Close()
	current, err := transcript.CountToolUses(f)
	if err != nil {
		return "", false
	}
	stored := readCount(stateDirPath, h.SessionID)
	if !shouldReflect(current-stored, k, h.StopHookActive) {
		return "", false
	}
	_ = writeCount(stateDirPath, h.SessionID, current)
	return blockOutput(reflectionPrompt), true
}

type config struct{ url string }

// loadConfig reads SENTRY_MCP_URL from the environment, falling back to
// ~/.matrix-sentry.env. This hook makes no network call — it only needs the URL
// to decide whether the memory server is configured (else it is a no-op).
func loadConfig() config {
	c := config{url: os.Getenv("SENTRY_MCP_URL")}
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
	for _, ln := range strings.Split(string(data), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		key, val, ok := strings.Cut(ln, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		switch key {
		case "SENTRY_MCP_URL":
			if c.url == "" {
				c.url = val
			}
		}
	}
}

func main() {
	// Best-effort: any problem -> clean exit 0, no output, the stop proceeds.
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return
	}
	h, err := parseHook(raw)
	if err != nil {
		return
	}
	// No memory server configured -> remember would fail; do not nudge.
	if loadConfig().url == "" {
		return
	}
	if out, fired := decide(h, stateDir(), reflectEvery); fired {
		os.Stdout.WriteString(out)
	}
}
