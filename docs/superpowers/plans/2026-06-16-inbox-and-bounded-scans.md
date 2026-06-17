# Agent Inbox + Bounded Admin Scans Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Add `sentry.Store.ScanReverse` + use it (plus the comms in-RAM index) to bound the admin/analytics scans, and add `comms.Store.Inbox` + an `inbox` MCP tool so agents never miss directed messages.

**Architecture:** Journal gains a reverse scan; comms.Store gains `Inbox`/`Recent` (in-RAM); the recency endpoints stop after N matches; a new `inbox` tool serves "messages addressed to me".

**Tech Stack:** Go (sentry journal, comms.Store, MCP server).

---

### Task 1: `sentry.Store.ScanReverse`

**Files:** Modify `sentry/store.go`; Test `sentry/store_test.go` (or the existing scan test file).

CONTEXT: `Scan` (sentry/store.go ~282) is `for i:=0;i<n;i++ { rec,err:=s.Read(Seq(i+1)); ...filter...; if !fn(rec){break} }` where `n=len(s.keydir)` under RLock. `Filter{Tenant *TenantID, Type *EventType}`. `Read(seq Seq)(Record,error)`.

- [ ] **Step 1: failing test (append to the sentry scan test file — find where Scan is tested)**

```go
func TestScanReverseNewestFirstAndStops(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, Options{})
	if err != nil { t.Fatal(err) }
	defer st.Close()
	for i := 0; i < 5; i++ {
		if _, err := st.Append(1, EventAccess, AccessPayload{ItemID: uint64(i)}); err != nil { t.Fatal(err) }
	}
	// newest-first order
	var seqs []Seq
	st.ScanReverse(Filter{}, func(r Record) bool { seqs = append(seqs, r.Seq); return true })
	if len(seqs) != 5 || seqs[0] != 5 || seqs[4] != 1 {
		t.Fatalf("want 5..1 newest-first, got %v", seqs)
	}
	// early stop: collect only 2, count invocations
	calls := 0
	var got []Seq
	st.ScanReverse(Filter{}, func(r Record) bool {
		calls++
		got = append(got, r.Seq)
		return len(got) < 2
	})
	if len(got) != 2 || got[0] != 5 || calls != 2 {
		t.Fatalf("early stop failed: got=%v calls=%d (should not read whole journal)", got, calls)
	}
}
```
Match the test file's package + existing Open/Append usage. Run `go test ./sentry/ -run TestScanReverse -v` → FAIL.

- [ ] **Step 2: implement (add after `Scan` in sentry/store.go)**

```go
// ScanReverse visits records newest-first (seq n→1), applying the same tenant/type
// filter as Scan. fn returns false to stop early — letting recency queries read
// only the journal tail instead of the whole log.
func (s *Store) ScanReverse(filter Filter, fn func(Record) bool) error {
	s.mu.RLock()
	n := len(s.keydir)
	s.mu.RUnlock()
	for i := n; i >= 1; i-- {
		rec, err := s.Read(Seq(i))
		if err != nil {
			return err
		}
		if filter.Tenant != nil && rec.Tenant != *filter.Tenant {
			continue
		}
		if filter.Type != nil && rec.Type != *filter.Type {
			continue
		}
		if !fn(rec) {
			break
		}
	}
	return nil
}
```
Run the test → PASS. `go test ./sentry/ -race`.

- [ ] **Step 3: commit**
```bash
git add sentry/store.go sentry/store_test.go
git commit -m "feat(sentry): Store.ScanReverse — newest-first scan with early stop (bound recency queries)"
```

---

### Task 2: `comms.Store.Inbox` + `comms.Store.Recent`

**Files:** Modify `comms/comms.go`, `comms/comms_test.go`.

CONTEXT: `comms.Store` has `entries []Message` under `mu sync.Mutex`; `Read(tenant, area, since)` (comms.go ~100) mirrors the filter pattern. `Message{Seq, Tenant, TS, Area, From, Kind, Text, Target, Ref}`. `Post(tenant, MessagePayload)(uint64,error)`.

- [ ] **Step 1: failing tests (append to comms/comms_test.go)**

