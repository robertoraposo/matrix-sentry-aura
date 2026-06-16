# Recall Observability (#4 v1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Journal every recall (EventRecall) and add an `analyze_recall` tool, so recall coverage + the real query distribution are measurable per tenant.

**Architecture:** New `memory.EventRecall`/`RecallPayload`; the MCP recall handler appends it best-effort (gated by `SENTRY_RECALL_LOG`); `analyze_recall` scans + reports coverage stats. `memory.Store.Recall` stays a pure read.

**Tech Stack:** Go (sentry journal Scan/Append, encoding/json, sort), existing MCP server.

---

### Task 1: `memory.EventRecall` + payload + round-trip test

**Files:** Modify `memory/memory.go`; Test `memory/memory_test.go`.

- [ ] **Step 1: failing test (append to memory/memory_test.go)**

```go
func TestRecallPayloadRoundTrip(t *testing.T) {
	p := RecallPayload{Query: "deploy oauth", K: 5, Hits: []RecallHit{{ID: 7, Dist: 1.1}, {ID: 9, Dist: 1.4}}}
	b, err := sentry.MarshalPayload(p)
	if err != nil {
		t.Fatal(err)
	}
	var got RecallPayload
	if err := sentry.UnmarshalPayload(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Query != "deploy oauth" || got.K != 5 || len(got.Hits) != 2 || got.Hits[0].ID != 7 || got.Hits[1].Dist != 1.4 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if EventRecall == EventMemory || EventRecall == EventForget {
		t.Fatal("EventRecall must be a distinct event type")
	}
}
```
Run `go test ./memory/ -run TestRecallPayload -v` → FAIL (undefined).

- [ ] **Step 2: implement (add to memory/memory.go near EventMemory/EventForget consts)**

```go
// EventRecall journals a recall query and its hits — observability only (never
// replayed by New; it is telemetry, not state).
const EventRecall sentry.EventType = 6

// RecallHit is one returned memory id + its squared-L2 distance to the query.
type RecallHit struct {
	ID   uint64  `json:"id"`
	Dist float32 `json:"dist"`
}

// RecallPayload is the persisted form of a recall: the query, k, and the hits.
type RecallPayload struct {
	Query string      `json:"q"`
	K     int         `json:"k"`
	Hits  []RecallHit `json:"hits"`
}
```
Run the test → PASS. `go test ./memory/ -race`.

- [ ] **Step 3: commit**
```bash
git add memory/memory.go memory/memory_test.go
git commit -m "feat(memory): EventRecall + RecallPayload — recall observability event"
```

---

### Task 2: log recalls + `SENTRY_RECALL_LOG` (cmd/sentrymcp)

**Files:** Modify `cmd/sentrymcp/main.go`; Test `cmd/sentrymcp/main_test.go`.

CONTEXT: `server` struct (~line 60) has `store *sentry.Store`, `mem *memory.Store`. The `recall` handler (~635) does `hits, err := s.mem.Recall(tenant, query, k)`, where `hits []memory.Memory` (each has `.ID` and `.Score`). `envFloat`/`envOr` helpers exist (~795); there is NO `envBool`. `newMemServer(t)` builds a test server with `s.mem` + `s.store`.

- [ ] **Step 1: failing test (append to cmd/sentrymcp/main_test.go)**

