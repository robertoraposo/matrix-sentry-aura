// Command sentry-wake is the single Claude Code / Codex Stop hook that carries
// all turn-end jobs in priority order: wake-on-reply > reflect > close, with a
// block-alert fallback. At turn end it fetches the agent's Matrix Sentry inbox
// and, when new directed messages exist, wakes the turn ({"decision":"block"})
// so an intra-session loop survives across turns; else it runs the reflection
// cadence; else it posts a close confirmation.
//
// It is best-effort and unkillable from the agent's point of view: any problem
// ends in a clean exit 0, it is a no-op without SENTRY_MCP_URL, and
// stop_hook_active prevents wake/reflect loops (the cursor advance is the guard).
//
//	go build -o sentry-wake ./cmd/sentry-wake
//	# register as the Stop hook (async:false), replacing sentry-reflect's entry
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"matrixsentry/internal/transcript"
)

// reflectEvery is the activity threshold (tool-uses per reflection), reused from
// sentry-reflect's Phase-0 yield measurement (K=40).
const reflectEvery = 40

// action is the turn-end decision sentry-wake takes, in priority order.
type action int

const (
	actWake       action = iota // new directed messages: wake the turn (decision:block)
	actReflect                  // no messages, enough activity: run the reflection cadence
	actClose                    // caught up, nothing to do: post a close note, let the turn end
	actBlockAlert               // inside a continuation with undelivered messages: alert, allow stop
)

// state is the per-session persisted cursor + reflection counter (JSON file).
type state struct {
	Cursor       uint64 `json:"cursor"`
	ReflectCount int    `json:"reflectCount"`
}

// inboxMsg is one directed message parsed from the inbox tool reply.
type inboxMsg struct {
	Seq    uint64
	From   string
	Kind   string
	Text   string
	Target string
}

// decide is the pure turn-end policy. It never does I/O so it is unit-testable
// without a server. Priority (all real paths ultimately exit 0 in main):
//
//  1. Wake:       !stopActive && any msg.Seq > st.Cursor  (new Cursor = maxSeq)
//  2. Reflect:    !stopActive && toolDelta >= reflectEvery
//  3. BlockAlert: stopActive  && any msg.Seq > st.Cursor  (won't wake again)
//  4. Close:      otherwise
//
// The returned state is the state to persist for this decision: on Wake the
// cursor advances to maxSeq (the loop guard so the same messages never wake
// twice); every other action leaves the cursor untouched. The ReflectCount is
// left to the caller to advance on a Reflect (main knows the current count).
func decide(msgs []inboxMsg, st state, stopActive bool, toolDelta int) (action, state) {
	var maxSeq uint64
	hasNew := false
	for _, m := range msgs {
		if m.Seq > maxSeq {
			maxSeq = m.Seq
		}
		if m.Seq > st.Cursor {
			hasNew = true
		}
	}
	switch {
	case !stopActive && hasNew:
		ns := st
		ns.Cursor = maxSeq
		return actWake, ns
	case !stopActive && toolDelta >= reflectEvery:
		return actReflect, st
	case stopActive && hasNew:
		return actBlockAlert, st
	default:
		return actClose, st
	}
}

// stateDir is where per-session wake state lives.
func stateDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "matrix-sentry", "wake")
}

// safeName reduces a session id to a single path-safe file name so a crafted id
// can never write outside the state dir. Copied verbatim from sentry-reflect.
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

// readState loads the per-session state; a missing/corrupt file reads as the
// zero state (best-effort, never errors).
func readState(dir, sid string) state {
	var st state
	b, err := os.ReadFile(filepath.Join(dir, safeName(sid)))
	if err != nil {
		return st
	}
	_ = json.Unmarshal(b, &st)
	return st
}

// writeState persists the per-session state as JSON.
func writeState(dir, sid string, st state) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, safeName(sid)), b, 0o644)
}

// --- I/O wiring (Task 3): stdin, config, inbox fetch/parse, block output, post ---

// hookInput is the Stop-event payload on stdin (same shape as sentry-reflect).
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

// buildInboxBody frames a stateless JSON-RPC tools/call for the `inbox` tool.
// The sentrymcp server dispatches tools/call without a prior initialize
// handshake, so one POST is enough. since==0 means "from the beginning".
func buildInboxBody(target string, since uint64) []byte {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "inbox",
			"arguments": map[string]any{
				"target": target,
				"since":  since,
			},
		},
	}
	b, _ := json.Marshal(msg)
	return b
}

// buildPostBody frames a stateless JSON-RPC tools/call for the comms `post`
// tool, used for the close confirmation and the block-alert. kind=note keeps it
// out of the way of directed traffic.
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
				"kind": "note",
			},
		},
	}
	b, _ := json.Marshal(msg)
	return b
}

