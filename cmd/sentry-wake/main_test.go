package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDecideWake: a directed message newer than the cursor wakes the turn and
// advances the cursor to the max seq seen.
func TestDecideWake(t *testing.T) {
	msgs := []inboxMsg{
		{Seq: 7, From: "forge", Kind: "task", Text: "do the thing", Target: "backend"},
		{Seq: 9, From: "forge", Kind: "task", Text: "and this one", Target: "backend"},
	}
	act, ns := decide(msgs, state{Cursor: 5, ReflectCount: 3}, false, 0)
	if act != actWake {
		t.Fatalf("action = %v, want actWake", act)
	}
	if ns.Cursor != 9 {
		t.Fatalf("new cursor = %d, want 9 (maxSeq)", ns.Cursor)
	}
	if ns.ReflectCount != 3 {
		t.Fatalf("ReflectCount = %d, want 3 (unchanged by wake)", ns.ReflectCount)
	}
}

// TestDecideNoReWake: once the cursor is at maxSeq, the same inbox must not wake
// again (the cursor advance is the loop guard).
func TestDecideNoReWake(t *testing.T) {
	msgs := []inboxMsg{
		{Seq: 7, From: "forge", Kind: "task", Text: "do the thing"},
		{Seq: 9, From: "forge", Kind: "task", Text: "and this one"},
	}
	act, ns := decide(msgs, state{Cursor: 9}, false, 0)
	if act == actWake {
		t.Fatalf("action = actWake, want NOT actWake (cursor already at maxSeq)")
	}
	if act != actClose {
		t.Fatalf("action = %v, want actClose (caught up, no reflect)", act)
	}
	if ns.Cursor != 9 {
		t.Fatalf("cursor = %d, want 9 (unchanged)", ns.Cursor)
	}
}

// TestDecideStopActiveNeverWakes: inside a continuation (stop_hook_active) we
// must never wake or reflect; undelivered messages select BlockAlert instead.
func TestDecideStopActiveNeverWakes(t *testing.T) {
	msgs := []inboxMsg{{Seq: 12, From: "forge", Kind: "task", Text: "still pending"}}

	act, ns := decide(msgs, state{Cursor: 5}, true, 100)
	if act != actBlockAlert {
		t.Fatalf("action = %v, want actBlockAlert", act)
	}
	// Cursor must NOT advance on a block-alert (we did not deliver/wake).
	if ns.Cursor != 5 {
		t.Fatalf("cursor = %d, want 5 (unchanged on block-alert)", ns.Cursor)
	}

	// stop_hook_active with no undelivered messages but a huge toolDelta must
	// still NOT reflect -> Close.
	act2, _ := decide(nil, state{Cursor: 20}, true, 1000)
	if act2 != actClose {
		t.Fatalf("action = %v, want actClose (stopActive suppresses reflect)", act2)
	}
}

// TestDecideReflectAtThreshold: with no new messages, Reflect fires only at the
// reflectEvery threshold; below it we Close.
func TestDecideReflectAtThreshold(t *testing.T) {
	if act, _ := decide(nil, state{Cursor: 3}, false, reflectEvery-1); act != actClose {
		t.Fatalf("toolDelta %d -> %v, want actClose", reflectEvery-1, act)
	}
	if act, _ := decide(nil, state{Cursor: 3}, false, reflectEvery); act != actReflect {
		t.Fatalf("toolDelta %d -> %v, want actReflect", reflectEvery, act)
	}
	if act, _ := decide(nil, state{Cursor: 3}, false, reflectEvery+50); act != actReflect {
		t.Fatalf("toolDelta %d -> %v, want actReflect", reflectEvery+50, act)
	}
}

// TestDecideWakeBeatsReflect: when both a new message AND the reflect threshold
// are satisfied, Wake wins (higher priority).
func TestDecideWakeBeatsReflect(t *testing.T) {
	msgs := []inboxMsg{{Seq: 4, From: "forge", Text: "reply please"}}
	act, ns := decide(msgs, state{Cursor: 1}, false, reflectEvery+10)
	if act != actWake {
		t.Fatalf("action = %v, want actWake (wake beats reflect)", act)
	}
	if ns.Cursor != 4 {
		t.Fatalf("cursor = %d, want 4", ns.Cursor)
	}
}

// TestDecideCloseWhenCaughtUp: caught up, not enough activity to reflect -> Close.
func TestDecideCloseWhenCaughtUp(t *testing.T) {
	act, ns := decide(nil, state{Cursor: 42, ReflectCount: 10}, false, 5)
	if act != actClose {
		t.Fatalf("action = %v, want actClose", act)
	}
	if ns != (state{Cursor: 42, ReflectCount: 10}) {
		t.Fatalf("state = %+v, want unchanged", ns)
	}
}

