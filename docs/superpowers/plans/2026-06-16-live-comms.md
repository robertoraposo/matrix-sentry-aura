# Live Comms in the Dashboard (#comms v2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Show real agent comms in the dashboard's Comms kanban by adding `GET /admin/comms` to sentrymcp and making `sentryadmin /api/comms` fetch + group them into the dashboard shape (replacing the empty stub).

**Architecture:** sentrymcp scans `EventMessage` per tenant → raw messages; sentryadmin groups by area into `{columns:[{key,label,color,cards[...]}], agents[...]}`; the frontend already renders that shape (no change).

**Tech Stack:** Go (sentry Scan, encoding/json), the existing dashboard assets.

---

### Task 1: `sentrymcp GET /admin/comms`

**Files:** Modify `cmd/sentrymcp/main.go`, `cmd/sentrymcp/main_test.go`.

CONTEXT: mirror `handleAdminJournal` (already in main.go — read it for the exact pattern: `resolveTenant`→401, `limit` parse, `s.store.Scan(Filter{Tenant:&t,Type:&et}, ...)`, `truncRunes`, JSON encode). `comms.EventMessage` (=5) and `comms.MessagePayload{Area,From,Kind,Text,Target,Ref}` exist; `comms` is already imported in main.go (used by post/read/promote). The server has `chat *comms.Store` but you do NOT need it — scan the journal directly like handleAdminJournal.

- [ ] **Step 1: failing test (append to main_test.go)**

```go
func TestAdminCommsReturnsTenantMessages(t *testing.T) {
	s := newMemServer(t)
	// post messages via the comms store the server already has (s.chat), or append directly
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
			Seq            uint64 `json:"seq"`
			Area, From     string
			Kind, Text     string
			Ref            uint64 `json:"ref"`
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
```
Confirm `s.chat` is non-nil in `newMemServer` (the server builds `comms.New`); if `newMemServer` doesn't set `s.chat`, set it in the test via `s.chat, _ = comms.New(s.store)`. Confirm `comms` is imported in the test file (add `"matrixsentry/comms"` if not). Check `comms.Store.Post(tenant, MessagePayload)` signature (returns `(uint64, error)`).

Run `go test ./cmd/sentrymcp/ -run TestAdminComms -v` → FAIL (handleAdminComms undefined).

- [ ] **Step 2: implement (add near handleAdminJournal)**

```go
// handleAdminComms serves a tenant's recent channel messages (EventMessage) for
// the dashboard's comms kanban. Auth + tenant via resolveTenant. Server-to-server.
func (s *server) handleAdminComms(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.resolveTenant(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	limit := 100
	if q := r.URL.Query().Get("limit"); q != "" {
		if v, err := strconv.Atoi(q); err == nil && v > 0 {
			limit = v
		}
	}
	if limit > 300 {
		limit = 300
	}
	type msg struct {
		Seq    uint64 `json:"seq"`
		TS     int64  `json:"ts"`
		Area   string `json:"area"`
		From   string `json:"from"`
		Kind   string `json:"kind"`
		Text   string `json:"text"`
		Target string `json:"target,omitempty"`
		Ref    uint64 `json:"ref,omitempty"`
	}
	msgs := make([]msg, 0, limit)
	t := tenant
	et := comms.EventMessage
	s.store.Scan(sentry.Filter{Tenant: &t, Type: &et}, func(rec sentry.Record) bool {
		var p comms.MessagePayload
		if sentry.UnmarshalPayload(rec.Payload, &p) != nil {
			return true
		}
		msgs = append(msgs, msg{Seq: uint64(rec.Seq), TS: rec.Tstamp / 1e6, Area: p.Area, From: p.From, Kind: p.Kind, Text: p.Text, Target: p.Target, Ref: p.Ref})
		if len(msgs) > limit {
			msgs = msgs[len(msgs)-limit:]
		}
		return true
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Messages []msg `json:"messages"`
	}{Messages: msgs})
}
```
Register in `serveHTTP` next to `/admin/journal`: `mux.HandleFunc("/admin/comms", s.handleAdminComms)`.

Run `go test ./cmd/sentrymcp/ -run TestAdminComms -v` → PASS. Then `go build ./... && go test ./... -race`.

- [ ] **Step 3: commit**
```bash
git add cmd/sentrymcp/main.go cmd/sentrymcp/main_test.go
git commit -m "feat(sentrymcp): GET /admin/comms — tenant's channel messages for the dashboard"
```

---

### Task 2: `sentryadmin /api/comms` real (group into dashboard shape)

**Files:** Modify `cmd/sentryadmin/api.go`, `cmd/sentryadmin/api_test.go`.

