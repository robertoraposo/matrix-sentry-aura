package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// stubTransport records how many requests reached the network and can force an
// error, so the best-effort/no-op paths are testable without a live server.
type stubTransport struct {
	calls   int
	lastURL string
	err     error
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.calls++
	s.lastURL = r.URL.String()
	if s.err != nil {
		return nil, s.err
	}
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{}}`)),
		Header:     make(http.Header),
	}, nil
}

func stubClient(t *stubTransport) *http.Client { return &http.Client{Transport: t} }

func TestIsDeployCommand(t *testing.T) {
	deploy := []string{
		"systemctl restart sentrymcp",
		"systemctl reload sentrymcp",
		"sudo systemctl restart sentrymcp",
		"gh pr merge 5",
		"gh pr merge --squash 12",
		"git merge feat/x",
		"scp bin matrix-sentry:/root/sentrymcp",
		"git push origin main",
		"git push deploy",
	}
	benign := []string{
		"ls",
		"go test ./...",
		"git status",
		"git push origin feat/comms-hooks-watcher",
		"echo hello",
		"cat main.go",
	}
	for _, c := range deploy {
		if !isDeployCommand(c) {
			t.Errorf("isDeployCommand(%q) = false, want true", c)
		}
	}
	for _, c := range benign {
		if isDeployCommand(c) {
			t.Errorf("isDeployCommand(%q) = true, want false", c)
		}
	}
}

// buildPostBody produces a valid JSON-RPC tools/call for the comms `post` tool
// carrying area/from/text (and kind=info).
func TestBuildPostBody(t *testing.T) {
	body := buildPostBody("proj/x", "backend", "deploy: git merge feat/x @ /repo")

	var msg struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  struct {
			Name string `json:"name"`
			Args struct {
				Area string `json:"area"`
				From string `json:"from"`
				Text string `json:"text"`
				Kind string `json:"kind"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.JSONRPC != "2.0" || msg.Method != "tools/call" {
		t.Errorf("envelope = %+v", msg)
	}
	if msg.Params.Name != "post" {
		t.Errorf("tool name = %q, want post", msg.Params.Name)
	}
	if msg.Params.Args.Area != "proj/x" || msg.Params.Args.From != "backend" {
		t.Errorf("args = %+v", msg.Params.Args)
	}
	if !strings.HasPrefix(msg.Params.Args.Text, "deploy:") {
		t.Errorf("text = %q, want deploy: prefix", msg.Params.Args.Text)
	}
	if msg.Params.Args.Kind != "info" {
		t.Errorf("kind = %q, want info", msg.Params.Args.Kind)
	}
}

// A deploy command over Bash posts exactly one JSON-RPC request when the URL is
// configured.
func TestRunPostsOnDeploy(t *testing.T) {
	st := &stubTransport{}
	in := []byte(`{"tool_name":"Bash","tool_input":{"command":"systemctl restart sentrymcp"},"tool_response":"ok","cwd":"/repo"}`)
	run(in, config{url: "https://mcp.example/mcp", token: "t"}, stubClient(st))
	if st.calls != 1 {
		t.Fatalf("expected 1 post on a deploy command, got %d", st.calls)
	}
}

// A non-deploy Bash command and a non-Bash tool must not post.
func TestRunNoopOnBenign(t *testing.T) {
	for _, in := range [][]byte{
		[]byte(`{"tool_name":"Bash","tool_input":{"command":"git status"},"cwd":"/repo"}`),
		[]byte(`{"tool_name":"Read","tool_input":{"file_path":"/repo/a.go"},"cwd":"/repo"}`),
	} {
		st := &stubTransport{}
		run(in, config{url: "https://mcp.example/mcp"}, stubClient(st))
		if st.calls != 0 {
			t.Errorf("expected no post for %s, got %d calls", in, st.calls)
		}
	}
}

// No endpoint configured -> silent no-op, no request, even on a deploy command.
func TestNoopWhenURLUnset(t *testing.T) {
	st := &stubTransport{}
	in := []byte(`{"tool_name":"Bash","tool_input":{"command":"gh pr merge 5"},"cwd":"/repo"}`)
	run(in, config{}, stubClient(st)) // empty config: url == ""
	if st.calls != 0 {
		t.Fatalf("expected no request when URL unset, got %d", st.calls)
	}
}

// Best-effort: an HTTP transport error on a deploy command must not panic; run
// returns cleanly (main exits 0).
func TestRunBestEffortOnHTTPError(t *testing.T) {
	st := &stubTransport{err: errors.New("network down")}
	in := []byte(`{"tool_name":"Bash","tool_input":{"command":"git merge feat/x"},"cwd":"/repo"}`)
	run(in, config{url: "https://mcp.example/mcp"}, stubClient(st)) // must not panic
	if st.calls != 1 {
		t.Fatalf("expected the request to be attempted once, got %d", st.calls)
	}
}

// Malformed stdin is a clean no-op (parseHook error path).
func TestRunNoopOnGarbage(t *testing.T) {
	st := &stubTransport{}
	run([]byte("not json"), config{url: "https://mcp.example/mcp"}, stubClient(st))
	if st.calls != 0 {
		t.Fatalf("garbage stdin should not post, got %d calls", st.calls)
	}
}
