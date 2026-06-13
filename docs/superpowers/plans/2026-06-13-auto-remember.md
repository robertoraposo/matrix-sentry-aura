# Auto-Remember Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the last leg of Matrix Sentry's memory cycle — automatically detect durable knowledge during agent work and persist it via `remember`, without polluting the corpus with noise.

**Architecture:** A Claude Code **Stop hook** (`cmd/sentry-reflect`) fires on an *activity threshold* (every *K* new tool-uses) and returns `{"decision":"block","reason":…}` to make the already-running model distill what it learned and call `remember` (agent self-report — reuses the LLM's in-context judgment, no extra generative model). Noise is controlled in depth: the reflection prompt tells the agent to `recall` first, AND the server semantically deduplicates at `remember` time. *K* and the dedup threshold τ are not invented — they are calibrated by a measurement pass (`cmd/reflectyield`) over real transcripts FIRST.

**Tech Stack:** Pure Go, zero deps (project rule). Shared transcript-parsing in `internal/transcript`. Reuses the existing `memory` package (exact-L2 search) and the `sentrymcp` MCP server. Hooks mirror the established best-effort pattern of `cmd/sentry-record` / `cmd/sentry-recall`.

---

## File Structure

- **Create** `internal/transcript/transcript.go` — parse Claude Code `.jsonl` transcripts: count `tool_use` blocks (the shared activity signal) and segment a session into *K*-tool-use windows of prose. One responsibility: read transcripts. Imported by BOTH the measurement and the hook so their counting is identical.
- **Create** `internal/transcript/transcript_test.go` — tests for counting + windowing.
- **Create** `cmd/reflectyield/main.go` — Phase-0 measurement instrument: emit per-window JSONL from real transcripts for yield labeling. Throwaway-grade but committed (reproducible calibration).
- **Create** `cmd/sentry-reflect/main.go` — the Stop hook. Best-effort, mirrors `cmd/sentry-record`.
- **Create** `cmd/sentry-reflect/main_test.go` — tests for gating, state, block payload, hook parse.
- **Modify** `memory/memory.go` — add `DedupThreshold` to `Store`; `Remember` returns `(id, deduped, err)` with a novelty gate.
- **Modify** `memory/memory_test.go` — update existing `Remember` call sites to 3 returns; add dedup tests.
- **Modify** `cmd/sentrymcp/main.go` — call the new `Remember` signature, report `deduped`, wire a `-dedup-tau` flag.
- **Create** `results/auto-remember-yield.md` — the Phase-0 finding (yield numbers, chosen *K*, τ seed).
- **Modify** `~/.claude/settings.json` — register the Stop hook (deployment, Task 8).

---

## Task 1: Shared transcript parsing (`internal/transcript`)

**Files:**
- Create: `internal/transcript/transcript.go`
- Test: `internal/transcript/transcript_test.go`

- [ ] **Step 1: Write the failing test**

```go
package transcript

import (
	"strings"
	"testing"
)

// A minimal but faithful slice of a Claude Code transcript: an assistant turn
// with prose + two tool_use blocks, then a user turn carrying a tool_result
// (which must NOT count as activity and must NOT pollute window text).
const fixture = `{"type":"assistant","message":{"content":[{"type":"text","text":"We decided to dedup server-side."},{"type":"tool_use","name":"Edit"},{"type":"tool_use","name":"Bash"}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":"file contents here"}]}}
{"type":"user","message":{"content":"plain string user prompt"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Gotcha: never open the live journal."},{"type":"tool_use","name":"Read"}]}}
`

func TestCountToolUses(t *testing.T) {
	n, err := CountToolUses(strings.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("CountToolUses = %d, want 3", n)
	}
}

func TestWindowsSegmentByToolUseCount(t *testing.T) {
	w, err := Windows("sess1", strings.NewReader(fixture), 2)
	if err != nil {
		t.Fatal(err)
	}
	// 3 tool_uses, k=2 -> window0 closes at the 2nd tool_use, window1 holds the rest.
	if len(w) != 2 {
		t.Fatalf("got %d windows, want 2", len(w))
	}
	if w[0].ToolUses != 2 || w[1].ToolUses != 1 {
		t.Fatalf("tool_use counts = %d,%d want 2,1", w[0].ToolUses, w[1].ToolUses)
	}
	if !strings.Contains(w[0].Text, "decided") {
		t.Fatalf("window0 text missing assistant prose: %q", w[0].Text)
	}
	if !strings.Contains(w[1].Text, "Gotcha") {
		t.Fatalf("window1 text missing assistant prose: %q", w[1].Text)
	}
	// tool_result content must never leak into window text.
	if strings.Contains(w[0].Text, "file contents") || strings.Contains(w[1].Text, "file contents") {
		t.Fatalf("tool_result content leaked into window text")
	}
	if w[0].Session != "sess1" {
		t.Fatalf("session = %q, want sess1", w[0].Session)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/transcript/`
Expected: FAIL — `undefined: CountToolUses` / `undefined: Windows`.

- [ ] **Step 3: Write minimal implementation**

```go
// Package transcript parses Claude Code .jsonl session transcripts. It exposes
// the two things Matrix Sentry's auto-remember needs: the count of tool_use
// blocks (the activity signal that drives the reflect hook's threshold) and a
// segmentation of a session into windows of K tool-uses with their surrounding
// prose (the material the yield measurement labels). Both the measurement and
// the hook count tool-uses through CountToolUses so their notion of "activity"
// is identical — the calibrated K is only meaningful if they agree.
package transcript

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

type line struct {
	Type    string `json:"type"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"`
}

// parseContent decodes a message's content, which is either a JSON string (a
// plain text message) or an array of typed blocks. Unknown shapes yield nil.
func parseContent(raw json.RawMessage) []block {
	if len(raw) == 0 {
		return nil
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return blocks
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []block{{Type: "text", Text: s}}
	}
	return nil
}

func newScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20) // transcript lines can be large
	return sc
}

// CountToolUses returns the number of tool_use blocks in a transcript. Non-JSON
// or partial lines are tolerated (skipped), so a live, still-growing transcript
// never errors the caller.
func CountToolUses(r io.Reader) (int, error) {
	sc := newScanner(r)
	n := 0
	for sc.Scan() {
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue
		}
		for _, b := range parseContent(l.Message.Content) {
			if b.Type == "tool_use" {
				n++
			}
		}
	}
	return n, sc.Err()
}

// Window is a slice of a session: the prose around K tool-uses, the unit the
// yield measurement labels for "contains a durable fact?".
type Window struct {
	Session  string `json:"session"`
	Index    int    `json:"window"`
	ToolUses int    `json:"tool_uses"`
	Text     string `json:"text"`
}

// Windows segments a transcript into windows that close every k tool-uses,
// collecting the text blocks seen along the way. tool_result content is
// deliberately excluded (it is file/command output — noise for durability).
func Windows(session string, r io.Reader, k int) ([]Window, error) {
	if k <= 0 {
		k = 1
	}
	sc := newScanner(r)
	var out []Window
	var sb strings.Builder
	tu, idx := 0, 0
	flush := func() {
		out = append(out, Window{Session: session, Index: idx, ToolUses: tu, Text: sb.String()})
		idx++
		tu = 0
		sb.Reset()
	}
	for sc.Scan() {
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue
		}
		for _, b := range parseContent(l.Message.Content) {
			switch b.Type {
			case "text":
				if t := strings.TrimSpace(b.Text); t != "" {
					sb.WriteString(t)
					sb.WriteString("\n")
				}
			case "tool_use":
				tu++
				if tu >= k {
					flush()
				}
			}
		}
	}
	if tu > 0 || sb.Len() > 0 {
		flush()
	}
	return out, sc.Err()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/transcript/`
Expected: PASS (2 tests ok).

- [ ] **Step 5: Commit**

```bash
git add internal/transcript/
git commit -m "feat(transcript): shared Claude Code transcript parser (tool-use count + windows)"
```

---

## Task 2: Phase-0 yield measurement (`cmd/reflectyield`) + calibrate K, τ

This task DECIDES WITH DATA. It builds the instrument, runs it on real transcripts, the executing agent labels a random sample, and the findings set *K* (Task 4) and seed τ (Tasks 5/8). If the yield is noise, STOP and report — do not proceed to build blindly.

**Files:**
- Create: `cmd/reflectyield/main.go`
- Create: `results/auto-remember-yield.md`

- [ ] **Step 1: Write the instrument**

```go
// Command reflectyield is the Phase-0 measurement for auto-remember. It walks
// real Claude Code transcripts, segments each session into windows of K
// tool-uses, and emits one JSON object per window on stdout so a judge (the
// same LLM that will do self-report) can label "does this window contain a
// durable fact?". The point is to calibrate K (so each reflection trigger has
// ~1 expected durable fact) and to sanity-check that the captured facts are
// real knowledge, not noise — before any production wiring is trusted.
//
//	go run ./cmd/reflectyield -dir <transcripts> -k 40 > windows.jsonl
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"matrixsentry/internal/transcript"
)

func main() {
	dir := flag.String("dir", "", "directory of Claude Code .jsonl transcripts")
	k := flag.Int("k", 40, "tool-uses per window")
	minText := flag.Int("min-text", 200, "skip windows with fewer than N chars of prose")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "usage: reflectyield -dir <transcripts dir> [-k N] [-min-text N]")
		os.Exit(2)
	}
	paths, _ := filepath.Glob(filepath.Join(*dir, "*.jsonl"))
	enc := json.NewEncoder(os.Stdout)
	sessions, total, emitted := 0, 0, 0
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		session := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		windows, err := transcript.Windows(session, f, *k)
		f.Close()
		if err != nil {
			continue
		}
		sessions++
		for _, w := range windows {
			total++
			if len(w.Text) < *minText {
				continue
			}
			emitted++
			_ = enc.Encode(w)
		}
	}
	fmt.Fprintf(os.Stderr, "sessions=%d windows=%d emitted(text>=%d)=%d k=%d\n",
		sessions, total, *minText, emitted, *k)
}
```

- [ ] **Step 2: Build it**

Run: `go build ./cmd/reflectyield && echo OK`
Expected: `OK`.

- [ ] **Step 3: Run the sweep over real transcripts**

```bash
TDIR=~/.claude/projects/-Users-alvinnunez-Downloads-matrix-sentry
for K in 20 40 80; do
  go run ./cmd/reflectyield -dir "$TDIR" -k $K > /tmp/windows-k$K.jsonl 2>/tmp/yield-k$K.txt
  cat /tmp/yield-k$K.txt