CONTEXT: `api.go` has `apiServer{mcpURL,token,client}`, `fetchCorpus` (the pattern to copy), `palette` ([]string of hues), and the current `handleComms` (stub returning `{"columns":[],"agents":[]}`). `handleJournal` shows the fetch+passthrough pattern.

- [ ] **Step 1: failing test (append to api_test.go)**

```go
func TestAPICommsGroupsByArea(t *testing.T) {
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/comms" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"messages":[
		  {"seq":1,"ts":1,"area":"ASHLEY/COMMS/09-&gt;08","from":"09-voice","kind":"question","text":"q?","ref":0},
		  {"seq":2,"ts":1,"area":"ASHLEY/COMMS/09-&gt;08","from":"08","kind":"answer","text":"a","ref":1},
		  {"seq":3,"ts":1,"area":"ashley/coherence","from":"08","kind":"info","text":"i"}
		]}`))
	}))
	defer mcp.Close()
	srv := &apiServer{mcpURL: mcp.URL, token: "x", client: http.DefaultClient}
	rec := httptest.NewRecorder()
	srv.handleComms(rec, httptest.NewRequest(http.MethodGet, "/api/comms?tenant=personal", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var d struct {
		Columns []struct {
			Label string `json:"label"`
			Cards []struct {
				Author, Type, TypeColor, Text string
				Promotable                    bool `json:"promotable"`
				Reply                         bool `json:"reply"`
			} `json:"cards"`
		} `json:"columns"`
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Columns) != 2 {
		t.Fatalf("want 2 area columns, got %d", len(d.Columns))
	}
	// HTML-escaped area label must be unescaped
	foundUnescaped := false
	for _, c := range d.Columns {
		if c.Label == "ASHLEY/COMMS/09->08" {
			foundUnescaped = true
			if len(c.Cards) != 2 {
				t.Fatalf("that area should have 2 cards, got %d", len(c.Cards))
			}
			// question maps to "pregunta", not promotable; answer is promotable + reply
			if c.Cards[0].Type != "pregunta" || c.Cards[0].Promotable {
				t.Fatalf("question card mapping wrong: %+v", c.Cards[0])
			}
			if c.Cards[1].Type != "respuesta" || !c.Cards[1].Reply {
				t.Fatalf("answer card mapping wrong: %+v", c.Cards[1])
			}
		}
	}
	if !foundUnescaped {
		t.Fatal("area label was not HTML-unescaped")
	}
	if len(d.Agents) != 2 { // 09-voice, 08
		t.Fatalf("want 2 distinct agents, got %d: %v", len(d.Agents), d.Agents)
	}
}

func TestAPICommsUpstreamErrorFallsBackEmpty(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	srv := &apiServer{mcpURL: bad.URL, token: "x", client: http.DefaultClient}
	rec := httptest.NewRecorder()
	srv.handleComms(rec, httptest.NewRequest(http.MethodGet, "/api/comms?tenant=personal", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("UI-safe fallback should still be 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"columns":[]`) {
		t.Fatalf("expected empty fallback, got %s", rec.Body.String())
	}
}
```
Run `go test ./cmd/sentryadmin/ -run TestAPIComms -v` → FAIL.

- [ ] **Step 2: implement — replace the stub `handleComms` body**

```go
type commsMsg struct {
	Seq    uint64 `json:"seq"`
	Area   string `json:"area"`
	From   string `json:"from"`
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	Target string `json:"target"`
	Ref    uint64 `json:"ref"`
	TS     int64  `json:"ts"`
}

var emptyComms = []byte(`{"columns":[],"agents":[]}`)

func (a *apiServer) handleComms(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	req, err := http.NewRequest(http.MethodGet, a.mcpURL+"/admin/comms", nil)
	if err != nil {
		w.Write(emptyComms)
		return
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		w.Write(emptyComms) // UI-safe fallback
		return
	}
	defer resp.Body.Close()
	var in struct {
		Messages []commsMsg `json:"messages"`
	}
	if json.NewDecoder(resp.Body).Decode(&in) != nil {
		w.Write(emptyComms)
		return
	}
	w.Write(buildComms(in.Messages))
}

type commsCard struct {
	ID         uint64 `json:"id"`
	Author     string `json:"author"`
	Type       string `json:"type"`
	TypeColor  string `json:"typeColor"`
	Target     string `json:"target"`
	Text       string `json:"text"`
	Mins       int    `json:"mins"`
	Reply      bool   `json:"reply"`
	Promotable bool   `json:"promotable"`
}
type commsColumn struct {
	Key   string      `json:"key"`
	Label string      `json:"label"`
	Color string      `json:"color"`
	Cards []commsCard `json:"cards"`
}

func buildComms(msgs []commsMsg) []byte {
	typeColor := map[string]string{"pregunta": "#35E6FF", "respuesta": "#34E5A0", "info": "#9B6CFF", "nota": "#7C8AA5"}
	kindToType := func(k string) string {
		switch k {
		case "question":
			return "pregunta"
		case "answer":
			return "respuesta"
		case "info":
			return "info"
		default:
			return "nota"
		}
	}
	now := time.Now().UnixMilli()
	order := []string{}
	cols := map[string]*commsColumn{}
	agentsSeen := map[string]bool{}
	agents := []string{}
	for _, m := range msgs {
		area := unescapeHTML(m.Area)
		col, ok := cols[area]
		if !ok {
			col = &commsColumn{Key: fmt.Sprintf("a%d", len(order)), Label: area, Color: palette[len(order)%len(palette)]}
			cols[area] = col
			order = append(order, area)
		}
		typ := kindToType(m.Kind)
		mins := int((now - m.TS) / 60000)
		if mins < 0 {
			mins = 0
		}
		col.Cards = append(col.Cards, commsCard{
			ID: m.Seq, Author: m.From, Type: typ, TypeColor: typeColor[typ], Target: m.Target,
			Text: m.Text, Mins: mins, Reply: m.Ref != 0, Promotable: typ != "pregunta",
		})
		if m.From != "" && !agentsSeen[m.From] {
			agentsSeen[m.From] = true
			agents = append(agents, m.From)
		}
	}
	out := struct {
		Columns []commsColumn `json:"columns"`
		Agents  []string      `json:"agents"`
	}{Columns: make([]commsColumn, 0, len(order)), Agents: agents}
	for _, area := range order {
		out.Columns = append(out.Columns, *cols[area])
	}
	b, err := json.Marshal(out)
	if err != nil {
		return emptyComms
	}
	return b
}

func unescapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&amp;", "&")
	return s
}
```
Add imports as needed: `strings` (for unescapeHTML), `time`, `fmt` (likely already imported in api.go — check; api.go already uses `fmt`/`time`/`sort`). Add `strings` if missing.

Run `go test ./cmd/sentryadmin/ -run TestAPIComms -v` → PASS. Then `go build ./... && go test ./... -race`, `go vet ./cmd/sentryadmin/`.

- [ ] **Step 3: commit**
```bash
git add cmd/sentryadmin/api.go cmd/sentryadmin/api_test.go
git commit -m "feat(sentryadmin): /api/comms — real channel messages grouped into the kanban shape"
```

---

### Task 3: deploy + verify

**Files:** none (operational).

- [ ] **Step 1: rebuild + redeploy sentrymcp (8808+8809) and sentryadmin (server2)**
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/sentrymcp ./cmd/sentrymcp
scp -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes /tmp/sentrymcp root@10.10.10.96:/root/sentrymcp.new
ssh matrix-sentry 'mv /root/sentrymcp /root/sentrymcp.old && mv /root/sentrymcp.new /root/sentrymcp && chmod +x /root/sentrymcp && systemctl restart sentrymcp && sleep 2 && systemctl is-active sentrymcp'
scp -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes /tmp/sentrymcp root@10.10.10.175:/root/sentrymcp.new
ssh matrix-sentry2 'mv /root/sentrymcp /root/sentrymcp.old && mv /root/sentrymcp.new /root/sentrymcp && chmod +x /root/sentrymcp && systemctl restart sentrymcp-mt && sleep 2 && systemctl is-active sentrymcp-mt'
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/sentryadmin ./cmd/sentryadmin
scp -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes /tmp/sentryadmin root@10.10.10.175:/root/sentryadmin.new
ssh matrix-sentry2 'systemctl stop sentryadmin && mv /root/sentryadmin.new /root/sentryadmin && chmod +x /root/sentryadmin && systemctl start sentryadmin && sleep 1 && systemctl is-active sentryadmin'
```

- [ ] **Step 2: verify** — controller runs a local mac `sentryadmin` against the real 8808 (token read on-server, not echoed), loads the dashboard in Playwright, switches to the Comms tab, and confirms the real areas (e.g. `ASHLEY/COMMS/09->08`, `ashley/coherence`) and messages render with 0 console errors. Also `curl -u admin:PASS http://localhost:8810/api/comms?tenant=personal` on server2 shows non-empty columns.

- [ ] **Step 3: update HANDOFF + commit**
Note `/admin/comms` + the real `/api/comms`; comms kanban now live. Mention clients still need a reconnect only for new TOOLS (this is a dashboard data path, no MCP tool change).
```bash
git add HANDOFF.md && git commit -m "docs: live comms in the dashboard (#comms v2) deployed"
```
