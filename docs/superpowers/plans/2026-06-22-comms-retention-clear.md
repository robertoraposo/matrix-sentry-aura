# Comms Retention + comms_clear Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Bound the comms in-RAM index with retention (count ∩ time) and add a `comms_clear(area)` tombstone sweep, without ever touching the append-only journal.

**Architecture:** `comms.Store` gains retention fields + `prune`, an `EventCommsClear` tombstone + `Clear`, and a two-pass `New` (messages, then clears); `cmd/sentrymcp` configures retention from env + exposes the `comms_clear` tool.

**Tech Stack:** Go (comms.Store over sentry journal, MCP server).

---

### Task 1: retention on the comms in-RAM index

**Files:** Modify `comms/comms.go`, `comms/comms_test.go`.

CONTEXT: `comms.Store{journal, mu sync.Mutex, entries []Message}`; `entries` is seq-ascending (append order). `Message` has `Seq uint64`, `TS int64` (nanoseconds). `Post` appends then (currently) returns. `time` is imported.

- [ ] **Step 1: failing test (append to comms/comms_test.go)**

```go
func TestRetentionCountAndTime(t *testing.T) {
	st := newTestStore(t) // real helper name from the file
	// count: post 5, retain last 3 (age off) → 3 newest
	for i := 0; i < 5; i++ {
		st.Post(1, MessagePayload{Area: "x", From: "a", Text: "m"})
	}
	st.SetRetention(3, 0)
	if got := len(st.Recent(1, 100)); got != 3 {
		t.Fatalf("count retention: want 3, got %d", got)
	}
	// time: craft entries directly and prune by age
	now := time.Now().UnixNano()
	st2 := newTestStore(t)
	st2.SetRetention(0, time.Hour) // time only
	st2.entries = []Message{
		{Seq: 1, Tenant: 1, TS: now - 2*int64(time.Hour), Area: "x", Text: "old"},
		{Seq: 2, Tenant: 1, TS: now - 10*int64(time.Minute), Area: "x", Text: "fresh"},
	}
	st2.pruneAt(now)
	got := st2.Recent(1, 100)
	if len(got) != 1 || got[0].Text != "fresh" {
		t.Fatalf("time retention: want only 'fresh', got %+v", got)
	}
	// both: a message must pass count AND time
	st3 := newTestStore(t)
	st3.SetRetention(1, time.Hour)
	st3.entries = []Message{
		{Seq: 1, Tenant: 1, TS: now - 10*int64(time.Minute), Area: "x", Text: "older-fresh"},
		{Seq: 2, Tenant: 1, TS: now - 5*int64(time.Minute), Area: "x", Text: "newest"},
	}
	st3.pruneAt(now)
	if g := st3.Recent(1, 100); len(g) != 1 || g[0].Text != "newest" {
		t.Fatalf("count∩time: want only 'newest', got %+v", g)
	}
}
```
(`pruneAt` and `entries` are package-internal — test is `package comms`.) Run `go test ./comms/ -run TestRetention -v` → FAIL.

- [ ] **Step 2: implement (comms/comms.go)**

Add fields to `Store`:
```go
	retainN   int           // keep at most the last N messages in the index (0 = off)
	retainAge time.Duration // drop messages older than this from the index (0 = off)
```
Add methods:
```go
// SetRetention bounds the in-RAM index: keep only messages that are BOTH within
// the last n (0 = off) AND newer than age (0 = off). The journal is untouched
// (audit). Applied here and after every Post. (Global across tenants — RAM bound.)
func (s *Store) SetRetention(n int, age time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retainN = n
	s.retainAge = age
	s.pruneAt(time.Now().UnixNano())
}

// pruneAt drops index entries failing either retention knob. Caller holds s.mu.
func (s *Store) pruneAt(now int64) {
	if s.retainN <= 0 && s.retainAge <= 0 {
		return
	}
	start := 0
	if s.retainN > 0 && len(s.entries) > s.retainN {
		start = len(s.entries) - s.retainN // count: keep the tail
	}
	cutoff := int64(0)
	if s.retainAge > 0 {
		cutoff = now - int64(s.retainAge)
	}
	out := make([]Message, 0, len(s.entries)-start)
	for _, m := range s.entries[start:] {
		if cutoff > 0 && m.TS < cutoff {
			continue // time
		}
		out = append(out, m)
	}
	s.entries = out
}
```
In `Post`, before `return uint64(seq), nil`, add `s.pruneAt(time.Now().UnixNano())` (s.mu already held).

Run `go test ./comms/ -run TestRetention -v` → PASS. `go test ./comms/ -race`.

- [ ] **Step 3: commit**
```bash
git add comms/comms.go comms/comms_test.go
git commit -m "feat(comms): in-RAM retention (count ∩ time) via SetRetention — bound the channel, journal untouched"
```

---

### Task 2: `comms_clear` via EventCommsClear tombstone

**Files:** Modify `comms/comms.go`, `comms/comms_test.go`.

CONTEXT: `New` currently single-scans `EventMessage`. `EventMessage=5`, `memory.EventRecall=6` → use `7`. `Store.Clear` mirrors memory's forget (append tombstone + drop from index; rebuild replays).