done
```
Expected: three summary lines like `sessions=35 windows=… emitted=… k=20/40/80`.

- [ ] **Step 4: Label a random sample (the executing agent does this with judgment)**

For each K, sample up to 25 windows and judge each:

```bash
shuf /tmp/windows-k40.jsonl | head -25 > /tmp/sample-k40.jsonl
```

Read `/tmp/sample-k40.jsonl`. For each window, decide: does its `text` contain ≥1 **durable** fact — a decision made, a convention adopted, or a gotcha discovered — that a future session would benefit from? (NOT transient state, NOT file contents, NOT task progress.) If yes, write the fact you would `remember`. Tally `durable_windows / sampled_windows` per K.

- [ ] **Step 5: Record the finding and pick K + seed τ**

Write `results/auto-remember-yield.md` with: the summary lines, the per-K yield fraction, 5-10 example extracted facts (verbatim quality evidence), the **chosen K** (the K whose yield ≈ "about 1 durable fact per window" — neither mostly-empty nor multi-fact-crowded), and a **τ seed** note. Decision rule, stated explicitly in the file:
- If yield at the best K is **< ~10%**, STOP: self-report would mostly fire on empty windows — report back before building the hook.
- Else record the chosen K (used in Task 4) and proceed.

τ seed: real `nomic-embed-text` vectors are dim-768 and unnormalized, so absolute L2 differs from the unit-test geometry. Seed τ from data later (Task 8) by embedding a few of the example facts against the live store; for now record "τ to be set at deploy from real-embedding distances; unit tests pin behavior with a synthetic τ".

- [ ] **Step 6: Commit**

```bash
git add cmd/reflectyield/ results/auto-remember-yield.md
git commit -m "feat(reflectyield): Phase-0 yield measurement + calibration finding (K chosen from data)"
```

---

## Task 3: Reflect hook — gating, state, payload (pure functions, TDD)

**Files:**
- Create: `cmd/sentry-reflect/main.go` (helpers only this task; `main()` in Task 4)
- Test: `cmd/sentry-reflect/main_test.go`

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestShouldReflect(t *testing.T) {
	cases := []struct {
		delta, k   int
		stopActive bool
		want       bool
	}{
		{39, 40, false, false}, // below threshold
		{40, 40, false, true},  // at threshold, fresh stop -> fire
		{99, 40, false, true},  // well past threshold
		{40, 40, true, false},  // inside a reflection pass we triggered -> never re-fire (loop guard)
		{0, 40, false, false},  // no activity
	}
	for _, c := range cases {
		if got := shouldReflect(c.delta, c.k, c.stopActive); got != c.want {
			t.Errorf("shouldReflect(%d,%d,%v)=%v want %v", c.delta, c.k, c.stopActive, got, c.want)
		}
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if got := readCount(dir, "sessA"); got != 0 {
		t.Fatalf("missing state should read 0, got %d", got)
	}
	if err := writeCount(dir, "sessA", 42); err != nil {
		t.Fatal(err)
	}
	if got := readCount(dir, "sessA"); got != 42 {
		t.Fatalf("readCount after write = %d, want 42", got)
	}
	// state is per-session
	if got := readCount(dir, "sessB"); got != 0 {
		t.Fatalf("other session leaked state: %d", got)
	}
	// a session id with path separators must not escape the state dir
	if err := writeCount(dir, "../escape", 7); err != nil {
		t.Fatal(err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*")); len(matches) == 0 {
		t.Fatalf("sanitized state file not written inside dir")
	}
}

func TestBlockOutputIsValidDecision(t *testing.T) {
	out := blockOutput("please reflect")
	var m struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("blockOutput not valid JSON: %v", err)
	}
	if m.Decision != "block" {
		t.Fatalf("decision = %q, want block", m.Decision)
	}
	if m.Reason == "" {
		t.Fatalf("reason must be non-empty")
	}
}

func TestParseHook(t *testing.T) {
	h, err := parseHook([]byte(`{"session_id":"s1","transcript_path":"/t.jsonl","stop_hook_active":true,"cwd":"/w"}`))
	if err != nil {
		t.Fatal(err)
	}
	if h.SessionID != "s1" || h.TranscriptPath != "/t.jsonl" || !h.StopHookActive || h.Cwd != "/w" {
		t.Fatalf("parsed hook wrong: %+v", h)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/sentry-reflect/`
