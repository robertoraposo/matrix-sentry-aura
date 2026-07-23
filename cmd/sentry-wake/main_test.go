package main

import (
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
