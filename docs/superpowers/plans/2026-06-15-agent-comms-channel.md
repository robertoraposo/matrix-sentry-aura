# Agent Comms Channel Implementation Plan (v1)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A pull-based, tenant-scoped agent communication channel on Matrix — agents `post` typed messages to an "area" and `read` new ones since a cursor, with `promote` to turn a message into durable memory — built as a separate `EventMessage` log (ordered, no dedup, no embedding), ready for a later SSE push layer.

**Architecture:** A new `comms` package wraps the existing SentryLog journal exactly like `memory.Store` does — appends `EventMessage` records and keeps an in-RAM index for cheap polling. `cmd/sentrymcp` exposes `post`/`read`/`promote` tools that route on the per-request tenant (reusing the multi-tenant `resolveTenant`). `promote` bridges to `memory.Remember`.

**Tech Stack:** Pure Go, zero deps. Spec: `docs/superpowers/specs/2026-06-15-agent-comms-channel-design.md`. New pkg `comms/`; `cmd/sentrymcp` wiring. Storage = the same journal; engine/memory untouched.

---

## File Structure

- **Create** `comms/comms.go` (+ `comms_test.go`) — `EventMessage`, `MessagePayload`, `Message`, `Store` (New/Post/Read/Get). Mirrors `memory.Store`'s journal-wrapping + in-RAM-index pattern. One responsibility: an ordered per-tenant message log.
- **Modify** `cmd/sentrymcp/main.go` (+ `main_test.go`) — `server.chat *comms.Store`; build it in `main()`; `post`/`read`/`promote` tool definitions + handlers (tenant-scoped).
- **Deploy (Task 3):** redeploy the live 8808 server (ADDITIVE — adds 3 tools; existing memory/recall/dedup behavior unchanged); clients re-list to see the new tools.

---

## Task 1: `comms` package (the message log)

**Files:** Create `comms/comms.go`, `comms/comms_test.go`

- [ ] **Step 1: Write the failing tests** → `comms/comms_test.go`

```go
package comms

import (
	"testing"

	"matrixsentry/sentry"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	j, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(j)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestPostReadRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	seq, err := s.Post(1, MessagePayload{Area: "proj/backend", From: "be", Text: "auth done"})
	if err != nil || seq == 0 {
		t.Fatalf("Post = (%d,%v)", seq, err)
	}
	got := s.Read(1, "proj/backend", 0)
	if len(got) != 1 || got[0].Text != "auth done" || got[0].From != "be" {
		t.Fatalf("Read = %+v", got)
	}
	if got[0].Kind != "note" { // default kind
		t.Fatalf("default kind = %q, want note", got[0].Kind)
	}
	if got[0].Seq != seq {
		t.Fatalf("msg seq %d != posted seq %d", got[0].Seq, seq)
	}
}

func TestReadSinceCursor(t *testing.T) {
	s, _ := newTestStore(t)
	s1, _ := s.Post(1, MessagePayload{Area: "a", From: "x", Text: "one"})
	s.Post(1, MessagePayload{Area: "a", From: "x", Text: "two"})
	s.Post(1, MessagePayload{Area: "a", From: "x", Text: "three"})
	got := s.Read(1, "a", s1) // only messages after the first
	if len(got) != 2 || got[0].Text != "two" || got[1].Text != "three" {
		t.Fatalf("since-cursor read = %+v", got)
	}
}

func TestAreaFilter(t *testing.T) {
	s, _ := newTestStore(t)
	s.Post(1, MessagePayload{Area: "a", From: "x", Text: "in a"})
	s.Post(1, MessagePayload{Area: "b", From: "x", Text: "in b"})
	got := s.Read(1, "a", 0)
	if len(got) != 1 || got[0].Text != "in a" {
		t.Fatalf("area filter leaked: %+v", got)
	}
}

func TestTenantIsolation(t *testing.T) {
	s, _ := newTestStore(t)
	s.Post(2, MessagePayload{Area: "a", From: "x", Text: "tenant 2 only"})
	if got := s.Read(1, "a", 0); len(got) != 0 {
		t.Fatalf("tenant 1 saw tenant 2's messages: %+v", got)
	}
	if got := s.Read(2, "a", 0); len(got) != 1 {
		t.Fatalf("tenant 2 read = %+v, want its 1 message", got)
	}
}

func TestNoDedup(t *testing.T) {
	s, _ := newTestStore(t)
	s.Post(1, MessagePayload{Area: "a", From: "x", Text: "same text"})
	s.Post(1, MessagePayload{Area: "a", From: "y", Text: "same text"})
	if got := s.Read(1, "a", 0); len(got) != 2 {
		t.Fatalf("comms must NOT dedup; got %d, want 2", len(got))
	}
}

func TestGet(t *testing.T) {
	s, _ := newTestStore(t)
	seq, _ := s.Post(1, MessagePayload{Area: "a", From: "x", Text: "find me"})
	if m, ok := s.Get(1, "a", seq); !ok || m.Text != "find me" {
		t.Fatalf("Get = %+v ok=%v", m, ok)
	}
	if _, ok := s.Get(1, "a", 999999); ok {
		t.Fatal("Get of missing seq should be false")
	}
	if _, ok := s.Get(2, "a", seq); ok {
		t.Fatal("Get across tenant should be false")
	}
}

func TestPostRequiresFields(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.Post(1, MessagePayload{Area: "a", From: "x"}); err == nil {
		t.Fatal("empty text must error")
	}
	if _, err := s.Post(1, MessagePayload{Area: "a", Text: "t"}); err == nil {
		t.Fatal("empty from must error")
	}
	if _, err := s.Post(1, MessagePayload{From: "x", Text: "t"}); err == nil {
		t.Fatal("empty area must error")
	}
}

func TestSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	j, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := New(j)
	s.Post(1, MessagePayload{Area: "a", From: "x", Text: "persist me"})
	j.Close()

	j2, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := New(j2)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.Read(1, "a", 0)
	if len(got) != 1 || got[0].Text != "persist me" {
		t.Fatalf("after reopen read = %+v", got)
	}
}
```