Expected: FAIL — `undefined: shouldReflect` etc.

- [ ] **Step 3: Write minimal implementation**

```go
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
// the Phase-0 yield measurement (Task 2): the K whose window holds ~1 durable
// fact. Confirm/adjust this constant from results/auto-remember-yield.md.
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/sentry-reflect/`
Expected: PASS (4 tests ok).

- [ ] **Step 5: Commit**

```bash
git add cmd/sentry-reflect/
git commit -m "feat(sentry-reflect): Stop-hook gating, per-session state, block payload"
```

---

## Task 4: Reflect hook — `main()` wiring + reflection prompt

**Files:**
- Modify: `cmd/sentry-reflect/main.go`
- Test: `cmd/sentry-reflect/main_test.go`

- [ ] **Step 1: Write the failing test (end-to-end gating through a temp transcript)**

Append to `cmd/sentry-reflect/main_test.go`:

```go
import (
	"os"
	"strings"
)

// run drives the hook's decision logic the way main() does, but against an
// explicit transcript + state dir so it is deterministic and offline.
func TestDecideFiresOncePerWindow(t *testing.T) {
	dir := t.TempDir()
	// transcript with 2 tool_use blocks
	tf := dir + "/t.jsonl"
	os.WriteFile(tf, []byte(
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read"},{"type":"tool_use","name":"Bash"}]}}`+"\n"), 0o644)

	// k=2, fresh session -> should fire and persist count=2
	out, fired := decide(hookInput{SessionID: "s1", TranscriptPath: tf}, dir, 2)
	if !fired {
		t.Fatalf("expected fire on delta=2 k=2")
	}
	if !strings.Contains(out, `"decision":"block"`) {
		t.Fatalf("fire output not a block decision: %q", out)
	}
	// immediately again, same transcript -> delta=0 -> no fire
	_, fired2 := decide(hookInput{SessionID: "s1", TranscriptPath: tf}, dir, 2)
	if fired2 {
		t.Fatalf("should not re-fire when no new activity")
	}
	// loop guard: stop_hook_active true never fires even past threshold
	os.WriteFile(dir+"/s2", []byte("0"), 0o644)
	_, fired3 := decide(hookInput{SessionID: "s2", TranscriptPath: tf, StopHookActive: true}, dir, 2)
	if fired3 {
		t.Fatalf("loop guard failed: fired with stop_hook_active")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/sentry-reflect/`
Expected: FAIL — `undefined: decide`.

- [ ] **Step 3: Add `decide`, `loadConfig`, the prompt, and `main()`**

Append to `cmd/sentry-reflect/main.go` and add `"io"` to the import block (the hook makes NO network call itself — the agent's `remember` tool does — so do not import `net/http`/`time`):

```go
// reflectionPrompt is the instruction injected as the Stop "reason". It mirrors
// the project's memory guidance: durable knowledge only, dedup first, be terse.
const reflectionPrompt = "Pause before finishing. Reflect on the work since your last memory checkpoint. " +
	"If — and only if — you learned durable knowledge (a decision made, a convention adopted, a gotcha " +
	"discovered) that a future session would benefit from, persist it: first call the recall tool to avoid " +
	"duplicating what is already stored, then call the remember tool once per genuinely-new fact, each fact " +
	"self-contained and concise. Do NOT store transient state, file contents, task progress, or anything " +
	"already in the code or git. If nothing durable was learned, store nothing. Be terse — do not narrate " +
	"this to the user. Then finish."

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
		case "SENTRY_MCP_TOKEN":
			if c.token == "" {
				c.token = val
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
```

After this task the full import block of `cmd/sentry-reflect/main.go` is exactly:
`"encoding/json"`, `"io"`, `"os"`, `"path/filepath"`, `"strconv"`, `"strings"`, and `"matrixsentry/internal/transcript"`. No `net/http`, no `time`.

- [ ] **Step 4: Run tests + vet**

Run: `go test ./cmd/sentry-reflect/ && go vet ./cmd/sentry-reflect/`
Expected: PASS, no vet complaints.

- [ ] **Step 5: Commit**

```bash
git add cmd/sentry-reflect/
git commit -m "feat(sentry-reflect): main() + reflection prompt; fires once per K-window, config-gated"
```

---

## Task 5: Semantic dedup in `memory` package

**Files:**
- Modify: `memory/memory.go:103-120` (the `Remember` method + `Store` struct at `:60`)
- Test: `memory/memory_test.go`

- [ ] **Step 1: Write the failing test**

Append to `memory/memory_test.go` (the existing `geoEmbedder` has `cat={0,0}`, `kitten={0.1,0}` at squared-L2 0.01 from cat, `feline={0.5,0}` at 0.25 from cat):

```go
func TestRememberDedupsNearDuplicate(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	st.DedupThreshold = 0.05 // kitten(0.01) < tau < feline(0.25)

	id1, dup1, err := st.Remember(1, "cat", nil, "")
	if err != nil || dup1 {
		t.Fatalf("first store: id=%d dup=%v err=%v", id1, dup1, err)
	}
	// "kitten" is within tau of "cat" -> deduped, not persisted, returns id1.
	id2, dup2, err := st.Remember(1, "kitten", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !dup2 {
		t.Fatalf("near-duplicate not deduped")
	}
	if id2 != id1 {
		t.Fatalf("dedup returned id %d, want existing %d", id2, id1)
	}
	if n := st.Count(1); n != 1 {
		t.Fatalf("store grew to %d on a duplicate, want 1", n)
	}
	// "feline" is beyond tau -> genuinely new, persisted.
	_, dup3, err := st.Remember(1, "feline", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if dup3 {
		t.Fatalf("distinct fact wrongly deduped (tau false positive)")
	}
	if n := st.Count(1); n != 2 {
		t.Fatalf("distinct fact not stored: count=%d want 2", n)
	}
}

func TestRememberNoDedupWhenThresholdZero(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	// DedupThreshold defaults to 0 -> feature off, every call persists.
	st.Remember(1, "cat", nil, "")
	_, dup, _ := st.Remember(1, "cat", nil, "")
	if dup {
		t.Fatalf("dedup fired with threshold 0 (should be disabled)")
	}
	if n := st.Count(1); n != 2 {
		t.Fatalf("count=%d want 2 (no dedup)", n)
	}
}
```

- [ ] **Step 2: Update existing call sites so the package compiles**

The signature changes to three returns. Find and fix every caller:

```bash
grep -rn '\.Remember(' --include=*.go .
```

In `memory/memory_test.go`, every existing `st.Remember(...)` used as `(id, err)` or `(_, err)` becomes `(id, _, err)` / `(_, _, err)`. Example existing site:

```go
// before:
if _, err := st.Remember(1, text, nil, ""); err != nil {
// after:
if _, _, err := st.Remember(1, text, nil, ""); err != nil {
```

Apply to all such sites in the test file (the `TestRecall*` and persistence tests).

- [ ] **Step 3: Run test to verify it fails (compile error or assertion)**

Run: `go test ./memory/`
Expected: FAIL — `Remember` returns 2 values, or `st.DedupThreshold undefined`.

- [ ] **Step 4: Implement the novelty gate**

Add the field to `Store` (`memory/memory.go:60`):

```go
type Store struct {
	journal *sentry.Store
	embed   Embedder
	mu      sync.Mutex
	entries []entry
	nextID  uint64

	// DedupThreshold is the squared-L2 novelty radius. If a new memory's
	// nearest existing neighbor (same tenant) is closer than this, Remember
	// treats it as a duplicate and does not persist it. 0 disables dedup.
	DedupThreshold float32
}
```

Replace `Remember` (`memory/memory.go:103-120`):

```go
// Remember embeds text and, unless it duplicates an existing memory, persists it
// to the journal as an EventMemory and adds it to the in-RAM index. When dedup
// is enabled (DedupThreshold > 0) and the nearest existing memory for tenant is
// within that squared-L2 radius, the text is NOT persisted: the existing id is
// returned with deduped=true. Otherwise the new id is returned with deduped=false.
func (s *Store) Remember(tenant sentry.TenantID, text string, tags []string, src string) (id uint64, deduped bool, err error) {
	vecs, err := s.embed.Embed([]string{text})
	if err != nil {
		return 0, false, fmt.Errorf("memory: embed: %w", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != s.embed.Dim() {
		return 0, false, fmt.Errorf("memory: embedder returned %d vectors / bad dim", len(vecs))
	}
	v := vecs[0]

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.DedupThreshold > 0 {
		var bestID uint64
		var bestDist float32
		found := false
		for _, e := range s.entries {
			if e.tenant != tenant {
				continue
			}
			d := sqL2(v, e.mem.Vector)
			if !found || d < bestDist {
				found, bestDist, bestID = true, d, e.mem.ID
			}
		}
		if found && bestDist < s.DedupThreshold {
			return bestID, true, nil
		}
	}

	p := MemoryPayload{ID: s.nextID, Text: text, Vector: v, Tags: tags, Source: src}
	if _, err := s.journal.Append(tenant, EventMemory, p); err != nil {
		return 0, false, fmt.Errorf("memory: append: %w", err)
	}
	s.entries = append(s.entries, entry{tenant: tenant, mem: p})
	s.nextID++
	return p.ID, false, nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./memory/`
Expected: PASS (all existing tests + 2 new dedup tests).

- [ ] **Step 6: Commit**

```bash
git add memory/
git commit -m "feat(memory): semantic dedup gate in Remember (DedupThreshold, returns deduped)"
```

---

## Task 6: Wire dedup into the MCP `remember` tool

**Files:**
- Modify: `cmd/sentrymcp/main.go:383-400` (the `remember` case) and the flag block near `:73`

- [ ] **Step 1: Update the `remember` handler**

Replace `cmd/sentrymcp/main.go:393-400`:

```go
		s.mu.Lock()
		id, deduped, err := s.mem.Remember(s.tenant, text, tags, src)
		s.mu.Unlock()
		if err != nil {
			return s.toolErr(req.ID, "remember failed: "+err.Error())
		}
		s.moko.Info("remember", map[string]string{"tenant": fmt.Sprint(s.tenant), "id": fmt.Sprint(id), "tags": fmt.Sprint(tags), "len": fmt.Sprint(len(text)), "deduped": fmt.Sprint(deduped)})
		if deduped {
			return s.toolText(req.ID, fmt.Sprintf("already known as memory #%d (deduped, not stored again)", id))
		}
		return s.toolText(req.ID, fmt.Sprintf("remembered as memory #%d", id))
```

- [ ] **Step 2: Add a `-dedup-tau` flag and apply it to the store**

Near the other flags (`cmd/sentrymcp/main.go:73`), add:

```go
	dedupTau := flag.Float64("dedup-tau", envFloat("SENTRY_DEDUP_TAU", 0), "squared-L2 dedup radius for remember (0 = off); set from Phase-0 calibration")
```

Apply the threshold to the store. In `cmd/sentrymcp/main.go`, immediately after `s.mem = mem` (line 104), add:

```go
		s.mem.DedupThreshold = float32(*dedupTau)
```

Add the `envFloat` helper next to the existing `envOr` helper (around `:465`):

```go
func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
```

`strconv` is NOT currently imported in `cmd/sentrymcp/main.go` — add `"strconv"` to the import block (`:18-27`).

- [ ] **Step 3: Build + test the server package**

Run: `go build ./cmd/sentrymcp/ && go test ./cmd/sentrymcp/`
Expected: build OK; existing sentrymcp tests PASS. If a sentrymcp test calls `Remember` directly, update it to the 3-return form (same as Task 5, Step 2).

- [ ] **Step 4: Full build + test sweep**

Run: `go build ./... && go test ./...`
Expected: BUILD OK; all packages `ok` (now including `internal/transcript`, `cmd/sentry-reflect`).

- [ ] **Step 5: Commit**

```bash
git add cmd/sentrymcp/
git commit -m "feat(sentrymcp): remember reports deduped + -dedup-tau flag (SENTRY_DEDUP_TAU)"
```

---

## Task 7: Adversarial verification on real data (before believing)

No new code unless a check fails. Verify the two claims the design rests on.

- [ ] **Step 1: Dedup does not suppress genuinely-distinct facts (τ false-positive check)**

With the live server reachable, embed several of the example facts captured in Task 2 and confirm their pairwise squared-L2 distances are comfortably above the τ you intend to deploy, while paraphrases of the SAME fact fall below it. Record the distance numbers. If distinct facts fall under τ, lower τ and re-check — do not deploy a τ that merges distinct knowledge.

Practical probe (server-side, on the VM, against a scratch tenant so the real corpus is untouched): store fact A, then store a paraphrase of A (expect `deduped`), then store an unrelated fact B (expect stored). Confirm the messages match expectations.

- [ ] **Step 2: The reflection actually captures durable, non-noise facts**

Dry-run the hook logic against a real recent transcript offline:

```bash
go run ./cmd/reflectyield -dir ~/.claude/projects/-Users-alvinnunez-Downloads-matrix-sentry -k <CHOSEN_K> | tail -3
```

Read the last few windows and confirm that, were the agent prompted with `reflectionPrompt` on that window, the facts it would store are genuinely durable (decisions/conventions/gotchas) and not transient. If windows are mostly empty of durable content at the chosen K, raise K and re-record (the calibration was off). Note the finding in `results/auto-remember-yield.md`.

- [ ] **Step 3: Record the verification outcome**

Append a short "Verification" section to `results/auto-remember-yield.md` with the τ distance numbers and the dry-run judgment. Commit:

```bash
git add results/auto-remember-yield.md
git commit -m "docs: auto-remember adversarial verification (tau distances + reflection dry-run)"
```

---

## Task 8: Deploy — install hook + redeploy server

**Files:**
- Modify: `~/.claude/settings.json` (Mac, hooks run here)
- Redeploy: `cmd/sentrymcp` binary on the homelab VM

- [ ] **Step 1: Build + install the hook on the Mac**

```bash
go build -o ~/.local/bin/sentry-reflect ./cmd/sentry-reflect && echo INSTALLED
```
Expected: `INSTALLED`.

- [ ] **Step 2: Register the Stop hook in `~/.claude/settings.json`**

Add to the `hooks` object (alongside the existing `PostToolUse` and `SessionStart`):

```json
"Stop": [
  {
    "hooks": [
      {
        "type": "command",
        "command": "/Users/alvinnunez/.local/bin/sentry-reflect",
        "statusMessage": "matrix-sentry: reflecting on durable knowledge",
        "async": false,
        "timeout": 8
      }
    ]
  }
]
```

`async:false` so the `block` decision on stdout is honored. The hook itself is fast (read transcript + state); it only ever emits the block decision, never network I/O.

- [ ] **Step 3: Redeploy sentrymcp with τ set from calibration**

Cross-compile, ship, set `SENTRY_DEDUP_TAU` from Task 7's numbers, restart:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/sentrymcp ./cmd/sentrymcp
scp /tmp/sentrymcp matrix-sentry:/root/sentrymcp.new
ssh matrix-sentry 'cp /root/sentrymcp /root/sentrymcp.bak && mv /root/sentrymcp.new /root/sentrymcp && \
  grep -q SENTRY_DEDUP_TAU /root/sentrymcp.env || echo "SENTRY_DEDUP_TAU=<TAU_FROM_TASK7>" >> /root/sentrymcp.env && \
  systemctl restart sentrymcp && sleep 1 && systemctl is-active sentrymcp'
```
Expected: `active`. (Replace `<TAU_FROM_TASK7>` with the verified number; keep `.bak` for rollback.)

- [ ] **Step 4: Verify the live tool surfaces dedup**

Call `remember` twice with the same text through the live endpoint and confirm the second response says `(deduped, not stored again)`. Use a scratch tenant or a clearly-test string so the real corpus stays clean; or accept one test memory and note it.

- [ ] **Step 5: Update the handoff + memory**

Add a "Auto-remember LIVE" section to `HANDOFF.md` (chosen K, τ, the Stop hook, dedup) and write/refresh a memory file noting the third memory leg is now automatic. Commit:

```bash
git add HANDOFF.md
git commit -m "docs: auto-remember live — Stop-hook self-report + server dedup close the memory cycle"
```

---

## Notes for the implementer

- **Best-effort hooks are sacred:** `sentry-reflect` must never block or error the agent. Every failure path returns cleanly (exit 0, no stdout). This mirrors `cmd/sentry-record` and `cmd/sentry-recall` exactly.
- **Config loader duplication is intentional:** the codebase already copies `loadConfig`/`loadEnvFile` per hook binary (`sentry-record`, `sentry-recall`). Follow that convention; do NOT refactor into a shared package for this plan (out of scope).
- **Never `sentry.Open` the live journal dir.** The dedup verification on the VM uses the running server's tools, not a second opener.
- **K and τ come from data.** Do not hard-code them from intuition; Task 2 sets K, Task 7 sets τ. The defaults in code (K=40, τ=0) are safe starting points the calibration confirms or overrides.
```