```go
func TestInboxFiltersByTarget(t *testing.T) {
	st := newTestStore(t) // use the comms test helper (read the file for its name)
	st.Post(1, MessagePayload{Area: "x", From: "a", Text: "for me", Target: "me"})
	st.Post(1, MessagePayload{Area: "y", From: "b", Text: "also for me", Target: "me"})
	st.Post(1, MessagePayload{Area: "x", From: "a", Text: "for other", Target: "other"})
	st.Post(2, MessagePayload{Area: "x", From: "z", Text: "other tenant", Target: "me"})

	in := st.Inbox(1, "me", 0)
	if len(in) != 2 {
		t.Fatalf("want 2 inbox msgs across areas, got %d", len(in))
	}
	// since cursor
	in2 := st.Inbox(1, "me", in[0].Seq)
	if len(in2) != 1 || in2[0].Seq <= in[0].Seq && false {
		t.Fatalf("since cursor wrong: %+v", in2)
	}
	if len(in2) != 1 {
		t.Fatalf("since should return only newer, got %d", len(in2))
	}
}

func TestRecentReturnsLastN(t *testing.T) {
	st := newTestStore(t)
	for i := 0; i < 5; i++ {
		st.Post(1, MessagePayload{Area: "x", From: "a", Text: "m"})
	}
	st.Post(2, MessagePayload{Area: "x", From: "z", Text: "other tenant"})
	got := st.Recent(1, 3)
	if len(got) != 3 {
		t.Fatalf("want last 3, got %d", len(got))
	}
	// ascending seq, last 3 of tenant 1
	if !(got[0].Seq < got[1].Seq && got[1].Seq < got[2].Seq) {
		t.Fatalf("Recent should be seq-ascending: %+v", got)
	}
	for _, m := range got {
		if m.Tenant != 1 {
			t.Fatal("Recent leaked another tenant")
		}
	}
}
```
Read `comms/comms_test.go` for the real store-construction helper (e.g. a `newTestStore`/`newStore` over a temp journal) and reuse it. Run `go test ./comms/ -run 'TestInbox|TestRecent' -v` → FAIL.

- [ ] **Step 2: implement (add after `Read` in comms/comms.go)**

```go
// Inbox returns messages directed at target (Target==target) across ALL areas
// for tenant, with Seq > since, in seq order. Lets an agent fetch everything
// addressed to it in one call instead of guessing areas. In-RAM; no journal scan.
func (s *Store) Inbox(tenant sentry.TenantID, target string, since uint64) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Message
	for _, m := range s.entries {
		if m.Tenant == tenant && m.Target == target && m.Seq > since {
			out = append(out, m)
		}
	}
	return out
}

// Recent returns the last `limit` messages for tenant across all areas, in seq
// order (oldest→newest). In-RAM; serves the dashboard without a journal scan.
func (s *Store) Recent(tenant sentry.TenantID, limit int) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Message
	for _, m := range s.entries {
		if m.Tenant == tenant {
			out = append(out, m)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}
```
Run the tests → PASS. `go test ./comms/ -race`.

- [ ] **Step 3: commit**
```bash
git add comms/comms.go comms/comms_test.go
git commit -m "feat(comms): Store.Inbox (by target, cross-area) + Recent (last N) — in-RAM, no scan"
```

---

### Task 3: bound the admin/analytics endpoints (main.go)

**Files:** Modify `cmd/sentrymcp/main.go`. (Existing endpoint tests must stay green.)

CONTEXT: read the current `handleAdminJournal`, `analyze_recall` case, and `handleAdminComms` in main.go. They currently use `s.store.Scan(...)`. Rewrite per below. Existing tests (`TestAdminJournal*`, `TestAdminComms*`, `TestAnalyzeRecall*`) must still pass — they assert content, not order-of-scan, so bounded reverse-scan must still return the right records.

- [ ] **Step 1: `/admin/journal` → ScanReverse bounded**

In `handleAdminJournal`, replace the forward `s.store.Scan(...)` + sliding-window-trim with a reverse scan that stops after `limit` matches, then reverses to chronological:
```go
	events := make([]ev, 0, limit)
	t := tenant
	s.store.ScanReverse(sentry.Filter{Tenant: &t}, func(rec sentry.Record) bool {
		var e ev
		e.Seq = uint64(rec.Seq)
		e.TS = rec.Tstamp / 1e6
		switch rec.Type {
		case memory.EventMemory:
			var p memory.MemoryPayload
			if sentry.UnmarshalPayload(rec.Payload, &p) != nil { return true }
			e.Type = "Memory"; e.Text = fmt.Sprintf("#%d %s", p.ID, truncRunes(p.Text, 80))
		case memory.EventForget:
			var p memory.ForgetPayload
			if sentry.UnmarshalPayload(rec.Payload, &p) != nil { return true }
			e.Type = "Forget"; e.Text = fmt.Sprintf("tombstone #%d", p.ID)
		case comms.EventMessage:
			var p comms.MessagePayload
			if sentry.UnmarshalPayload(rec.Payload, &p) != nil { return true }
			e.Type = "Message"
			tgt := ""; if p.Target != "" { tgt = " → " + p.Target }
			e.Text = fmt.Sprintf("%s%s @%s: %s", p.From, tgt, p.Area, truncRunes(p.Text, 60))
		default:
			return true
		}
		events = append(events, e)
		return len(events) < limit // stop once we have the last `limit` matches
	})
	// reverse to chronological (ScanReverse gave newest-first)
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
```
(Keep the surrounding handler — auth, limit parse, JSON encode — unchanged.)

