# Admin Live Journal (v2.1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Replace the dashboard's simulated Journal panel with the real append-only journal's semantic events (Memory/Forget/Message) for the configured tenant.

**Architecture:** `sentrymcp GET /admin/journal` (Scan by tenant, decode semantic events) → `sentryadmin GET /api/journal` (gated proxy) → `live.js` caches + exposes them; `index.html` seeds the journal from real events and polls for new ones, falling back to the synthetic generator.

**Tech Stack:** Go (sentry.Store.Scan, encoding/json, httptest), the embedded dashboard assets.

---

### Task 1: `sentrymcp GET /admin/journal`

**Files:** Modify `cmd/sentrymcp/main.go`, `cmd/sentrymcp/main_test.go`.

CONTEXT: the `server` has `store *sentry.Store`. `sentry.Scan(Filter{Tenant *TenantID, Type *EventType}, func(Record) bool)` iterates seq-ascending. `Record{Seq, Tstamp (int64 ns), Type EventType, Tenant, Payload []byte}`. Decode with `sentry.UnmarshalPayload(rec.Payload, &v)`. Event types: `memory.EventMemory`=3 (`memory.MemoryPayload{ID,Text}`), `memory.EventForget`=4 (`memory.ForgetPayload{ID}`), `comms.EventMessage`=5 (`comms.MessagePayload{Area,From,Kind,Text,Target}`). `resolveTenant(r)` gives the tenant. `_trunc` helper: just slice runes.

- [ ] **Step 1: failing tests (append to cmd/sentrymcp/main_test.go)**

```go
func TestAdminJournalReturnsSemanticEvents(t *testing.T) {
	s := newMemServer(t)
	id1, _, _, _ := s.mem.Remember(s.tenant, "first memory text", memory.RememberOpts{})
	s.mem.Remember(s.tenant, "second memory text", memory.RememberOpts{})
	s.mem.Forget(s.tenant, id1)
	// an access event must NOT appear in the journal feed
	s.reg.Record(s.tenant, "/some/path", "Read")

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
	// 2 Memory + 1 Forget = 3 semantic events; the Access is excluded.
	if len(out.Events) != 3 {
		t.Fatalf("want 3 semantic events, got %d: %+v", len(out.Events), out.Events)
	}
	var types []string
	for _, e := range out.Events {
		types = append(types, e.Type)
		if e.Type == "Access" {
			t.Fatal("Access events must be excluded from the journal feed")
		}
	}
	// ascending seq order: Memory, Memory, Forget
	if types[0] != "Memory" || types[2] != "Forget" {
		t.Fatalf("unexpected types/order: %v", types)
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
```
Confirm `memory.Store.Forget(tenant, id)` and `Registry.Record(tenant, path, source)` signatures match (they're `(bool,error)` and `(uint64,Seq,error)`); adjust the discards if needed. Confirm imports `memory`, `comms` present in the test file (add if missing — `comms` likely is not; add `"matrixsentry/comms"` only if the test references it, which it doesn't, so probably just `memory`).

Run `go test ./cmd/sentrymcp/ -run TestAdminJournal -v` → FAIL (handleAdminJournal undefined).

- [ ] **Step 2: implement the handler + route**

Add to `main.go` near `handleAdminCorpus`:

```go
// handleAdminJournal serves a tenant's recent SEMANTIC journal events
// (memory writes, tombstones, agent messages) for the dashboard's journal panel.
// Access/PathMap records are excluded (bulk telemetry, no cheap id→path). Auth +
// tenant via resolveTenant. Server-to-server only.
func (s *server) handleAdminJournal(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.resolveTenant(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	limit := 60
	if q := r.URL.Query().Get("limit"); q != "" {
		if v, err := strconv.Atoi(q); err == nil && v > 0 {
			limit = v
		}
	}
	if limit > 200 {
		limit = 200
	}
	type ev struct {
		Seq  uint64 `json:"seq"`
		TS   int64  `json:"ts"`
		Type string `json:"type"`
		Text string `json:"text"`
	}
	events := make([]ev, 0, limit)
	t := tenant
	s.store.Scan(sentry.Filter{Tenant: &t}, func(rec sentry.Record) bool {
		var e ev
		e.Seq = uint64(rec.Seq)
		e.TS = rec.Tstamp / 1e6 // ns → ms
		switch rec.Type {
		case memory.EventMemory:
			var p memory.MemoryPayload
			if sentry.UnmarshalPayload(rec.Payload, &p) != nil {
				return true
			}
			e.Type = "Memory"
			e.Text = fmt.Sprintf("#%d %s", p.ID, truncRunes(p.Text, 80))
		case memory.EventForget:
			var p memory.ForgetPayload
			if sentry.UnmarshalPayload(rec.Payload, &p) != nil {
				return true
			}
			e.Type = "Forget"
			e.Text = fmt.Sprintf("tombstone #%d", p.ID)
		case comms.EventMessage:
			var p comms.MessagePayload
			if sentry.UnmarshalPayload(rec.Payload, &p) != nil {
				return true
			}
			e.Type = "Message"
			tgt := ""
			if p.Target != "" {
				tgt = " → " + p.Target
			}
			e.Text = fmt.Sprintf("%s%s @%s: %s", p.From, tgt, p.Area, truncRunes(p.Text, 60))
		default:
			return true // skip Access / PathMap
		}
		events = append(events, e)
		if len(events) > limit {
			events = events[len(events)-limit:] // keep last N
		}
		return true
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Events []ev `json:"events"`
	}{Events: events})
}

// truncRunes shortens s to n runes, appending an ellipsis when cut.
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
```