```go
func TestRecallIsJournaledWhenEnabled(t *testing.T) {
	s := newMemServer(t)
	s.logRecall = true
	s.mem.Remember(s.tenant, "deploy on fridays", memory.RememberOpts{})

	// invoke the recall tool through dispatch/callTool the way other tool tests do
	req := jsonRPCReq("tools/call", map[string]any{"name": "recall", "arguments": map[string]any{"query": "deploy on fridays", "k": 3}})
	s.callTool(req, s.tenant)

	// a single EventRecall must be in the journal for this tenant
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
	req := jsonRPCReq("tools/call", map[string]any{"name": "recall", "arguments": map[string]any{"query": "x", "k": 3}})
	s.callTool(req, s.tenant)
	et := memory.EventRecall
	n := 0
	s.store.Scan(sentry.Filter{Tenant: &s.tenant, Type: &et}, func(r sentry.Record) bool { n++; return true })
	if n != 0 {
		t.Fatalf("logging disabled but %d EventRecall written", n)
	}
}
```
IMPORTANT: read `cmd/sentrymcp/main_test.go` for the REAL way tests invoke a tool — there may be a helper to build a JSON-RPC request (the snippet's `jsonRPCReq` is a placeholder) and the real `callTool`/`dispatch` signature (earlier work used `callTool(req, tenant)`). Match the existing test invocation pattern EXACTLY (how other tool handlers like remember/forget are tested). If tests call the handler via `dispatch` or a request struct, mirror that.

Run `go test ./cmd/sentrymcp/ -run TestRecallIs -v` → FAIL (s.logRecall undefined).

- [ ] **Step 2: implement**

Add `logRecall bool` to the `server` struct. In `main()`, after building `s` (near where `s.tenant` etc. are set), set `s.logRecall = envBool("SENTRY_RECALL_LOG", true)`. Add the helper near `envFloat`:
```go
func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v != "0" && strings.ToLower(v) != "false"
}
```
(Ensure `strings` is imported — it is.)

In the `recall` handler, after the successful `hits, err := s.mem.Recall(...)` block (after the `if err != nil` return, before `formatRecall`), add:
```go
		if s.logRecall {
			rp := memory.RecallPayload{Query: query, K: k, Hits: make([]memory.RecallHit, len(hits))}
			for i, h := range hits {
				rp.Hits[i] = memory.RecallHit{ID: h.ID, Dist: h.Score}
			}
			if _, aerr := s.store.Append(tenant, memory.EventRecall, rp); aerr != nil {
				s.moko.Info("recall-log failed", map[string]string{"tenant": fmt.Sprint(tenant), "err": aerr.Error()})
			}
		}
```

Run `go test ./cmd/sentrymcp/ -run TestRecall -v` → PASS. Then `go build ./... && go test ./... -race`.

- [ ] **Step 3: commit**
```bash
git add cmd/sentrymcp/main.go cmd/sentrymcp/main_test.go
git commit -m "feat(sentrymcp): journal recalls as EventRecall (SENTRY_RECALL_LOG, best-effort)"
```

---

### Task 3: `analyze_recall` tool (cmd/sentrymcp)

**Files:** Modify `cmd/sentrymcp/main.go`; Test `cmd/sentrymcp/main_test.go`.

- [ ] **Step 1: failing test (append to main_test.go)**

```go
func TestAnalyzeRecallReportsCoverage(t *testing.T) {
	s := newMemServer(t)
	s.logRecall = true
	s.mem.Remember(s.tenant, "deploy on fridays", memory.RememberOpts{})
	for _, q := range []string{"deploy on fridays", "something unrelated zzz"} {
		req := jsonRPCReq("tools/call", map[string]any{"name": "recall", "arguments": map[string]any{"query": q, "k": 3}})
		s.callTool(req, s.tenant)
	}
	req := jsonRPCReq("tools/call", map[string]any{"name": "analyze_recall", "arguments": map[string]any{}})
	resp := s.callTool(req, s.tenant)
	text := toolText(resp) // use the same extraction other analyze tests use
	if !strings.Contains(text, "total=2") {
		t.Fatalf("analyze_recall should report total=2, got: %s", text)
	}
	if !strings.Contains(text, "deploy on fridays") {
		t.Fatalf("analyze_recall should echo a recent query, got: %s", text)
	}
}
```
Match the real tool-call + response-text extraction pattern from existing analyze/recall tests (the placeholder `jsonRPCReq`/`toolText` must be the file's real helpers — read main_test.go).

Run `go test ./cmd/sentrymcp/ -run TestAnalyzeRecall -v` → FAIL.

- [ ] **Step 2: add the tool definition + handler**

Add to the tools list (near the `analyze_access` def, ~line 459):
```go
		{
			"name":        "analyze_recall",
			"description": "Measure recall coverage for this tenant: how many recalls have run, the top-hit distance distribution (a high top-distance means recall found nothing relevant — a coverage gap), and the most recent real queries.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
```
Add the handler case (near `analyze_access`):
```go
	case "analyze_recall":
		et := memory.EventRecall
		var tops []float64
		empty, total := 0, 0
		var recent []string
		tnt := tenant
		s.store.Scan(sentry.Filter{Tenant: &tnt, Type: &et}, func(r sentry.Record) bool {
			var p memory.RecallPayload
			if sentry.UnmarshalPayload(r.Payload, &p) != nil {
				return true
			}
			total++
			if len(p.Hits) == 0 {
				empty++
			} else {
				tops = append(tops, float64(p.Hits[0].Dist))
			}
			q := p.Query
			if len(q) > 40 {
				q = q[:40]
			}
			recent = append(recent, q)
			return true
		})
		if total == 0 {
			return s.toolText(req.ID, fmt.Sprintf("recall coverage (tenant %d): total=0 — no recalls logged yet", tenant))
		}
		sort.Float64s(tops)
		pct := func(f float64) float64 {
			if len(tops) == 0 {
				return 0
			}
			i := int(f * float64(len(tops)))
			if i >= len(tops) {
				i = len(tops) - 1
			}
			return tops[i]
		}
		tail := recent
		if len(tail) > 8 {
			tail = tail[len(tail)-8:]
		}
		return s.toolText(req.ID, fmt.Sprintf(
			"recall coverage (tenant %d): total=%d empty=%d  topDist min=%.3f p50=%.3f p90=%.3f max=%.3f\nrecent: %q",
			tenant, total, empty, pct(0), pct(0.5), pct(0.9), tops[len(tops)-1], tail))
```
Ensure `sort` is imported (it is, used by formatRecall/Recall? confirm; add if missing).

Run `go test ./cmd/sentrymcp/ -run TestAnalyzeRecall -v` → PASS. Then `go build ./... && go test ./... -race`, `go vet ./cmd/sentrymcp/`.

- [ ] **Step 3: commit**
```bash
git add cmd/sentrymcp/main.go cmd/sentrymcp/main_test.go
git commit -m "feat(sentrymcp): analyze_recall — recall coverage (top-dist dist + recent queries)"
```

---

### Task 4: deploy

**Files:** none (operational).

- [ ] **Step 1: rebuild + redeploy 8808 + 8809**
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/sentrymcp ./cmd/sentrymcp
scp -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes /tmp/sentrymcp root@10.10.10.96:/root/sentrymcp.new
ssh matrix-sentry 'mv /root/sentrymcp /root/sentrymcp.old && mv /root/sentrymcp.new /root/sentrymcp && chmod +x /root/sentrymcp && systemctl restart sentrymcp && sleep 2 && systemctl is-active sentrymcp'
scp -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes /tmp/sentrymcp root@10.10.10.175:/root/sentrymcp.new
ssh matrix-sentry2 'mv /root/sentrymcp /root/sentrymcp.old && mv /root/sentrymcp.new /root/sentrymcp && chmod +x /root/sentrymcp && systemctl restart sentrymcp-mt && sleep 2 && systemctl is-active sentrymcp-mt'
```

- [ ] **Step 2: verify** — on 8808 with the token: run a couple of `recall` tool calls, then `analyze_recall` → confirm `total` reflects them, topDist distribution prints, and recent queries echo. Confirm a `Scan` for EventRecall on the journal shows the new records.

- [ ] **Step 3: update HANDOFF + commit**
Add a "RECALL OBSERVABILITY (#4 v1)" note: EventRecall journaling (SENTRY_RECALL_LOG), analyze_recall coverage tool, deployed; next = calibrate a no-good-match floor from real top-dist + v2 LLM relevance judging over the log.
```bash
git add HANDOFF.md && git commit -m "docs: recall observability (#4 v1) deployed — EventRecall + analyze_recall"
```