- [ ] **Step 2: Run, verify FAIL** — `go test ./comms/` → `undefined: New/Post/Read/Get/...`.

- [ ] **Step 3: Implement** → `comms/comms.go`

```go
// Package comms is Matrix Sentry's agent communication channel: an ordered,
// per-tenant message log built on the SentryLog journal — the coordination
// counterpart to the semantic memory in package memory. Unlike memory it does
// NOT embed or deduplicate: messages are a chronological stream agents poll by
// area. It mirrors memory.Store's journal-wrapping + in-RAM-index pattern so
// reads are a cheap in-RAM filter (5 agents polling every few seconds must be
// cheap), while the journal keeps the durable, append-only record.
package comms

import (
	"fmt"
	"sync"
	"time"

	"matrixsentry/sentry"
)

// EventMessage is the journal record type for a channel message (1 access,
// 2 pathmap, 3 memory, 4 forget are taken).
const EventMessage sentry.EventType = 5

// MessagePayload is the persisted form of one message.
type MessagePayload struct {
	Area   string `json:"area"`
	From   string `json:"from"`
	Kind   string `json:"kind,omitempty"`   // question|answer|info|note (default note)
	Text   string `json:"text"`
	Target string `json:"target,omitempty"` // agent label this is directed at ("" = broadcast)
	Ref    uint64 `json:"ref,omitempty"`    // journal seq of the message this replies to (0 = none)
}

// Message is a read result: the payload plus the journal seq (its id) and tenant/ts.
type Message struct {
	Seq    uint64
	Tenant sentry.TenantID
	TS     int64
	Area   string
	From   string
	Kind   string
	Text   string
	Target string
	Ref    uint64
}

// Store is the message log: a journal for durability plus an in-RAM index for
// cheap polling. Safe for concurrent use.
type Store struct {
	journal *sentry.Store
	mu      sync.Mutex
	entries []Message
}

// New wraps a journal, rebuilding the in-RAM message index from any EventMessage
// records already on disk.
func New(journal *sentry.Store) (*Store, error) {
	s := &Store{journal: journal}
	etype := EventMessage
	var scanErr error
	err := journal.Scan(sentry.Filter{Type: &etype}, func(r sentry.Record) bool {
		var p MessagePayload
		if err := sentry.UnmarshalPayload(r.Payload, &p); err != nil {
			scanErr = fmt.Errorf("comms: decode record seq %d: %w", r.Seq, err)
			return false
		}
		s.entries = append(s.entries, message(uint64(r.Seq), r.Tenant, r.Tstamp, p))
		return true
	})
	if err != nil {
		return nil, err
	}
	if scanErr != nil {
		return nil, scanErr
	}
	return s, nil
}

func message(seq uint64, tenant sentry.TenantID, ts int64, p MessagePayload) Message {
	return Message{
		Seq: seq, Tenant: tenant, TS: ts,
		Area: p.Area, From: p.From, Kind: p.Kind, Text: p.Text, Target: p.Target, Ref: p.Ref,
	}
}

// Post appends a message to area for tenant and returns its journal seq (its id).
// Area, From and Text are required; Kind defaults to "note".
func (s *Store) Post(tenant sentry.TenantID, p MessagePayload) (uint64, error) {
	if p.Area == "" || p.From == "" || p.Text == "" {
		return 0, fmt.Errorf("comms: area, from and text are required")
	}
	if p.Kind == "" {
		p.Kind = "note"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, err := s.journal.Append(tenant, EventMessage, p)
	if err != nil {
		return 0, fmt.Errorf("comms: append: %w", err)
	}
	s.entries = append(s.entries, message(uint64(seq), tenant, time.Now().UnixNano(), p))
	return uint64(seq), nil
}

// Read returns tenant's messages in area with Seq > since, in seq order.
func (s *Store) Read(tenant sentry.TenantID, area string, since uint64) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Message
	for _, m := range s.entries {
		if m.Tenant == tenant && m.Area == area && m.Seq > since {
			out = append(out, m)
		}
	}
	return out
}

// Get returns the message at seq in area for tenant (for promote).
func (s *Store) Get(tenant sentry.TenantID, area string, seq uint64) (Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.entries {
		if m.Tenant == tenant && m.Area == area && m.Seq == seq {
			return m, true
		}
	}
	return Message{}, false
}
```