// TestSafeNameTraversal: a crafted session id can never escape the state dir.
// safeName (copied verbatim from sentry-reflect) flattens path separators to
// "_" so the whole id becomes one filename; the invariant under test is that the
// result is a single safe basename (no separators, never ".." or empty).
func TestSafeNameTraversal(t *testing.T) {
	// Exact mappings mirroring the verbatim implementation.
	exact := map[string]string{
		"../../etc/passwd":  ".._.._etc_passwd",
		"a/b/c":             "a_b_c",
		"":                  "default",
		"normal-session-id": "normal-session-id",
	}
	for in, want := range exact {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}

	// Security invariant on a broad set of hostile inputs: no path separators
	// survive, and the result is never a traversal token or empty.
	hostile := []string{
		"../../etc/passwd", "../../../root/.claude", "a/b/c", "..", "../..",
		"/", "/etc/passwd", "foo/../bar", "", "with space", "..\\..\\win",
	}
	for _, in := range hostile {
		got := safeName(in)
		if got == "" {
			t.Errorf("safeName(%q) = empty (unsafe)", in)
		}
		if got == ".." || got == "." {
			t.Errorf("safeName(%q) = %q (traversal token)", in, got)
		}
		if strings.Contains(got, "/") {
			t.Errorf("safeName(%q) = %q contains a path separator", in, got)
		}
	}
}

// TestStateRoundTrip: write then read yields the same state (JSON persistence).
func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sid := "sess-123"
	want := state{Cursor: 99, ReflectCount: 7}
	if err := writeState(dir, sid, want); err != nil {
		t.Fatalf("writeState: %v", err)
	}
	got := readState(dir, sid)
	if got != want {
		t.Fatalf("readState = %+v, want %+v", got, want)
	}
	// A missing file reads as the zero state (best-effort, never errors).
	if z := readState(dir, "no-such-session"); z != (state{}) {
		t.Fatalf("readState(missing) = %+v, want zero state", z)
	}
	// The persisted file must live under the state dir, not escape it.
	if _, err := filepath.Rel(dir, filepath.Join(dir, safeName(sid))); err != nil {
		t.Fatalf("state file escaped dir: %v", err)
	}
}

// --- Task 3: I/O wiring (inbox fetch/parse, wake output, main) ---

// stubTransport records how many requests reached the network and can return a
// canned reply body or force an error, so the fetch/post paths are testable
// without a live server. Mirrors cmd/sentry-deploy's test stub.
type stubTransport struct {
	calls   int
	lastURL string
	reply   []byte
	err     error
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.calls++
	s.lastURL = r.URL.String()
	if s.err != nil {
		return nil, s.err
	}
	body := s.reply
	if body == nil {
		body = []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	}
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

func stubClient(t *stubTransport) *http.Client { return &http.Client{Transport: t} }

// mustReply frames text as a JSON-RPC tools/call reply, mirroring the server's
// toolStruct output (result.content[0].text).
func mustReply(text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	})
	return b
}

func mustJSON(m map[string]any) []byte {
	b, _ := json.Marshal(m)
	return b
}

// captureStdout redirects os.Stdout for the duration of fn and returns what was
// written, so the block-decision output of run() can be asserted.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	_ = w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b)
}

// TestParseInbox: a real inbox reply parses into messages + trailing cursor;
// an "inbox empty" reply and malformed bodies yield no messages.
func TestParseInbox(t *testing.T) {
	text := "~ forge [antelisp]: working (2m)\n" +
		"2 message(s) for \"backend\":\n" +
		"#7 [task] forge→backend @antelisp: run the migration\n" +
		"#9 [question] papa→backend @antelisp: status?\n" +
		"(cursor: #9)"
	msgs, cursor := parseInbox(mustReply(text))
	if len(msgs) != 2 {
		t.Fatalf("parsed %d messages, want 2: %+v", len(msgs), msgs)
	}
	if cursor != 9 {
		t.Fatalf("cursor = %d, want 9", cursor)
	}
	m0 := msgs[0]
	if m0.Seq != 7 || m0.From != "forge" || m0.Kind != "task" || m0.Target != "backend" || m0.Text != "run the migration" {
		t.Fatalf("msg[0] = %+v", m0)
	}
	m1 := msgs[1]
	if m1.Seq != 9 || m1.From != "papa" || m1.Kind != "question" || m1.Target != "backend" || m1.Text != "status?" {
		t.Fatalf("msg[1] = %+v", m1)
	}

	// "inbox empty" -> no messages, no cursor.
	if msgs, cursor := parseInbox(mustReply(`inbox empty for "backend" (since #0)`)); len(msgs) != 0 || cursor != 0 {
		t.Fatalf("empty inbox -> %d msgs, cursor %d, want 0/0", len(msgs), cursor)
	}

	// Malformed bodies -> no messages, no panic.
	if msgs, _ := parseInbox([]byte("not json at all")); len(msgs) != 0 {
		t.Fatalf("garbage body -> %d msgs, want 0", len(msgs))
	}
	if msgs, _ := parseInbox(mustReply("random prose with no message lines")); len(msgs) != 0 {
		t.Fatalf("no-# reply -> %d msgs, want 0", len(msgs))
	}
	if msgs, _ := parseInbox([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)); len(msgs) != 0 {
		t.Fatalf("empty content -> %d msgs, want 0", len(msgs))
	}
}