// parseInbox pulls the directed messages and trailing cursor out of the inbox
// tool's JSON-RPC reply. It reads the human-readable result.content[0].text —
// the same string the server renders — and tolerates anything unexpected: a
// malformed body, an empty inbox, or a "no messages" reply all yield no
// messages and a zero cursor, so main() cleanly falls through to reflect/close.
func parseInbox(rpcReplyBody []byte) ([]inboxMsg, uint64) {
	var r struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rpcReplyBody, &r); err != nil {
		return nil, 0
	}
	if len(r.Result.Content) == 0 {
		return nil, 0
	}
	return parseInboxText(r.Result.Content[0].Text)
}

// parseInboxText parses the rendered inbox text: an optional presence preamble
// and header, then one line per message
//
//	#SEQ [KIND] FROM→TGT @AREA: TEXT
//
// and a trailing "(cursor: #N)". Lines that don't match are skipped.
func parseInboxText(text string) ([]inboxMsg, uint64) {
	var msgs []inboxMsg
	var cursor uint64
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "(cursor: #") {
			num := strings.TrimSuffix(strings.TrimPrefix(ln, "(cursor: #"), ")")
			if n, err := strconv.ParseUint(strings.TrimSpace(num), 10, 64); err == nil {
				cursor = n
			}
			continue
		}
		if m, ok := parseInboxLine(ln); ok {
			msgs = append(msgs, m)
		}
	}
	return msgs, cursor
}

// parseInboxLine parses one rendered message line
// "#SEQ [KIND] FROM→TGT @AREA: TEXT" (the inbox form, which carries @AREA; it
// also tolerates the read form without @AREA). Returns ok=false for any line
// that isn't a well-formed message line.
func parseInboxLine(ln string) (inboxMsg, bool) {
	if !strings.HasPrefix(ln, "#") {
		return inboxMsg{}, false
	}
	seqStr, rest, ok := strings.Cut(ln[1:], " ")
	if !ok {
		return inboxMsg{}, false
	}
	seq, err := strconv.ParseUint(seqStr, 10, 64)
	if err != nil {
		return inboxMsg{}, false
	}
	if !strings.HasPrefix(rest, "[") {
		return inboxMsg{}, false
	}
	kind, rest, ok := strings.Cut(rest[1:], "]")
	if !ok {
		return inboxMsg{}, false
	}
	rest = strings.TrimPrefix(rest, " ")
	from, rest, ok := strings.Cut(rest, "→")
	if !ok {
		return inboxMsg{}, false
	}
	var target, text string
	if tgt, after, ok2 := strings.Cut(rest, " @"); ok2 {
		// inbox form: "TGT @AREA: TEXT" — drop AREA, keep TEXT.
		if _, txt, ok3 := strings.Cut(after, ": "); ok3 {
			target, text = tgt, txt
		} else {
			return inboxMsg{}, false
		}
	} else if tgt, txt, ok2 := strings.Cut(rest, ": "); ok2 {
		// read form: "TGT: TEXT" (no @AREA).
		target, text = tgt, txt
	} else {
		return inboxMsg{}, false
	}
	return inboxMsg{
		Seq:    seq,
		From:   strings.TrimSpace(from),
		Kind:   strings.TrimSpace(kind),
		Text:   text,
		Target: strings.TrimSpace(target),
	}, true
}

func blockOutput(reason string) string {
	b, _ := json.Marshal(map[string]any{"decision": "block", "reason": reason})
	return string(b)
}

// reflectionPrompt is the instruction injected as the Stop "reason" for the
// reflect cadence. Copied verbatim from sentry-reflect so the two hooks nudge
// identically (only the Stop registration is swapped by the installer).
const reflectionPrompt = "Pause before finishing. Reflect on the work since your last memory checkpoint. " +
	"If — and only if — you learned durable knowledge (a decision made, a convention adopted, a gotcha " +
	"discovered) that a future session would benefit from, persist it: first call the recall tool to avoid " +
	"duplicating what is already stored, then call the remember tool once per genuinely-new fact, each fact " +
	"self-contained and concise. If recall surfaces a memory that is now outdated or wrong, call remember " +
	"with supersedes set to that memory's id to replace it instead of storing a contradicting duplicate. " +
	"If remember reports your fact was deduped but it is genuinely distinct (not a restatement of the named " +
	"memory), call remember again with force set to true to store it anyway. " +
	"Do NOT store transient state, file contents, task progress, or anything already in the code or git. " +
	"If nothing durable was learned, store nothing. Be terse — do not narrate this to the user. Then finish."