- [ ] **Step 1: failing test (append to comms/comms_test.go)**

```go
func TestClearAreaTombstoneSurvivesReopen(t *testing.T) {
	st, dir := newTestStoreDir(t) // a helper that returns the store + its journal dir (see existing reopen tests)
	st.Post(1, MessagePayload{Area: "X", From: "a", Text: "x1"})
	st.Post(1, MessagePayload{Area: "X", From: "a", Text: "x2"})
	st.Post(1, MessagePayload{Area: "Y", From: "a", Text: "y1"})
	st.Post(2, MessagePayload{Area: "X", From: "z", Text: "other-tenant"})

	cleared, err := st.Clear(1, "X")
	if err != nil { t.Fatal(err) }
	if cleared != 2 { t.Fatalf("want 2 cleared, got %d", cleared) }
	if len(st.Read(1, "X", 0)) != 0 { t.Fatal("X not cleared in index") }
	if len(st.Read(1, "Y", 0)) != 1 { t.Fatal("Y must be untouched") }
	if len(st.Read(2, "X", 0)) != 1 { t.Fatal("tenant 2's X must be untouched") }

	// a post AFTER the clear survives
	st.Post(1, MessagePayload{Area: "X", From: "a", Text: "x3-after"})
	if g := st.Read(1, "X", 0); len(g) != 1 || g[0].Text != "x3-after" {
		t.Fatalf("post-clear message should survive: %+v", g)
	}

	// reopen: the EventCommsClear must replay → X still cleared, x3-after kept
	jr, err := sentry.Open(dir, sentry.Options{})
	if err != nil { t.Fatal(err) }
	defer jr.Close()
	re, err := New(jr)
	if err != nil { t.Fatal(err) }
	if g := re.Read(1, "X", 0); len(g) != 1 || g[0].Text != "x3-after" {
		t.Fatalf("after reopen, X should have only x3-after: %+v", g)
	}
	if len(re.Read(1, "Y", 0)) != 1 { t.Fatal("Y lost on reopen") }
}
```
ADAPT to the real test helpers: find how comms tests construct a store + get the dir for reopen (e.g. `newTestStore` returns `(*Store, string)` — earlier tasks used that). Use the real signatures. Run `go test ./comms/ -run TestClear -v` → FAIL.

- [ ] **Step 2: implement (comms/comms.go)**

```go
// EventCommsClear tombstones an area: on rebuild, messages in that area+tenant
// posted before this record are dropped from the index (the journal keeps them).
const EventCommsClear sentry.EventType = 7

// ClearPayload names the area to sweep.
type ClearPayload struct {
	Area string `json:"area"`
}

// Clear removes a tenant's messages in area from the live index by appending an
// EventCommsClear tombstone (the journal record is retained). Messages posted to
// the area AFTER the clear survive. Returns how many were dropped.
func (s *Store) Clear(tenant sentry.TenantID, area string) (int, error) {
	if area == "" {
		return 0, fmt.Errorf("comms: area required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, err := s.journal.Append(tenant, EventCommsClear, ClearPayload{Area: area})
	if err != nil {
		return 0, fmt.Errorf("comms: append clear: %w", err)
	}
	cut := uint64(seq)
	out := s.entries[:0:0]
	dropped := 0
	for _, m := range s.entries {
		if m.Tenant == tenant && m.Area == area && m.Seq < cut {
			dropped++
			continue
		}
		out = append(out, m)
	}
	s.entries = out
	return dropped, nil
}
```
Rewrite `New` to two passes + apply clears (keep pass 1 as-is; add pass 2):
```go
func New(journal *sentry.Store) (*Store, error) {
	s := &Store{journal: journal}
	etype := EventMessage
	var scanErr error
	if err := journal.Scan(sentry.Filter{Type: &etype}, func(r sentry.Record) bool {
		var p MessagePayload
		if err := sentry.UnmarshalPayload(r.Payload, &p); err != nil {
			scanErr = fmt.Errorf("comms: decode record seq %d: %w", r.Seq, err)
			return false
		}
		s.entries = append(s.entries, message(uint64(r.Seq), r.Tenant, r.Tstamp, p))
		return true
	}); err != nil {
		return nil, err
	}
	if scanErr != nil {
		return nil, scanErr
	}
	// Pass 2: apply area-clear tombstones (drop messages in cleared area+tenant
	// posted before the clear). A clear at seq S clears area messages with seq < S.
	ctype := EventCommsClear
	if err := journal.Scan(sentry.Filter{Type: &ctype}, func(r sentry.Record) bool {
		var cp ClearPayload
		if err := sentry.UnmarshalPayload(r.Payload, &cp); err != nil {
			scanErr = fmt.Errorf("comms: decode clear seq %d: %w", r.Seq, err)
			return false
		}
		cut := uint64(r.Seq)
		out := s.entries[:0]
		for _, m := range s.entries {
			if m.Tenant == r.Tenant && m.Area == cp.Area && m.Seq < cut {
				continue
			}
			out = append(out, m)
		}
		s.entries = out
		return true
	}); err != nil {
		return nil, err
	}
	if scanErr != nil {
		return nil, scanErr
	}
	return s, nil
}
```
Run `go test ./comms/ -run TestClear -v` → PASS. Then `go test ./comms/ -race` (retention test still green).

