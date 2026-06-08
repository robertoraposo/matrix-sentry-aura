package main

import (
	"encoding/json"
	"testing"
)

func TestParseHook(t *testing.T) {
	in := []byte(`{
		"tool_name": "Read",
		"tool_input": {"file_path": "/repo/a.go"},
		"tool_response": "ok",
		"cwd": "/repo"
	}`)
	h, err := parseHook(in)
	if err != nil {
		t.Fatal(err)
	}
	if h.ToolName != "Read" || h.Cwd != "/repo" {
		t.Errorf("parsed = %+v", h)
	}
	if got := str(h.ToolInput["file_path"]); got != "/repo/a.go" {
		t.Errorf("file_path = %q", got)
	}
}

// buildCallBody produces a valid JSON-RPC tools/call for record_access carrying
// the batch of paths and the source tool.
func TestBuildCallBody(t *testing.T) {
	body := buildCallBody([]string{"/repo/a.go", "/repo/b.go"}, "Bash")

	var msg struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			Name string `json:"name"`
			Args struct {
				Paths []string `json:"paths"`
				Src   string   `json:"src"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.JSONRPC != "2.0" || msg.Method != "tools/call" {
		t.Errorf("envelope = %+v", msg)
	}
	if msg.Params.Name != "record_access" {
		t.Errorf("tool name = %q", msg.Params.Name)
	}
	if len(msg.Params.Args.Paths) != 2 || msg.Params.Args.Src != "Bash" {
		t.Errorf("args = %+v", msg.Params.Args)
	}
}
