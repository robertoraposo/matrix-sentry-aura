package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"matrixsentry/memory"
	"matrixsentry/mokoblinks"
	"matrixsentry/sentry"
)

func newTestServer(t *testing.T) *server {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sentry")
	store, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	reg, err := sentry.NewRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	return &server{store: store, reg: reg, moko: mokoblinks.FromEnv(), tenant: 1}
}

func callRecord(t *testing.T, s *server, args map[string]any) rpcResp {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"name": "record_access", "arguments": args})
	return s.callTool(rpcReq{ID: json.RawMessage("1"), Params: params})
}

func respText(t *testing.T, r rpcResp) string {
	t.Helper()
	m, ok := r.Result.(map[string]any)
	if !ok {
		t.Fatalf("result not a map: %#v", r.Result)
	}
	if m["isError"] == true {
		t.Fatalf("tool returned error: %#v", m["content"])
	}
	content := m["content"].([]map[string]any)
	return content[0]["text"].(string)
}

func accessItems(t *testing.T, s *server) []sentry.AccessPayload {
	t.Helper()
	tenant := s.tenant
	etype := sentry.EventAccess
	var out []sentry.AccessPayload
	s.store.Scan(sentry.Filter{Tenant: &tenant, Type: &etype}, func(r sentry.Record) bool {
		var p sentry.AccessPayload
		sentry.UnmarshalPayload(r.Payload, &p)
		out = append(out, p)
		return true
	})
	return out
}

func TestRecordAccessByPath(t *testing.T) {
	s := newTestServer(t)
	resp := callRecord(t, s, map[string]any{"path": "/repo/a.go", "src": "Read"})
	respText(t, resp) // asserts non-error

	items := accessItems(t, s)
	if len(items) != 1 {
		t.Fatalf("expected 1 access, got %d", len(items))
	}
	if items[0].ItemID != 1 || items[0].Source != "Read" {
		t.Errorf("access = %+v", items[0])
	}
}

func TestRecordAccessBatch(t *testing.T) {
	s := newTestServer(t)
	resp := callRecord(t, s, map[string]any{
		"paths": []any{"/repo/a.go", "/repo/b.go", "/repo/a.go"},
		"src":   "Bash",
	})
	respText(t, resp)

	items := accessItems(t, s)
	if len(items) != 3 {
		t.Fatalf("expected 3 accesses, got %d", len(items))
	}
	if items[0].ItemID != 1 || items[1].ItemID != 2 || items[2].ItemID != 1 {
		t.Errorf("expected ids 1,2,1; got %d,%d,%d", items[0].ItemID, items[1].ItemID, items[2].ItemID)
	}
}

