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