Register in `serveHTTP` next to `/admin/corpus`:
```go
	mux.HandleFunc("/admin/journal", s.handleAdminJournal)
```
Ensure imports: `strconv`, `matrixsentry/comms` (add to main.go's import block — `memory` and `sentry` are already imported). Check `truncRunes` doesn't collide with an existing helper; if `_trunc`/`trunc` already exists in main.go reuse it instead.

Run `go test ./cmd/sentrymcp/ -run TestAdminJournal -v` → PASS. Then `go build ./... && go test ./... -race`.

- [ ] **Step 3: commit**
```bash
git add cmd/sentrymcp/main.go cmd/sentrymcp/main_test.go
git commit -m "feat(sentrymcp): GET /admin/journal — tenant's recent semantic events for the dashboard"
```

---

### Task 2: `sentryadmin GET /api/journal`

**Files:** Modify `cmd/sentryadmin/api.go`, `cmd/sentryadmin/main.go`, `cmd/sentryadmin/api_test.go`.

- [ ] **Step 1: failing test (append to api_test.go)**

```go
func TestAPIJournalPassThrough(t *testing.T) {
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/journal" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"events":[{"seq":7,"ts":1,"type":"Memory","text":"#7 hi"}]}`))
	}))
	defer mcp.Close()
	srv := &apiServer{mcpURL: mcp.URL, token: "x", client: http.DefaultClient}
	rec := httptest.NewRecorder()
	srv.handleJournal(rec, httptest.NewRequest(http.MethodGet, "/api/journal?tenant=personal", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var d struct {
		Events []struct {
			Seq  int    `json:"seq"`
			Type string `json:"type"`
		} `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Events) != 1 || d.Events[0].Seq != 7 || d.Events[0].Type != "Memory" {
		t.Fatalf("passthrough wrong: %+v", d.Events)
	}
}

func TestAPIJournalUpstreamErrorIs502(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	srv := &apiServer{mcpURL: bad.URL, token: "x", client: http.DefaultClient}
	rec := httptest.NewRecorder()
	srv.handleJournal(rec, httptest.NewRequest(http.MethodGet, "/api/journal?tenant=personal", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rec.Code)
	}
}
```
Run `go test ./cmd/sentryadmin/ -run TestAPIJournal -v` → FAIL.

- [ ] **Step 2: implement `handleJournal` in api.go**

```go
// handleJournal proxies the tenant's recent semantic journal events from the
// MCP (server-side, with the bearer) straight through to the dashboard.
func (a *apiServer) handleJournal(w http.ResponseWriter, r *http.Request) {
	limit := r.URL.Query().Get("limit")
	url := a.mcpURL + "/admin/journal"
	if limit != "" {
		url += "?limit=" + limit
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadGateway)
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
		http.Error(w, `{"error":"upstream"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	io.Copy(w, resp.Body)
}
```
Add `"io"` to api.go imports.

- [ ] **Step 3: wire the route in main.go** — inside the `if mcpURL != "" {` block (next to `/api/galaxy`), gated by basicAuth:
```go
		mux.Handle("/api/journal", basicAuth(user, pass, http.HandlerFunc(api.handleJournal)))
```

Run `go test ./cmd/sentryadmin/ -run TestAPIJournal -v` → PASS. Then `go build ./... && go test ./... -race`, `go vet ./cmd/sentryadmin/`.

- [ ] **Step 4: commit**
```bash
git add cmd/sentryadmin/api.go cmd/sentryadmin/main.go cmd/sentryadmin/api_test.go
git commit -m "feat(sentryadmin): /api/journal — gated passthrough of the MCP semantic journal"
```

---

### Task 3: frontend — seed + poll the real journal

**Files:** Modify `cmd/sentryadmin/assets/live.js`, `cmd/sentryadmin/assets/index.html`.

- [ ] **Step 1: extend `live.js`** — in `prime`, after caching galaxy+comms, also fetch the journal; add accessors. Replace the body of `prime`'s try-block fetch section to also do:

```javascript
        try {
          const rj = await fetch("/api/journal?tenant=" + encodeURIComponent(tenantKey) + "&limit=60");
          if (rj.ok) journalCache[tenantKey] = (await rj.json()).events || [];
        } catch (e) { /* journal optional */ }
```
Add `const journalCache = {};` next to the other caches. Add to the `window.MatrixLive` object (alongside `prime`/`_patch`):
```javascript
    journalEvents(tenantKey) {
      const e = journalCache[tenantKey];
      return Array.isArray(e) && e.length ? e : null;
    },
    async fetchJournal(tenantKey) {
      try {
        const r = await fetch("/api/journal?tenant=" + encodeURIComponent(tenantKey) + "&limit=60");
        if (!r.ok) return null;
        const e = (await r.json()).events || [];
        journalCache[tenantKey] = e;
        return e;
      } catch (x) { return null; }
    },
```

- [ ] **Step 2: edit `index.html` `_seedJournal`** — replace:
```javascript
  _seedJournal() {
    for (let i = 0; i < 8; i++) this.randomEvent(true);
  }
```
with:
```javascript
  _seedJournal() {
    const live = window.MatrixLive && window.MatrixLive.journalEvents(this.state.tenant);
    if (live && live.length) {
      this._jSeq = 0;
      live.forEach((ev) => { this.pushEvent(ev.type, ev.text, true); if (ev.seq > this._jSeq) this._jSeq = ev.seq; });
      return;
    }
    this._jSeq = null;
    for (let i = 0; i < 8; i++) this.randomEvent(true);
  }
```

- [ ] **Step 3: edit `index.html` `_tickJournal`** — replace:
```javascript
  _tickJournal() {
    if (this._dead) return;
    this.randomEvent(false);
    this._jt = setTimeout(() => this._tickJournal(), 1000 + Math.random() * 1500);
  }
```
with:
```javascript
  _tickJournal() {
    if (this._dead) return;
    if (this._jSeq != null && window.MatrixLive) {
      window.MatrixLive.fetchJournal(this.state.tenant).then((events) => {
        if (this._dead || !events) return;
        events.forEach((ev) => {
          if (ev.seq > this._jSeq) { this.pushEvent(ev.type, ev.text, false); this._jSeq = ev.seq; }
        });
      });
    } else {
      this.randomEvent(false);
    }
    this._jt = setTimeout(() => this._tickJournal(), 3000 + Math.random() * 2000);
  }
```
(Slightly slower cadence for the live poll; fine for the sim path too.)

- [ ] **Step 4: reset `_jSeq` on tenant switch** — in `selectTenant`'s `.then(...)` body (the normal branch, where it already re-primes and calls generate/setState), before re-seeding the journal, the simplest is: the `.then` already calls `this._seedJournal` if it did before — check. If `selectTenant` does NOT currently call `_seedJournal`, add `this._jSeq = null;` at the start of the `.then` body so `_seedJournal` (called via the existing flow) re-evaluates; if `selectTenant` never re-seeds the journal, add `this._seedJournal();` inside the `.then` after `setData`. Read `selectTenant` and make the minimal change so a tenant switch re-seeds the journal from the (re-primed) live cache. Keep existing statements intact.

- [ ] **Step 5: build + verify mock fallback (live OFF)**
```bash
go build -o /tmp/sentryadmin ./cmd/sentryadmin
(/tmp/sentryadmin -http 127.0.0.1:8957 >/tmp/sa.log 2>&1 &) ; sleep 1
curl -s -o /dev/null -w "index:%{http_code}\n" http://127.0.0.1:8957/
pkill -f "sentryadmin -http 127.0.0.1:8957"
```
Index must be 200. (Live OFF → journalEvents null → synthetic journal still runs. The controller will Playwright-verify the live path.) `node --check cmd/sentryadmin/assets/live.js`.

- [ ] **Step 6: commit**
```bash
git add cmd/sentryadmin/assets/live.js cmd/sentryadmin/assets/index.html
git commit -m "feat(sentryadmin): live journal — seed + poll real events, synthetic fallback"
```

---

### Task 4: Deploy + verify

**Files:** none (operational).

- [ ] **Step 1: rebuild + redeploy sentrymcp (8808 + 8809)**
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/sentrymcp ./cmd/sentrymcp
scp -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes /tmp/sentrymcp root@10.10.10.96:/root/sentrymcp.new
ssh matrix-sentry 'mv /root/sentrymcp /root/sentrymcp.old && mv /root/sentrymcp.new /root/sentrymcp && chmod +x /root/sentrymcp && systemctl restart sentrymcp && sleep 2 && systemctl is-active sentrymcp'
scp -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes /tmp/sentrymcp root@10.10.10.175:/root/sentrymcp.new
ssh matrix-sentry2 'mv /root/sentrymcp /root/sentrymcp.old && mv /root/sentrymcp.new /root/sentrymcp && chmod +x /root/sentrymcp && systemctl restart sentrymcp-mt && sleep 2 && systemctl is-active sentrymcp-mt'
```

- [ ] **Step 2: rebuild + redeploy sentryadmin (server2, stop before overwrite — binary is busy)**
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/sentryadmin ./cmd/sentryadmin
scp -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes /tmp/sentryadmin root@10.10.10.175:/root/sentryadmin.new
ssh matrix-sentry2 'systemctl stop sentryadmin && mv /root/sentryadmin.new /root/sentryadmin && chmod +x /root/sentryadmin && systemctl start sentryadmin && sleep 1 && systemctl is-active sentryadmin'
```

- [ ] **Step 3: verify real journal (controller, Playwright)** — run a local mac `sentryadmin` against the real 8808 (token read on-server, not echoed), load in Playwright, confirm 0 console errors and the Journal panel shows real Memory events (real memory ids/text like "#198 …"), not the fabricated "agent-X recall →" lines. Also spot-check `/api/journal` returns real events.

- [ ] **Step 4: update HANDOFF + commit**
Add a "LIVE JOURNAL (v2.1)" note under the dashboard section: `/admin/journal` (semantic events only, Access excluded), `/api/journal` gated proxy, frontend seed+poll with synthetic fallback. Commit.
```bash
git add HANDOFF.md && git commit -m "docs: admin live journal (v2.1) deployed — real semantic events in the panel"
```