// In HTTP mode the MokoBlinks mirror must flush after each tool call — otherwise
// low-volume telemetry sits in the batch buffer (batchSize 50) and never ships,
// so the live log explorer looks empty even though the journal is recording.
func TestHTTPHandlerFlushesMokoBlinks(t *testing.T) {
	got := make(chan string, 4)
	ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer ingest.Close()
	t.Setenv("MOKOBLINKS_URL", ingest.URL)
	t.Setenv("MOKOBLINKS_API_KEY", "test")

	s := newTestServer(t) // newTestServer builds moko via FromEnv, now enabled

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"record_access","arguments":{"path":"/x/a.go","src":"Read"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	s.handleHTTP(httptest.NewRecorder(), req)

	select {
	case line := <-got:
		if !strings.Contains(line, "record_access") {
			t.Errorf("MokoBlinks received a line without record_access: %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("MokoBlinks never received the record_access log (HTTP handler did not flush)")
	}
}

func TestRecordAccessByItemBackCompat(t *testing.T) {
	s := newTestServer(t)
	resp := callRecord(t, s, map[string]any{"item": float64(5)})
	txt := respText(t, resp)
	if !strings.Contains(txt, "5") {
		t.Errorf("expected response to mention item 5, got %q", txt)
	}
	items := accessItems(t, s)
	if len(items) != 1 || items[0].ItemID != 5 {
		t.Errorf("expected single access item 5, got %+v", items)
	}
}

// --- semantic memory tools ---

// testEmbedder is a tiny 2-D embedder: known words map to fixed points so the
// recall ordering is deterministic; unknown text lands far away.
type testEmbedder struct{}

func (testEmbedder) Dim() int { return 2 }
func (testEmbedder) Embed(texts []string) ([][]float32, error) {
	pts := map[string][]float32{
		"prefer tabs over spaces": {0, 0},
		"indentation style":       {0.1, 0},
		"deploy on fridays":       {9, 9},
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if v, ok := pts[t]; ok {
			out[i] = v
		} else {
			out[i] = []float32{5, 5}
		}
	}
	return out, nil
}

func newMemServer(t *testing.T) *server {
	t.Helper()
	s := newTestServer(t)
	mem, err := memory.New(s.store, testEmbedder{})
	if err != nil {
		t.Fatal(err)
	}
	s.mem = mem
	return s
}

func callNamed(s *server, name string, args map[string]any) rpcResp {
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	return s.callTool(rpcReq{ID: json.RawMessage("1"), Params: params})
}

func TestRememberThenRecall(t *testing.T) {
	s := newMemServer(t)
	respText(t, callNamed(s, "remember", map[string]any{"text": "prefer tabs over spaces", "tags": []any{"style"}}))
	respText(t, callNamed(s, "remember", map[string]any{"text": "deploy on fridays"}))

	txt := respText(t, callNamed(s, "recall", map[string]any{"query": "indentation style", "k": float64(2)}))
	if !strings.Contains(txt, "prefer tabs over spaces") {
		t.Fatalf("recall did not surface the relevant memory: %q", txt)
	}
	// nearest must come before the far one
	if i, j := strings.Index(txt, "tabs"), strings.Index(txt, "fridays"); i == -1 || (j != -1 && i > j) {
		t.Fatalf("recall ordering wrong: %q", txt)
	}
}

func TestRememberRequiresText(t *testing.T) {
	s := newMemServer(t)
	r := callNamed(s, "remember", map[string]any{})
	m := r.Result.(map[string]any)
	if m["isError"] != true {
		t.Fatalf("remember without text should error, got %#v", m)
	}
}

func TestMemoryToolsErrorWithoutEmbedder(t *testing.T) {
	s := newTestServer(t) // s.mem stays nil (no -ollama configured)
	for _, name := range []string{"remember", "recall"} {
		r := callNamed(s, name, map[string]any{"text": "x", "query": "x"})
		m := r.Result.(map[string]any)
		if m["isError"] != true {
			t.Fatalf("%s without embedder must error clearly, got %#v", name, m)
		}
		if !strings.Contains(m["content"].([]map[string]any)[0]["text"].(string), "embed") {
			t.Fatalf("%s error should mention embeddings: %#v", name, m["content"])
		}
	}
}

func TestRecallReportsEmptyStore(t *testing.T) {
	s := newMemServer(t)
	txt := respText(t, callNamed(s, "recall", map[string]any{"query": "anything"}))
	if !strings.Contains(strings.ToLower(txt), "no ") && !strings.Contains(txt, "0") {
		t.Fatalf("empty recall should say so, got %q", txt)
	}
}

func TestMemoryToolsListed(t *testing.T) {
	names := map[string]bool{}
	for _, tl := range toolList() {
		names[tl["name"].(string)] = true
	}
	for _, want := range []string{"remember", "recall"} {
		if !names[want] {
			t.Fatalf("tool %q not advertised in tools/list", want)
		}
	}
}

func TestBoolArg(t *testing.T) {
	if !boolArg(map[string]any{"force": true}, "force") {
		t.Fatal("true not parsed")
	}
	if boolArg(map[string]any{"force": false}, "force") {
		t.Fatal("false not parsed")
	}
	if boolArg(map[string]any{}, "force") {
		t.Fatal("missing should be false")
	}
	if boolArg(map[string]any{"force": "yes"}, "force") {
		t.Fatal("non-bool should be false")
	}
}