// TestBuildInboxBody: a valid JSON-RPC tools/call for the `inbox` tool carrying
// target + since.
func TestBuildInboxBody(t *testing.T) {
	body := buildInboxBody("backend", 12)
	var msg struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  struct {
			Name string `json:"name"`
			Args struct {
				Target string `json:"target"`
				Since  uint64 `json:"since"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.JSONRPC != "2.0" || msg.Method != "tools/call" {
		t.Errorf("envelope = %+v", msg)
	}
	if msg.Params.Name != "inbox" {
		t.Errorf("tool name = %q, want inbox", msg.Params.Name)
	}
	if msg.Params.Args.Target != "backend" || msg.Params.Args.Since != 12 {
		t.Errorf("args = %+v, want target=backend since=12", msg.Params.Args)
	}
}

// TestWakeReasonContainsMessages: the wake reason lists the new directed
// messages (seq + from + text) and excludes anything at/below the cursor.
func TestWakeReasonContainsMessages(t *testing.T) {
	msgs := []inboxMsg{
		{Seq: 7, From: "forge", Kind: "task", Text: "run the migration", Target: "backend"},
		{Seq: 9, From: "papa", Kind: "question", Text: "status?", Target: "backend"},
	}
	reason := wakeReason(msgs, 5)
	for _, want := range []string{"#7", "#9", "forge", "papa", "run the migration", "status?"} {
		if !strings.Contains(reason, want) {
			t.Errorf("wakeReason missing %q in:\n%s", want, reason)
		}
	}
	// A message already delivered (seq <= cursor) must be excluded.
	if r := wakeReason([]inboxMsg{{Seq: 3, From: "old", Text: "stale msg"}}, 5); strings.Contains(r, "stale msg") {
		t.Errorf("wakeReason should exclude seq<=cursor, got:\n%s", r)
	}
}

// TestMainNoopWhenURLUnset: with no configured URL, run() makes no network
// request and writes no output (exit 0, silent no-op).
func TestMainNoopWhenURLUnset(t *testing.T) {
	in := mustJSON(map[string]any{
		"session_id": "s1", "transcript_path": "/nonexistent",
		"stop_hook_active": false, "cwd": "/repo",
	})
	inbox, postC := &stubTransport{}, &stubTransport{}
	out := captureStdout(t, func() {
		run(in, config{}, stubClient(inbox), stubClient(postC)) // empty config: url == ""
	})
	if inbox.calls != 0 || postC.calls != 0 {
		t.Fatalf("expected no requests when URL unset, got inbox=%d post=%d", inbox.calls, postC.calls)
	}
	if out != "" {
		t.Fatalf("expected no output when URL unset, got %q", out)
	}
}

// TestRunWakesOnNewMessage: end-to-end wiring — a fresh inbox message drives run
// to fetch the inbox once, emit a {"decision":"block"} carrying the message, and
// NOT post (wake, not close).
func TestRunWakesOnNewMessage(t *testing.T) {
	// Isolate per-session state under a temp cache dir so a prior run can't
	// have advanced the cursor past our message.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	t.Setenv("SENTRY_COMMS_AGENT", "backend")
	t.Setenv("SENTRY_COMMS_AREA", "antelisp")

	text := "1 message(s) for \"backend\":\n#42 [task] forge→backend @antelisp: ship it\n(cursor: #42)"
	inbox := &stubTransport{reply: mustReply(text)}
	postC := &stubTransport{}

	tf := filepath.Join(tmp, "transcript.jsonl")
	if err := os.WriteFile(tf, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	in := mustJSON(map[string]any{
		"session_id": "wake-sess", "transcript_path": tf,
		"stop_hook_active": false, "cwd": tmp,
	})

	out := captureStdout(t, func() {
		run(in, config{url: "https://mcp.example/mcp", token: "tk"}, stubClient(inbox), stubClient(postC))
	})

	if inbox.calls != 1 {
		t.Fatalf("expected 1 inbox fetch, got %d", inbox.calls)
	}
	if postC.calls != 0 {
		t.Fatalf("a wake must not post, got %d post calls", postC.calls)
	}
	var dec struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &dec); err != nil {
		t.Fatalf("stdout not a block-decision JSON: %q (%v)", out, err)
	}
	if dec.Decision != "block" {
		t.Fatalf("decision = %q, want block", dec.Decision)
	}
	if !strings.Contains(dec.Reason, "ship it") || !strings.Contains(dec.Reason, "#42") {
		t.Fatalf("wake reason missing the message: %q", dec.Reason)
	}
}
