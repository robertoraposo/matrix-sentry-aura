package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	// "../escape" sanitizes to a single safe name inside dir (sessA + .._escape)
	if matches, _ := filepath.Glob(filepath.Join(dir, "*")); len(matches) != 2 {
		t.Fatalf("expected 2 state files inside dir, got %v", matches)
	}
	// ".." and "." must not collapse to the dir itself (would break the counter)
	for _, bad := range []string{"..", ".", ""} {
		if got := safeName(bad); got == "/" || got == "." || got == "" {
			t.Fatalf("safeName(%q) = %q, would write to the state dir itself", bad, got)
		}
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

// decide drives the hook's decision logic the way main() does, but against an
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

func TestReflectionPromptAdvertisesSupersede(t *testing.T) {
	// The prompt must tell the agent to update stale memories via supersedes,
	// not just store new ones — this is what closes the truth-blind-dedup gap.
	for _, want := range []string{"recall", "remember", "supersedes"} {
		if !strings.Contains(reflectionPrompt, want) {
			t.Fatalf("reflectionPrompt missing %q", want)
		}
	}
}

func TestReflectionPromptAdvertisesForce(t *testing.T) {
	// The prompt must tell the agent to force-store a genuinely-distinct fact that
	// was deduped against a vocabulary-similar but unrelated memory.
	if !strings.Contains(reflectionPrompt, "force") {
		t.Fatal("reflectionPrompt should advertise force:true for distinct deduped facts")
	}
}