- [ ] **Step 2: `analyze_recall` → ScanReverse bounded window**

In the `analyze_recall` case, replace `s.store.Scan` with `ScanReverse` capped to a recent window (e.g. 500). Since reverse gives newest-first, collect up to 500, and the "recent" tail to display is the FIRST few (newest):
```go
	const recallWindow = 500
	et := memory.EventRecall
	var tops []float64
	empty, total := 0, 0
	var recent []string
	tnt := tenant
	s.store.ScanReverse(sentry.Filter{Tenant: &tnt, Type: &et}, func(rec sentry.Record) bool {
		var p memory.RecallPayload
		if sentry.UnmarshalPayload(rec.Payload, &p) != nil { return true }
		total++
		if len(p.Hits) == 0 { empty++ } else { tops = append(tops, float64(p.Hits[0].Dist)) }
		if len(recent) < 8 { recent = append(recent, truncRunes(p.Query, 40)) } // newest-first
		return total < recallWindow
	})
```
Then keep the existing `total==0` guard, `sort.Float64s(tops)`, percentile + output formatting. NOTE: `recent` is now newest-first (reverse order vs before) — that's fine for "recent queries"; the output line stays the same.

- [ ] **Step 3: `/admin/comms` → serve from comms.Store in-RAM**

Replace `handleAdminComms`'s `s.store.Scan(...)` body with `s.chat.Recent(tenant, limit)`:
```go
	tenant, ok := s.resolveTenant(r)
	if !ok { http.Error(w, "unauthorized", http.StatusUnauthorized); return }
	limit := 100
	if q := r.URL.Query().Get("limit"); q != "" { if v, err := strconv.Atoi(q); err == nil && v > 0 { limit = v } }
	if limit > 300 { limit = 300 }
	type msg struct {
		Seq uint64 `json:"seq"`; TS int64 `json:"ts"`; Area string `json:"area"`; From string `json:"from"`
		Kind string `json:"kind"`; Text string `json:"text"`; Target string `json:"target,omitempty"`; Ref uint64 `json:"ref,omitempty"`
	}
	recent := s.chat.Recent(tenant, limit)
	msgs := make([]msg, 0, len(recent))
	for _, m := range recent {
		msgs = append(msgs, msg{Seq: m.Seq, TS: m.TS / 1e6, Area: m.Area, From: m.From, Kind: m.Kind, Text: m.Text, Target: m.Target, Ref: m.Ref})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct{ Messages []msg `json:"messages"` }{Messages: msgs})
```
(Confirm `comms.Message.TS` is nanoseconds — it is set from `r.Tstamp`/`time.Now().UnixNano()`, so `/1e6` → ms, matching the prior handler.)

- [ ] **Step 4: build + existing tests green**

Run `go build ./... && go test ./... -race`. The existing `TestAdminJournal*`, `TestAdminComms*`, `TestAnalyzeRecall*` MUST still pass (content unchanged; only the scan direction/source changed). Fix any ordering assumption if a test breaks.

- [ ] **Step 5: commit**
```bash
git add cmd/sentrymcp/main.go
git commit -m "perf(sentrymcp): bound admin/analytics scans (ScanReverse + comms in-RAM) — O(N) not O(journal)"
```

---

### Task 4: `inbox` MCP tool

**Files:** Modify `cmd/sentrymcp/main.go`, `cmd/sentrymcp/main_test.go`.

CONTEXT: read the `read` tool def + handler (it renders messages + a cursor) and how `s.chat` is used. Mirror it for `inbox`, calling `s.chat.Inbox(tenant, target, since)`. Tests use `callNamed(s, name, args)` + the response-text extraction helper (from earlier tasks).

- [ ] **Step 1: failing test (append to main_test.go)**

```go
func TestInboxToolReturnsDirectedMessages(t *testing.T) {
	s := newMemServer(t)
	if s.chat == nil { s.chat, _ = comms.New(s.store) }
	s.chat.Post(s.tenant, comms.MessagePayload{Area: "ch1", From: "alice", Text: "ping", Target: "08", Kind: "question"})
	s.chat.Post(s.tenant, comms.MessagePayload{Area: "ch2", From: "bob", Text: "fyi", Target: "08", Kind: "info"})
	s.chat.Post(s.tenant, comms.MessagePayload{Area: "ch1", From: "alice", Text: "not for 08", Target: "09"})

	resp := callNamed(s, "inbox", map[string]any{"target": "08"})
	text := toolText(resp) // same extractor other tool tests use
	if !strings.Contains(text, "ping") || !strings.Contains(text, "fyi") {
		t.Fatalf("inbox should return both messages directed at 08, got: %s", text)
	}
	if strings.Contains(text, "not for 08") {
		t.Fatalf("inbox leaked a message directed at 09: %s", text)
	}
}
```
Run `go test ./cmd/sentrymcp/ -run TestInboxTool -v` → FAIL (unknown tool).

