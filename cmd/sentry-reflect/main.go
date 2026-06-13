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
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	return filepath.Base(filepath.Clean("/" + strings.ReplaceAll(sid, string(os.PathSeparator), "_")))
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