(If `sentry.UnmarshalPayload` has a different name/signature, match what `memory/memory.go`'s `New` uses — it decodes `r.Payload` into a struct the same way.)

- [ ] **Step 4: Run, verify PASS** — `go test ./comms/ -race && go vet ./comms/`. Also `go build ./...`.

- [ ] **Step 5: Commit**

```bash
git add comms/
git commit -m "feat(comms): agent message channel — ordered per-tenant EventMessage log (Post/Read/Get, no dedup, in-RAM index)"
```
End the commit body with:
Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>

---

## Task 2: `cmd/sentrymcp` — `post` / `read` / `promote` tools

**Files:** Modify `cmd/sentrymcp/main.go` (server struct; `main()` wiring; `toolList()`; `callTool` switch), `cmd/sentrymcp/main_test.go`

- [ ] **Step 1: Write the failing test** → append to `cmd/sentrymcp/main_test.go`

Use the existing embedder-backed harness (the one the remember/recall + `TestCallToolTenantIsolation` tests use — `newMemServer`/`callNamed`/`callTool(req, tenant)`; read the file for exact names). The server must also have `chat` set; if the harness doesn't set it, initialize `s.chat` from the same journal in the helper (mirror how `mem` is set). Then:

```go
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
```

(If `newMemServer` doesn't exist, the file's memory tests build a `*server` some way — reuse it and ensure `chat` is set. The contract is: post→read round-trips, tenant 2 is isolated, missing required arg errors.)

- [ ] **Step 2: Run, verify FAIL** — `go test ./cmd/sentrymcp/ -run Comms` → no `post`/`read` case.

- [ ] **Step 3: Add the `chat` field + build it in `main()`**

In the `server` struct add: `chat *comms.Store // agent communication channel`. Import `matrixsentry/comms`. In `main()`, right after the journal `store` is opened (and BEFORE/independent of the `if *ollamaURL != ""` block — comms needs no embedder), add:
```go
	s.chat, err = comms.New(store)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sentrymcp: init comms: %v\n", err)
		os.Exit(1)
	}
```
(Use the same `store` variable passed to `memory.New`. Match the file's error-handling/var style.)

- [ ] **Step 4: Add the three tool definitions** to `toolList()` (after `forget`):

```go
		{
			"name":        "post",
			"description": "Post a message to a shared agent channel ('area') so other agents working the same project see it. Use kind=question to ask, kind=answer to reply (set ref to the question's #), kind=info to share, target to direct it at a specific agent (else broadcast).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"area":   map[string]any{"type": "string", "description": "channel name, e.g. 'projX/backend' (agents agree on names)"},
					"from":   map[string]any{"type": "string", "description": "your agent label, e.g. 'backend' or '01-core'"},
					"text":   map[string]any{"type": "string", "description": "the message"},
					"kind":   map[string]any{"type": "string", "description": "question | answer | info | note (default note)"},
					"target": map[string]any{"type": "string", "description": "optional agent label to direct this at; empty = broadcast"},
					"ref":    map[string]any{"type": "integer", "description": "optional message # this replies to"},
				},
				"required": []any{"area", "from", "text"},
			},
		},
		{
			"name":        "read",
			"description": "Read new messages in an area since a cursor. Pass since=<the last # you saw> to get only newer messages; the response ends with the latest # to use as your next cursor. Poll this to coordinate in near-real-time.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"area":   map[string]any{"type": "string", "description": "channel name"},
					"since":  map[string]any{"type": "integer", "description": "return only messages with # greater than this (default 0 = all)"},
					"target": map[string]any{"type": "string", "description": "optional: only messages directed at this label (plus broadcasts)"},
				},
				"required": []any{"area"},
			},
		},
		{
			"name":        "promote",
			"description": "Promote a channel message to durable semantic memory (remember), e.g. a decision or an answer worth keeping. The message stays in the channel; a memory is also created.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"area": map[string]any{"type": "string", "description": "channel name"},
					"seq":  map[string]any{"type": "integer", "description": "the message # to promote"},
					"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "optional tags for the memory"},
				},
				"required": []any{"area", "seq"},
			},
		},
```

- [ ] **Step 5: Add the handlers** to the `callTool` switch (the function takes `tenant sentry.TenantID` from the prior multi-tenant work):

```go
	case "post":
		area, _ := strArg(p.Args, "area")
		from, _ := strArg(p.Args, "from")
		text, _ := strArg(p.Args, "text")
		if area == "" || from == "" || text == "" {
			return s.toolErr(req.ID, "provide 'area', 'from' and 'text' to post")
		}
		kind, _ := strArg(p.Args, "kind")
		target, _ := strArg(p.Args, "target")
		ref := uintArg(p.Args, "ref")
		s.mu.Lock()
		seq, err := s.chat.Post(tenant, comms.MessagePayload{Area: area, From: from, Kind: kind, Text: text, Target: target, Ref: ref})
		s.mu.Unlock()
		if err != nil {
			return s.toolErr(req.ID, "post failed: "+err.Error())
		}
		s.moko.Info("post", map[string]string{"tenant": fmt.Sprint(tenant), "area": area, "from": from, "seq": fmt.Sprint(seq)})
		return s.toolText(req.ID, fmt.Sprintf("posted message #%d in %s", seq, area))
	case "read":
		area, _ := strArg(p.Args, "area")
		if area == "" {
			return s.toolErr(req.ID, "provide 'area' to read")
		}
		since := uintArg(p.Args, "since")
		target, _ := strArg(p.Args, "target")
		msgs := s.chat.Read(tenant, area, since)
		const cap = 100
		if len(msgs) > cap {
			msgs = msgs[len(msgs)-cap:]
		}
		var b strings.Builder
		var last uint64 = since
		n := 0
		for _, m := range msgs {
			if target != "" && m.Target != "" && m.Target != target {
				continue // filter: keep broadcasts + those addressed to target
			}
			to := m.Target
			if to == "" {
				to = "all"
			}
			fmt.Fprintf(&b, "#%d [%s] %s→%s: %s\n", m.Seq, m.Kind, m.From, to, m.Text)
			if m.Seq > last {
				last = m.Seq
			}
			n++
		}
		if n == 0 {
			return s.toolText(req.ID, fmt.Sprintf("no new messages in %s since #%d", area, since))
		}
		fmt.Fprintf(&b, "(cursor: #%d)", last)
		return s.toolText(req.ID, b.String())
	case "promote":
		if s.mem == nil {
			return s.toolErr(req.ID, "semantic memory disabled: no embedder configured (start sentrymcp with -ollama URL)")
		}
		area, _ := strArg(p.Args, "area")
		seq := uintArg(p.Args, "seq")
		if area == "" || seq == 0 {
			return s.toolErr(req.ID, "provide 'area' and 'seq' to promote")
		}
		m, ok := s.chat.Get(tenant, area, seq)
		if !ok {
			return s.toolErr(req.ID, fmt.Sprintf("message #%d not found in %s", seq, area))
		}
		tags := append(stringsArg(p.Args, "tags"), "promoted")
		s.mu.Lock()
		id, _, _, err := s.mem.Remember(tenant, fmt.Sprintf("[%s %s#%d] %s", m.From, area, seq, m.Text), memory.RememberOpts{Tags: tags, Src: "promote"})
		s.mu.Unlock()
		if err != nil {
			return s.toolErr(req.ID, "promote failed: "+err.Error())
		}
		s.moko.Info("promote", map[string]string{"tenant": fmt.Sprint(tenant), "area": area, "seq": fmt.Sprint(seq), "memid": fmt.Sprint(id)})
		return s.toolText(req.ID, fmt.Sprintf("promoted message #%d in %s → memory #%d", seq, area, id))
```

(`strArg`/`uintArg`/`stringsArg`/`toolErr`/`toolText`/`s.moko` all exist. `Remember` returns `(id, deduped, superseded, err)`.)

- [ ] **Step 6: Build + vet + test the whole module**

Run: `go build ./... && go vet ./... && go test ./...` → all green (comms tests, `TestCommsPostReadPromote`, and every existing test). Update the test harness's `newMemServer` to set `s.chat` if it didn't (report it).

- [ ] **Step 7: Commit**

```bash
git add cmd/sentrymcp/
git commit -m "feat(sentrymcp): post/read/promote tools — tenant-scoped agent comms channel + promote-to-memory"
```
End the commit body with the Co-Authored-By trailer.

---

## Task 3: Deploy + live verify (controller-executed)

The 8808 server gains 3 ADDITIVE tools; existing memory/recall/dedup behavior is unchanged. Clients must re-list tools to see `post`/`read`/`promote` (the stale-cache gotcha — reconnect/new session).

- [ ] **Step 1: Full green gate** — `go build ./... && go test ./...`.
- [ ] **Step 2: Redeploy 8808** (same pattern as prior deploys; keep `.bak`):
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/sentrymcp ./cmd/sentrymcp
scp /tmp/sentrymcp matrix-sentry:/root/sentrymcp.new
ssh matrix-sentry 'cp /root/sentrymcp /root/sentrymcp.bak && mv /root/sentrymcp.new /root/sentrymcp && chmod +x /root/sentrymcp && systemctl restart sentrymcp && sleep 2 && systemctl is-active sentrymcp'
```
- [ ] **Step 3: Live verify** via raw JSON-RPC (owner token → tenant 1): `tools/list` shows post/read/promote; `post(area:"smoke", from:"a", text:"hello", kind:"info")` → `#<seq>`; `read(area:"smoke", since:0)` → the message; `post` a second from "b"; `read(since:<seq1>)` → only the new one; `promote(area:"smoke", seq:<seq>)` → `memory #N`; then `forget` that promoted memory to keep the corpus clean. Confirm a `read` of a non-existent area → "no new messages".
- [ ] **Step 4: Update HANDOFF + memory** — comms channel LIVE (post/read/promote, tenant-scoped, pull v1, push deferred). Note clients must reconnect to see the new tools. Commit.

---

## Notes for the implementer
- **Mirror `memory.Store`** for `comms.Store` (journal + in-RAM index + `New` rebuild) — same patterns, different (no dedup/embed) semantics.
- **comms has no embedder dependency** — build `s.chat` unconditionally in `main()`; only `promote` needs `s.mem`.
- **Tenant comes from the `callTool` `tenant` param** (multi-tenant routing already threads it) — channels are isolated per tenant for free.
- **Message id = journal seq** — no separate counter; `ref`/`promote seq`/`since` all use it.
- Keep `read` payloads bounded (cap 100) so a busy area doesn't return a megabyte.