- [ ] **Step 2: add the tool def + handler**

Tool def (near the `read` def):
```go
		{
			"name":        "inbox",
			"description": "Fetch all messages directed at YOU (by target) across every area, in one call — so you never miss a directed message by polling the wrong area. Pass target=<your agent label> and since=<the last # you saw> for only-newer. The response ends with the latest # to use as your next cursor. Reply with post(area=…, kind=\"answer\", ref=<that #>, target=<sender>).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target": map[string]any{"type": "string", "description": "your agent label — messages whose target is this are returned"},
					"since":  map[string]any{"type": "integer", "description": "return only messages with # greater than this (default 0 = all)"},
				},
				"required": []any{"target"},
			},
		},
```
Handler (near the `read` case). Reuse the same rendering `read` uses (find its formatter; if `read` formats inline, mirror it):
```go
	case "inbox":
		target, _ := strArg(p.Args, "target")
		if target == "" {
			return s.toolErr(req.ID, "provide 'target' (your agent label) to read your inbox")
		}
		var since uint64
		if v, ok := numArg(p.Args, "since"); ok && v > 0 {
			since = uint64(v)
		}
		msgs := s.chat.Inbox(tenant, target, since)
		var b strings.Builder
		cursor := since
		if len(msgs) == 0 {
			fmt.Fprintf(&b, "inbox empty for %q (since #%d)", target, since)
		} else {
			fmt.Fprintf(&b, "%d message(s) for %q:\n", len(msgs), target)
			for _, m := range msgs {
				tgt := m.Target
				if tgt == "" { tgt = "all" }
				fmt.Fprintf(&b, "#%d [%s] %s→%s @%s: %s\n", m.Seq, m.Kind, m.From, tgt, m.Area, m.Text)
				if m.Seq > cursor { cursor = m.Seq }
			}
			fmt.Fprintf(&b, "(cursor: #%d)", cursor)
		}
		return s.toolText(req.ID, b.String())
```
(Match the actual `read` rendering style if it differs — keep `inbox` consistent with `read`. Ensure `strings` is imported.)

- [ ] **Step 3: tests + build**

Run `go test ./cmd/sentrymcp/ -run TestInboxTool -v` → PASS. Then `go build ./... && go test ./... -race`, `go vet ./cmd/sentrymcp/`.

- [ ] **Step 4: commit**
```bash
git add cmd/sentrymcp/main.go cmd/sentrymcp/main_test.go
git commit -m "feat(sentrymcp): inbox tool — all messages directed at an agent, one call (no missed comms)"
```

---

### Task 5: deploy + verify

**Files:** none (operational).

- [ ] **Step 1: rebuild + redeploy 8808 + 8809**
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/sentrymcp ./cmd/sentrymcp
scp -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes /tmp/sentrymcp root@10.10.10.96:/root/sentrymcp.new
ssh matrix-sentry 'mv /root/sentrymcp /root/sentrymcp.old && mv /root/sentrymcp.new /root/sentrymcp && chmod +x /root/sentrymcp && systemctl restart sentrymcp && sleep 2 && systemctl is-active sentrymcp'
scp -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes /tmp/sentrymcp root@10.10.10.175:/root/sentrymcp.new
ssh matrix-sentry2 'mv /root/sentrymcp /root/sentrymcp.old && mv /root/sentrymcp.new /root/sentrymcp && chmod +x /root/sentrymcp && systemctl restart sentrymcp-mt && sleep 2 && systemctl is-active sentrymcp-mt'
```

- [ ] **Step 2: verify perf + inbox** — on 8808 with the token:
  - Time `analyze_recall` and `/admin/journal` → should now be a few ms (was ~350ms). Compare before/after.
  - `inbox(target=<a real agent label seen in comms, e.g. 08-architecture>)` → returns that agent's directed messages with a cursor.
  - Confirm the dashboard journal/comms still render (the bounded endpoints return the right recent records).

- [ ] **Step 3: update HANDOFF + the comms-protocol memory; commit**
HANDOFF: note ScanReverse + bounded endpoints (perf #3) and the `inbox` tool. Update the comms-protocol memory to mention `inbox`. Commit.
```bash
git add HANDOFF.md && git commit -m "docs: inbox tool + bounded admin scans deployed"
```
