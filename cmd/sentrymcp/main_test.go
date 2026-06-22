package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"matrixsentry/comms"
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
	// No owner secret in tests → registry with no entries (open/local mode).
	tokens, err := loadTokenRegistry("", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	return &server{store: store, reg: reg, moko: mokoblinks.FromEnv(), tenant: 1, tokens: tokens}
}

func callRecord(t *testing.T, s *server, args map[string]any) rpcResp {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"name": "record_access", "arguments": args})
	return s.callTool(rpcReq{ID: json.RawMessage("1"), Params: params}, s.tenant)
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
	chat, err := comms.New(s.store)
	if err != nil {
		t.Fatal(err)
	}
	s.chat = chat
	return s
}

func callNamed(s *server, name string, args map[string]any) rpcResp {
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	return s.callTool(rpcReq{ID: json.RawMessage("1"), Params: params}, s.tenant)
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

func TestForgetToolValidation(t *testing.T) {
	// Without an embedder (s.mem == nil), the mem==nil guard fires first.
	s := newTestServer(t)
	r := callNamed(s, "forget", map[string]any{})
	m := r.Result.(map[string]any)
	if m["isError"] != true {
		t.Fatalf("forget without embedder must error, got %#v", m)
	}
	content := m["content"].([]map[string]any)[0]["text"].(string)
	if !strings.Contains(content, "embed") {
		t.Fatalf("forget error should mention embedder, got %q", content)
	}

	// With an embedder: missing 'id' should return an error mentioning "id".
	s2 := newMemServer(t)
	r2 := callNamed(s2, "forget", map[string]any{})
	m2 := r2.Result.(map[string]any)
	if m2["isError"] != true {
		t.Fatalf("forget without id must error, got %#v", m2)
	}
	content2 := m2["content"].([]map[string]any)[0]["text"].(string)
	if !strings.Contains(content2, "id") {
		t.Fatalf("forget missing-id error should mention 'id', got %q", content2)
	}
}

func TestCallToolTenantIsolation(t *testing.T) {
	s := newMemServer(t) // *server with mem + a default tenant (1)
	// remember under tenant 2 via callTool's tenant param
	rememberReq := func(text string) rpcReq {
		args, _ := json.Marshal(map[string]any{"name": "remember", "arguments": map[string]any{"text": text}})
		return rpcReq{ID: json.RawMessage("1"), Method: "tools/call", Params: args}
	}
	recallReq := func(q string) rpcReq {
		args, _ := json.Marshal(map[string]any{"name": "recall", "arguments": map[string]any{"query": q, "k": 5}})
		return rpcReq{ID: json.RawMessage("1"), Method: "tools/call", Params: args}
	}
	textOf := func(r rpcResp) string {
		b, _ := json.Marshal(r.Result)
		return string(b)
	}

	s.callTool(rememberReq("ACME-only secret roadmap fact"), 2)
	s.callTool(rememberReq("owner-only personal note"), 1)

	// tenant 2 recall sees ACME, not owner's
	got2 := textOf(s.callTool(recallReq("roadmap fact"), 2))
	if !strings.Contains(got2, "ACME-only") || strings.Contains(got2, "owner-only") {
		t.Fatalf("tenant 2 recall leaked/missing: %s", got2)
	}
	// tenant 1 recall sees owner's, not ACME
	got1 := textOf(s.callTool(recallReq("roadmap fact"), 1))
	if !strings.Contains(got1, "owner-only") || strings.Contains(got1, "ACME-only") {
		t.Fatalf("tenant 1 recall leaked/missing: %s", got1)
	}
}

func TestResolveTenantRoutingAndNoBypass(t *testing.T) {
	s := &server{
		tenant: 1,
		tokens: &tokenRegistry{entries: []tokenEntry{{Secret: "teamsec", Tenant: 2, Label: "team"}}},
		// s.token == "" and s.oauth == nil, but the registry is NON-empty:
	}
	req := func(auth string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		return r
	}
	// team bearer → team tenant (end-to-end routing)
	if tn, ok := s.resolveTenant(req("Bearer teamsec")); !ok || tn != 2 {
		t.Fatalf("team bearer → (%d,%v), want (2,true)", tn, ok)
	}
	// NO bypass: unauthenticated request must NOT fall into open mode when a registry exists
	if _, ok := s.resolveTenant(req("")); ok {
		t.Fatal("unauthenticated request must be rejected when registry has entries (no open-mode bypass)")
	}
	// unknown bearer → rejected
	if _, ok := s.resolveTenant(req("Bearer nope")); ok {
		t.Fatal("unknown bearer must be rejected")
	}
}

func TestResolveTenantOpenModeWhenTrulyUnconfigured(t *testing.T) {
	// No static token, no oauth, EMPTY registry → open/local mode → default tenant.
	s := &server{tenant: 1, tokens: &tokenRegistry{}}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if tn, ok := s.resolveTenant(r); !ok || tn != 1 {
		t.Fatalf("open mode → (%d,%v), want (1,true)", tn, ok)
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

func TestCommsPromote(t *testing.T) {
	s := newMemServer(t)
	textOf := func(r rpcResp) string { b, _ := json.Marshal(r.Result); return string(b) }
	call := func(name string, args map[string]any) rpcResp {
		a, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
		return s.callTool(rpcReq{ID: json.RawMessage("1"), Method: "tools/call", Params: a}, 1)
	}
	// post a message, capture its seq from "posted message #N in area"
	postText := textOf(call("post", map[string]any{"area": "proj/x", "from": "be", "text": "use Fiber not chi"}))
	var seq int
	if _, err := fmt.Sscanf(postText, `{"content":[{"text":"posted message #%d`, &seq); err != nil || seq == 0 {
		// fallback: extract the first integer after '#'
		i := strings.Index(postText, "#")
		if i >= 0 {
			fmt.Sscanf(postText[i+1:], "%d", &seq)
		}
	}
	if seq == 0 {
		t.Fatalf("could not parse seq from post response: %s", postText)
	}
	// promote it → should report a memory id
	got := textOf(call("promote", map[string]any{"area": "proj/x", "seq": float64(seq)}))
	if !strings.Contains(got, "memory #") {
		t.Fatalf("promote did not create a memory: %s", got)
	}
	// promote a missing seq → not-found error
	missing := textOf(call("promote", map[string]any{"area": "proj/x", "seq": float64(999999)}))
	if !strings.Contains(strings.ToLower(missing), "not found") {
		t.Fatalf("promote of missing seq should say not found: %s", missing)
	}
}

func TestAdminCorpusReturnsTenantMemories(t *testing.T) {
	s := newMemServer(t)
	s.mem.Remember(s.tenant, "deploy on fridays", memory.RememberOpts{Tags: []string{"deploy"}})
	s.mem.Remember(s.tenant, "indentation style", memory.RememberOpts{})

	req := httptest.NewRequest(http.MethodGet, "/admin/corpus", nil)
	rec := httptest.NewRecorder()
	s.handleAdminCorpus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Tenant   int `json:"tenant"`
		Count    int `json:"count"`
		Memories []struct {
			ID  uint64    `json:"id"`
			Vec []float32 `json:"vec"`
		} `json:"memories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 2 || len(out.Memories) != 2 {
		t.Fatalf("want 2 memories, got %d", out.Count)
	}
	if len(out.Memories[0].Vec) == 0 {
		t.Fatal("memories must include vectors")
	}
}

func TestAdminCorpusRequiresAuthWhenConfigured(t *testing.T) {
	s := newMemServer(t)
	s.token = "owner-secret"
	reg, err := loadTokenRegistry("", "owner-secret", s.tenant)
	if err != nil {
		t.Fatal(err)
	}
	s.tokens = reg
	req := httptest.NewRequest(http.MethodGet, "/admin/corpus", nil) // no Authorization
	rec := httptest.NewRecorder()
	s.handleAdminCorpus(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer must be 401, got %d", rec.Code)
	}
}

func TestAdminJournalReturnsSemanticEvents(t *testing.T) {
	s := newMemServer(t)
	id1, _, _, _ := s.mem.Remember(s.tenant, "first memory text", memory.RememberOpts{})
	s.mem.Remember(s.tenant, "second memory text", memory.RememberOpts{})
	s.mem.Forget(s.tenant, id1)
	_, _, _ = s.reg.Record(s.tenant, "/some/path", "Read") // an Access event — must NOT appear

	req := httptest.NewRequest(http.MethodGet, "/admin/journal", nil)
	rec := httptest.NewRecorder()
	s.handleAdminJournal(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Events []struct {
			Seq  uint64 `json:"seq"`
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 3 {
		t.Fatalf("want 3 semantic events (2 Memory + 1 Forget), got %d: %+v", len(out.Events), out.Events)
	}
	for _, e := range out.Events {
		if e.Type == "Access" {
			t.Fatal("Access events must be excluded")
		}
	}
	if out.Events[0].Type != "Memory" || out.Events[2].Type != "Forget" {
		t.Fatalf("unexpected types/order: %+v", out.Events)
	}
}

func TestAdminJournalRequiresAuthWhenConfigured(t *testing.T) {
	s := newMemServer(t)
	s.token = "owner-secret"
	reg, err := loadTokenRegistry("", "owner-secret", s.tenant)
	if err != nil {
		t.Fatal(err)
	}
	s.tokens = reg
	rec := httptest.NewRecorder()
	s.handleAdminJournal(rec, httptest.NewRequest(http.MethodGet, "/admin/journal", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer must be 401, got %d", rec.Code)
	}
}

func TestCommsPostReadPromote(t *testing.T) {
	s := newMemServer(t) // *server with mem + chat over a temp journal, default tenant 1
	post := func(area, from, text string) rpcReq {
		args, _ := json.Marshal(map[string]any{"name": "post", "arguments": map[string]any{"area": area, "from": from, "text": text}})
		return rpcReq{ID: json.RawMessage("1"), Method: "tools/call", Params: args}
	}
	textOf := func(r rpcResp) string { b, _ := json.Marshal(r.Result); return string(b) }

	// post in tenant 1, read it back
	if got := textOf(s.callTool(post("proj/x", "be", "schema ready"), 1)); !strings.Contains(got, "#") {
		t.Fatalf("post did not return a seq: %s", got)
	}
	readArgs, _ := json.Marshal(map[string]any{"name": "read", "arguments": map[string]any{"area": "proj/x", "since": 0}})
	got1 := textOf(s.callTool(rpcReq{ID: json.RawMessage("1"), Method: "tools/call", Params: readArgs}, 1))
	if !strings.Contains(got1, "schema ready") || !strings.Contains(got1, "be") {
		t.Fatalf("read missing the message: %s", got1)
	}
	// tenant 2 must NOT see tenant 1's channel
	got2 := textOf(s.callTool(rpcReq{ID: json.RawMessage("1"), Method: "tools/call", Params: readArgs}, 2))
	if strings.Contains(got2, "schema ready") {
		t.Fatalf("tenant 2 saw tenant 1's channel: %s", got2)
	}
	// missing area → tool error
	badArgs, _ := json.Marshal(map[string]any{"name": "post", "arguments": map[string]any{"from": "be", "text": "x"}})
	if got := textOf(s.callTool(rpcReq{ID: json.RawMessage("1"), Method: "tools/call", Params: badArgs}, 1)); !strings.Contains(strings.ToLower(got), "area") {
		t.Fatalf("missing area should error mentioning area: %s", got)
	}
}

func TestRecallIsJournaledWhenEnabled(t *testing.T) {
	s := newMemServer(t)
	s.logRecall = true
	s.mem.Remember(s.tenant, "deploy on fridays", memory.RememberOpts{})

	callNamed(s, "recall", map[string]any{"query": "deploy on fridays", "k": float64(3)})

	et := memory.EventRecall
	var found *memory.RecallPayload
	s.store.Scan(sentry.Filter{Tenant: &s.tenant, Type: &et}, func(r sentry.Record) bool {
		var p memory.RecallPayload
		if sentry.UnmarshalPayload(r.Payload, &p) == nil {
			found = &p
		}
		return true
	})
	if found == nil || found.Query != "deploy on fridays" || len(found.Hits) == 0 {
		t.Fatalf("recall not journaled correctly: %+v", found)
	}
}

func TestRecallNotJournaledWhenDisabled(t *testing.T) {
	s := newMemServer(t)
	s.logRecall = false
	s.mem.Remember(s.tenant, "x", memory.RememberOpts{})
	callNamed(s, "recall", map[string]any{"query": "x", "k": float64(3)})
	et := memory.EventRecall
	n := 0
	s.store.Scan(sentry.Filter{Tenant: &s.tenant, Type: &et}, func(r sentry.Record) bool { n++; return true })
	if n != 0 {
		t.Fatalf("logging disabled but %d EventRecall written", n)
	}
}

func TestAnalyzeRecallReportsCoverage(t *testing.T) {
	s := newMemServer(t)
	s.logRecall = true
	s.mem.Remember(s.tenant, "deploy on fridays", memory.RememberOpts{})
	for _, q := range []string{"deploy on fridays", "something unrelated zzz"} {
		callNamed(s, "recall", map[string]any{"query": q, "k": float64(3)})
	}
	resp := callNamed(s, "analyze_recall", map[string]any{})
	text := respText(t, resp)
	if !strings.Contains(text, "total=2") {
		t.Fatalf("analyze_recall should report total=2, got: %s", text)
	}
	if !strings.Contains(text, "deploy on fridays") {
		t.Fatalf("analyze_recall should echo a recent query, got: %s", text)
	}
}

func TestInboxToolReturnsDirectedMessages(t *testing.T) {
	s := newMemServer(t)
	if s.chat == nil {
		s.chat, _ = comms.New(s.store)
	}
	s.chat.Post(s.tenant, comms.MessagePayload{Area: "ch1", From: "alice", Text: "ping", Target: "08", Kind: "question"})
	s.chat.Post(s.tenant, comms.MessagePayload{Area: "ch2", From: "bob", Text: "fyi", Target: "08", Kind: "info"})
	s.chat.Post(s.tenant, comms.MessagePayload{Area: "ch1", From: "alice", Text: "not for 08", Target: "09"})

	resp := callNamed(s, "inbox", map[string]any{"target": "08"})
	text := respText(t, resp)
	if !strings.Contains(text, "ping") || !strings.Contains(text, "fyi") {
		t.Fatalf("inbox should return both messages directed at 08, got: %s", text)
	}
	if strings.Contains(text, "not for 08") {
		t.Fatalf("inbox leaked a message directed at 09: %s", text)
	}
}

func TestAnalyzeRecallUsesRing(t *testing.T) {
	s := newMemServer(t)
	s.logRecall = true
	s.mem.Remember(s.tenant, "deploy on fridays", memory.RememberOpts{})
	for _, q := range []string{"deploy on fridays", "zzz unrelated"} {
		callNamed(s, "recall", map[string]any{"query": q, "k": float64(3)})
	}
	resp := callNamed(s, "analyze_recall", map[string]any{})
	text := respText(t, resp)
	if !strings.Contains(text, "total=2") {
		t.Fatalf("analyze_recall should report total=2 from the ring, got: %s", text)
	}
	if !strings.Contains(text, "deploy on fridays") {
		t.Fatalf("should echo a recent query, got: %s", text)
	}
}

func TestAdminCommsReturnsTenantMessages(t *testing.T) {
	s := newMemServer(t)
	if s.chat == nil {
		s.chat, _ = comms.New(s.store)
	}
	s.chat.Post(s.tenant, comms.MessagePayload{Area: "X", From: "a1", Kind: "question", Text: "hola?"})
	s.chat.Post(s.tenant, comms.MessagePayload{Area: "X", From: "a2", Kind: "answer", Text: "sí", Ref: 1})
	s.chat.Post(s.tenant, comms.MessagePayload{Area: "Y", From: "a1", Kind: "info", Text: "fyi"})

	req := httptest.NewRequest(http.MethodGet, "/admin/comms", nil)
	rec := httptest.NewRecorder()
	s.handleAdminComms(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Messages []struct {
			Seq  uint64 `json:"seq"`
			Area string `json:"area"`
			From string `json:"from"`
			Kind string `json:"kind"`
			Text string `json:"text"`
			Ref  uint64 `json:"ref"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d", len(out.Messages))
	}
	if out.Messages[0].Area != "X" || out.Messages[0].From != "a1" || out.Messages[0].Kind != "question" {
		t.Fatalf("first message wrong: %+v", out.Messages[0])
	}
}

func TestCommsClearTool(t *testing.T) {
	s := newMemServer(t)
	if s.chat == nil {
		s.chat, _ = comms.New(s.store)
	}
	s.chat.Post(s.tenant, comms.MessagePayload{Area: "build", From: "a", Text: "m1"})
	s.chat.Post(s.tenant, comms.MessagePayload{Area: "build", From: "a", Text: "m2"})
	resp := callNamed(s, "comms_clear", map[string]any{"area": "build"})
	text := respText(t, resp)
	if !strings.Contains(text, "2") {
		t.Fatalf("comms_clear should report 2 cleared, got: %s", text)
	}
	if len(s.chat.Read(s.tenant, "build", 0)) != 0 {
		t.Fatal("area not cleared")
	}
}