- [ ] **Step 3: commit**
```bash
git add comms/comms.go comms/comms_test.go
git commit -m "feat(comms): comms_clear via EventCommsClear tombstone — sweep an area, survives reopen"
```

---

### Task 3: wire retention env + `comms_clear` tool (cmd/sentrymcp)

**Files:** Modify `cmd/sentrymcp/main.go`, `cmd/sentrymcp/main_test.go`.

CONTEXT: `s.chat, err = comms.New(store)` at ~line 117. `envFloat`/`envOr` exist (~971); add `envInt`. The `read`/`post` tool defs + handlers are the pattern. Dispatch passes `tenant` to `callTool`.

- [ ] **Step 1: configure retention after building s.chat**

Add `envInt` helper near `envFloat`:
```go
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
```
After `s.chat, err = comms.New(store)` (and its error check), add:
```go
	s.chat.SetRetention(envInt("SENTRY_COMMS_RETAIN_N", 2000), time.Duration(envInt("SENTRY_COMMS_RETAIN_DAYS", 14))*24*time.Hour)
```
(`time` + `strconv` already imported.)

- [ ] **Step 2: failing test for the tool (append to main_test.go)**

```go
func TestCommsClearTool(t *testing.T) {
	s := newMemServer(t)
	if s.chat == nil { s.chat, _ = comms.New(s.store) }
	s.chat.Post(s.tenant, comms.MessagePayload{Area: "build", From: "a", Text: "m1"})
	s.chat.Post(s.tenant, comms.MessagePayload{Area: "build", From: "a", Text: "m2"})
	resp := callNamed(s, "comms_clear", map[string]any{"area": "build"})
	text := respText(t, resp) // real extractor
	if !strings.Contains(text, "2") {
		t.Fatalf("comms_clear should report 2 cleared, got: %s", text)
	}
	if len(s.chat.Read(s.tenant, "build", 0)) != 0 {
		t.Fatal("area not cleared")
	}
}
```
Run `go test ./cmd/sentrymcp/ -run TestCommsClear -v` → FAIL.

- [ ] **Step 3: add the tool def + handler**

Tool def (near the `post`/`read` defs):
```go
		{
			"name":        "comms_clear",
			"description": "Sweep a FINISHED coordination area: drops its messages from the live channel (the durable journal is retained for audit). Use when an area's work is done; promote anything worth keeping to memory first. Tenant-scoped.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"area": map[string]any{"type": "string", "description": "the area to clear"}},
				"required":   []any{"area"},
			},
		},
```
Handler (near `case "read":`):
```go
	case "comms_clear":
		area, _ := strArg(p.Args, "area")
		if area == "" {
			return s.toolErr(req.ID, "provide 'area' to clear")
		}
		n, err := s.chat.Clear(tenant, area)
		if err != nil {
			return s.toolErr(req.ID, "comms_clear failed: "+err.Error())
		}
		return s.toolText(req.ID, fmt.Sprintf("cleared %d message(s) from area %q (journal retained)", n, area))
```

Run `go test ./cmd/sentrymcp/ -run TestCommsClear -v` → PASS. Then `go build ./... && go test ./... -race`, `go vet ./cmd/sentrymcp/`.

- [ ] **Step 4: commit**
```bash
git add cmd/sentrymcp/main.go cmd/sentrymcp/main_test.go
git commit -m "feat(sentrymcp): comms retention env (N=2000,14d) + comms_clear tool"
```

---

### Task 4: deploy + verify

**Files:** none (operational).

- [ ] **Step 1: rebuild + redeploy 8808 + 8809**
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/sentrymcp ./cmd/sentrymcp
scp -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes /tmp/sentrymcp root@10.10.10.96:/root/sentrymcp.new
ssh matrix-sentry 'mv /root/sentrymcp /root/sentrymcp.old && mv /root/sentrymcp.new /root/sentrymcp && chmod +x /root/sentrymcp && systemctl restart sentrymcp && sleep 2 && systemctl is-active sentrymcp'
scp -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes /tmp/sentrymcp root@10.10.10.175:/root/sentrymcp.new
ssh matrix-sentry2 'mv /root/sentrymcp /root/sentrymcp.old && mv /root/sentrymcp.new /root/sentrymcp && chmod +x /root/sentrymcp && systemctl restart sentrymcp-mt && sleep 2 && systemctl is-active sentrymcp-mt'
```

- [ ] **Step 2: verify** — on 8808 with the token: `/admin/comms` still returns recent (retention default keeps the 605, all recent); create a throwaway area via `post`, then `comms_clear` it → `read` returns empty; confirm the live channel is bounded (retention active) and the journal record count is unchanged (clear is additive — a tombstone, not a delete).

- [ ] **Step 3: update HANDOFF + the comms note; commit**
```bash
git add HANDOFF.md && git commit -m "docs: comms retention (2000/14d) + comms_clear deployed"
```
