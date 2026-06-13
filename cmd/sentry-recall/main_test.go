package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseSessionStart(t *testing.T) {
	in := `{"cwd":"/Users/x/Downloads/matrix-sentry","source":"startup","hook_event_name":"SessionStart"}`
	h, err := parseHook([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if h.Cwd != "/Users/x/Downloads/matrix-sentry" || h.Source != "startup" {
		t.Fatalf("parsed %+v", h)
	}
}

func TestQueryFromCwd(t *testing.T) {
	cases := map[string]string{
		"/Users/x/Downloads/matrix-sentry": "matrix-sentry",
		"/home/alvin/blazeolt/":            "blazeolt",
		"/srv/repo":                        "repo",
		"":                                 "",
	}
	for in, want := range cases {
		if got := queryFromCwd(in); got != want {
			t.Errorf("queryFromCwd(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShouldInject(t *testing.T) {
	// inject on startup/resume; skip compact/clear (context already present)
	yes := []string{"startup", "resume", ""}
	no := []string{"compact", "clear"}
	for _, s := range yes {
		if !shouldInject(s) {
			t.Errorf("shouldInject(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if shouldInject(s) {
			t.Errorf("shouldInject(%q) = true, want false", s)
		}
	}
}

func TestBuildRecallBody(t *testing.T) {
	b := buildRecallBody("matrix-sentry", 5)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	p := m["params"].(map[string]any)
	if p["name"] != "recall" {
		t.Fatalf("name = %v", p["name"])
	}
	args := p["arguments"].(map[string]any)
	if args["query"] != "matrix-sentry" || args["k"].(float64) != 5 {
		t.Fatalf("args = %v", args)
	}
}

func TestParseRecallResponseExtractsText(t *testing.T) {
	resp := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"3 relevant memories for \"x\":\n1. [#2] foo"}]}}`
	txt := parseRecallText([]byte(resp))
	if !strings.Contains(txt, "relevant memories") {
		t.Fatalf("extracted %q", txt)
	}
}

func TestParseRecallResponseHandlesError(t *testing.T) {
	if txt := parseRecallText([]byte(`not json`)); txt != "" {
		t.Fatalf("bad json should yield empty, got %q", txt)
	}
	if txt := parseRecallText([]byte(`{"result":{}}`)); txt != "" {
		t.Fatalf("missing content should yield empty, got %q", txt)
	}
}

func TestFormatOutputInjectsContext(t *testing.T) {
	out := formatOutput("3 relevant memories for \"matrix-sentry\":\n1. [#2] foo")
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	hso := m["hookSpecificOutput"].(map[string]any)
	if hso["hookEventName"] != "SessionStart" {
		t.Fatalf("hookEventName = %v", hso["hookEventName"])
	}
	ctx, _ := hso["additionalContext"].(string)
	if !strings.Contains(ctx, "foo") || !strings.Contains(strings.ToLower(ctx), "memor") {
		t.Fatalf("additionalContext missing memory content: %q", ctx)
	}
}

func TestFormatOutputEmptyWhenNoMemories(t *testing.T) {
	// recall text saying nothing was found -> no injection (empty output)
	for _, txt := range []string{"", "no memories found for \"x\" (0 stored matches)"} {
		if out := formatOutput(txt); out != "" {
			t.Fatalf("expected no output for %q, got %q", txt, out)
		}
	}
}

func TestFormatOutputTruncatesTo10k(t *testing.T) {
	big := "1 relevant memory:\n" + strings.Repeat("x", 20000)
	out := formatOutput(big)
	var m map[string]any
	json.Unmarshal([]byte(out), &m)
	ctx := m["hookSpecificOutput"].(map[string]any)["additionalContext"].(string)
	if len(ctx) > 10000 {
		t.Fatalf("additionalContext %d chars, must be <= 10000", len(ctx))
	}
}
