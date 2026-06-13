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

func TestWindowsExactMultipleNoTrailingFlush(t *testing.T) {
	// k == total tool-uses -> exactly one window, no extra trailing window.
	w, err := Windows("s", strings.NewReader(fixture), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != 1 {
		t.Fatalf("got %d windows, want 1", len(w))
	}
	if w[0].ToolUses != 3 {
		t.Fatalf("ToolUses = %d, want 3", w[0].ToolUses)
	}
}

func TestWindowsKOnePerToolUse(t *testing.T) {
	// k == 1 -> every tool-use is its own window.
	w, err := Windows("s", strings.NewReader(fixture), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != 3 {
		t.Fatalf("got %d windows, want 3", len(w))
	}
	for i, win := range w {
		if win.ToolUses != 1 {
			t.Fatalf("window %d ToolUses = %d, want 1", i, win.ToolUses)
		}
	}
}