// wakeReason renders the continuation prompt for a wake: a compact list of the
// new directed messages (seq > sinceCursor) plus the instruction to reply.
func wakeReason(msgs []inboxMsg, sinceCursor uint64) string {
	var b strings.Builder
	b.WriteString("New directed messages in your Matrix Sentry inbox — do not finish yet:\n")
	for _, m := range msgs {
		if m.Seq <= sinceCursor {
			continue
		}
		fmt.Fprintf(&b, "  #%d [%s] %s→%s: %s\n", m.Seq, m.Kind, m.From, m.Target, m.Text)
	}
	b.WriteString("Process them, then reply with post(area, kind=\"answer\", ref=<seq>, target=<from>).")
	return b.String()
}

// closeText is the fire-and-forget note posted when the inbox is caught up.
func closeText(sessionID string, cursor uint64) string {
	return fmt.Sprintf("cierre: %s — inbox caught up @cursor #%d", sessionID, cursor)
}

// blockAlertText is the fire-and-forget alert posted when a continuation still
// holds undelivered messages it won't wake for (the automatic bloqueo signal).
func blockAlertText(msgs []inboxMsg, cursor uint64) string {
	n := 0
	for _, m := range msgs {
		if m.Seq > cursor {
			n++
		}
	}
	return fmt.Sprintf("wake guard: holding, %d undelivered @#%d", n, cursor)
}

// envOr returns the environment value for key, or fallback when it is unset/empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// fetchInbox POSTs the inbox JSON-RPC call and parses the reply. Best-effort:
// any transport/read error yields no messages (main falls through to reflect or
// close). The client's timeout bounds the synchronous wait.
func fetchInbox(client *http.Client, cfg config, target string, since uint64) ([]inboxMsg, uint64) {
	req, err := http.NewRequest("POST", cfg.url, bytes.NewReader(buildInboxBody(target, since)))
	if err != nil {
		return nil, 0
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0
	}
	return parseInbox(body)
}

// post is the fire-and-forget comms post used by close/block-alert. It never
// blocks for long and never errors the caller.
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

// toolActivity returns the tool-use delta since the last reflection and the
// current absolute count. An unreadable transcript reads as no new activity
// (delta 0) so it can never trigger a reflection.
func toolActivity(transcriptPath string, reflectCount int) (delta, current int) {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return 0, reflectCount
	}
	defer f.Close()
	n, err := transcript.CountToolUses(f)
	if err != nil {
		return 0, reflectCount
	}
	return n - reflectCount, n
}

// run is the testable orchestration: it fetches the inbox, decides, persists
// state, and either wakes/reflects (block output on stdout) or posts a
// close/block-alert note. It no-ops (no request, no output) when the URL is
// unset. Every path returns cleanly — main() then exits 0. inboxClient and
// postClient are injected so a stub transport can drive it without a server.
func run(raw []byte, cfg config, inboxClient, postClient *http.Client) {
	h, err := parseHook(raw)
	if err != nil {
		return
	}
	if cfg.url == "" {
		return // no memory server configured: silent no-op
	}
	cwd := h.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	agent := envOr("SENTRY_COMMS_AGENT", filepath.Base(cwd))
	area := envOr("SENTRY_COMMS_AREA", filepath.Base(cwd))

	dir := stateDir()
	st := readState(dir, h.SessionID)

	msgs, _ := fetchInbox(inboxClient, cfg, agent, st.Cursor)
	toolDelta, current := toolActivity(h.TranscriptPath, st.ReflectCount)

	act, ns := decide(msgs, st, h.StopHookActive, toolDelta)
	switch act {
	case actWake:
		_ = writeState(dir, h.SessionID, ns) // cursor advanced by decide: the loop guard
		os.Stdout.WriteString(blockOutput(wakeReason(msgs, st.Cursor)))
	case actReflect:
		ns.ReflectCount = current
		_ = writeState(dir, h.SessionID, ns)
		os.Stdout.WriteString(blockOutput(reflectionPrompt))
	case actBlockAlert:
		post(postClient, cfg, buildPostBody(area, agent, blockAlertText(msgs, st.Cursor)))
	default: // actClose
		post(postClient, cfg, buildPostBody(area, agent, closeText(h.SessionID, st.Cursor)))
	}
}

func main() {
	// Best-effort: any problem → clean exit 0, no output, the stop proceeds.
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return
	}
	inboxClient := &http.Client{Timeout: 5 * time.Second}
	postClient := &http.Client{Timeout: 700 * time.Millisecond}
	run(raw, loadConfig(), inboxClient, postClient)
}
