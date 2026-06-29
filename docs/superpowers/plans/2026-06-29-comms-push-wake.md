# Comms Push (wake-on-update) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Push a wake "nudge" to a blazeagent agent the instant comms activity it cares about happens, so it reacts immediately and polls only as a slow safety net — never missing a message.

**Architecture:** Matrix gains an in-RAM pub/sub hub in `comms` (fired by `Post`/`PostImage`) and an authenticated SSE endpoint `GET /comms/subscribe`. blazeagent gains a `Subscribe` client that consumes the SSE nudge stream and a loop that triggers its existing `Read`/`Inbox` cycle on a nudge (or a slow fallback ticker). The nudge is advisory; all message delivery stays in the existing, tested poll path. The journal is never touched.

**Tech Stack:** Pure Go, zero external deps, one binary per repo. matrix-sentry module `matrixsentry`; blazeagent module `github.com/blazesphere/blazeagent`. Build-on-Mac→ship-linux for Matrix; blazeagent ships via CI.

## Global Constraints

- **Zero external dependencies** — stdlib only, both repos.
- **The journal is never mutated** — the nudge is ephemeral in-RAM pub/sub derived from `Post`/`PostImage`; NO new journal event type.
- **`publish` must never block `Post`** — buffered-size-1 channel + non-blocking send + coalescing.
- **Transport is SSE** (`text/event-stream` + `http.Flusher`); client→server stays on the existing POST `/mcp`.
- **Tenant isolation** — the SSE stream is scoped to the bearer's tenant via the existing `resolveTenant`.
- **Wire protocol:** `GET /comms/subscribe?target=<t>&areas=<a,b>&since=<n>` + `Authorization: Bearer <tok>` → `text/event-stream` with `event: nudge\ndata: {"seq":N,"area":"…","target":"…","kind":"…"}\n\n` and `:hb\n\n` heartbeats (~25s). At least one of `target`/`areas` required.
- **Match rule:** a message matches a subscriber iff same tenant AND (`msg.Target==filter.Target` when Target set) OR (`msg.Area ∈ filter.Areas`).
- **Sequence:** Phase A (matrix-sentry, Tasks 1–2) is deployable/testable alone with `curl`. Phase B (blazeagent, Tasks 3–4) depends on the endpoint existing.

---

## Phase A — matrix-sentry (server). Working dir: `/Users/alvinnunez/Downloads/matrix-sentry`

### Task 1: comms notify hub + match/catch-up helpers

**Files:**
- Create: `comms/hub.go`
- Create: `comms/hub_test.go`
- Modify: `comms/comms.go` (add `hub` field to `Store`, init in `New`, fire `publish` in `Post` and `PostImage`)

**Interfaces:**
- Consumes: `sentry.TenantID`; existing `Store`, `Message`, `message()`, `Post`, `PostImage`.
- Produces:
  - `type Nudge struct { Seq uint64; Area, Target, Kind string }`
  - `type Filter struct { Tenant sentry.TenantID; Target string; Areas []string }`
  - `func (s *Store) Subscribe(f Filter) (<-chan Nudge, func())`
  - `func (s *Store) MatchingSince(f Filter, since uint64) uint64`
  - internal: `func (f Filter) matches(m Message) bool`

- [ ] **Step 1: Write the failing test**

Create `comms/hub_test.go`:

```go
package comms

import (
	"testing"
	"time"
)

func TestHubDeliversOnMatchAndCoalesces(t *testing.T) {
	st, _ := newTestStore(t)

	// Subscribe by target.
	chT, cancelT := st.Subscribe(Filter{Tenant: 1, Target: "worker-a"})
	defer cancelT()
	// Subscribe by area.
	chA, cancelA := st.Subscribe(Filter{Tenant: 1, Areas: []string{"proj/x"}})
	defer cancelA()

	// A message directed at worker-a in some area → target subscriber gets it.
	seq, _ := st.Post(1, MessagePayload{Area: "anything", From: "o", Text: "do it", Target: "worker-a"})
	select {
	case n := <-chT:
		if n.Seq != seq {
			t.Fatalf("target nudge seq=%d want %d", n.Seq, seq)
		}
	case <-time.After(time.Second):
		t.Fatal("target subscriber got no nudge")
	}

	// A message in proj/x not directed at worker-a → area subscriber gets it, target does not.
	seq2, _ := st.Post(1, MessagePayload{Area: "proj/x", From: "o", Text: "fyi"})
	select {
	case n := <-chA:
		if n.Seq != seq2 {
			t.Fatalf("area nudge seq=%d want %d", n.Seq, seq2)
		}
	case <-time.After(time.Second):
		t.Fatal("area subscriber got no nudge")
	}

	// Tenant isolation: a different tenant's message must not deliver.
	st.Post(2, MessagePayload{Area: "proj/x", From: "o", Text: "other tenant", Target: "worker-a"})
	select {
	case n := <-chT:
		t.Fatalf("tenant isolation broken: got nudge %+v", n)
	case <-time.After(100 * time.Millisecond):
	}

	// Coalescing / non-blocking: with a full buffer, many posts must not block Post.
	for i := 0; i < 50; i++ {
		st.Post(1, MessagePayload{Area: "flood", From: "o", Text: "x", Target: "worker-a"})
	}

	// MatchingSince finds the latest matching seq > since.
	if got := st.MatchingSince(Filter{Tenant: 1, Target: "worker-a"}, 0); got == 0 {
		t.Fatal("MatchingSince should find matching messages")
	}
	if got := st.MatchingSince(Filter{Tenant: 1, Target: "nobody"}, 0); got != 0 {
		t.Fatalf("MatchingSince for non-matching target = %d, want 0", got)
	}

	// Cancel stops delivery.
	cancelT()
	st.Post(1, MessagePayload{Area: "z", From: "o", Text: "after cancel", Target: "worker-a"})
	select {
	case _, open := <-chT:
		if open {
			t.Fatal("delivery continued after cancel")
		}
	case <-time.After(100 * time.Millisecond):
	}
}
```

> `newTestStore(t)` returns `(*Store, string)` — same helper Task-2 image tests used. Confirm its signature in `comms_test.go` and adapt if needed.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./comms/ -run TestHubDeliversOnMatchAndCoalesces`
Expected: FAIL — `Subscribe`/`MatchingSince`/`Nudge`/`Filter` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `comms/hub.go`:

```go
package comms

import "sync"

// Nudge is an ephemeral wake signal: "activity matching your filter, up to Seq".
// It is NOT journaled — it is derived from a Post/PostImage and fanned out in RAM.
type Nudge struct {
	Seq    uint64 `json:"seq"`
	Area   string `json:"area,omitempty"`
	Target string `json:"target,omitempty"`
	Kind   string `json:"kind,omitempty"`
}

// Filter is a subscriber's interest: same tenant AND (Target match OR Area match).
type Filter struct {
	Tenant sentryTenant
	Target string
	Areas  []string
}

// matches reports whether message m should wake this filter's subscriber.
func (f Filter) matches(m Message) bool {
	if m.Tenant != f.Tenant {
		return false
	}
	if f.Target != "" && m.Target == f.Target {
		return true
	}
	for _, a := range f.Areas {
		if m.Area == a {
			return true
		}
	}
	return false
}

type subscription struct {
	filter Filter
	ch     chan Nudge
}

// hub is an in-RAM pub/sub of comms nudges. Its mutex is independent of Store.mu;
// the only lock order is Store.mu → hub.mu (publish is called from Post under
// Store.mu; Subscribe/cancel take only hub.mu), so there is no inversion.
type hub struct {
	mu   sync.Mutex
	next int
	subs map[int]subscription
}

func newHub() *hub { return &hub{subs: map[int]subscription{}} }

// subscribe registers a filter and returns a receive channel (buffered 1) plus a
// cancel func that unregisters and closes the channel.
func (h *hub) subscribe(f Filter) (<-chan Nudge, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.next
	h.next++
	ch := make(chan Nudge, 1)
	h.subs[id] = subscription{filter: f, ch: ch}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if s, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(s.ch)
		}
	}
}

// publish fans m out to matching subscribers with a NON-BLOCKING send. A full
// buffer means a nudge is already pending; coalescing is safe because a nudge
// only says "there is something ≥ Seq" and the agent reads everything since its
// cursor. Never blocks the caller (Post).
func (h *hub) publish(m Message) {
	n := Nudge{Seq: m.Seq, Area: m.Area, Target: m.Target, Kind: m.Kind}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.subs {
		if !s.filter.matches(m) {
			continue
		}
		select {
		case s.ch <- n:
		default: // buffer full → pending nudge already covers this; drop.
		}
	}
}
```

> `sentryTenant` is a local alias to keep `hub.go` import-light. Add at the top of `hub.go`:
> `type sentryTenant = sentry.TenantID` and `import "matrixsentry/sentry"`. (If the linter prefers, use
> `sentry.TenantID` directly in `Filter` and drop the alias — either is fine; pick one and be consistent.)

In `comms/comms.go`, add the hub to `Store` and wire it. Add the field:

```go
	hub *hub // in-RAM nudge pub/sub (wake-on-update); nil-safe via lazy init in New
```

In `New`, after `s := &Store{journal: journal}` (before returning), set `s.hub = newHub()`.

At the END of `Post` (after `s.pruneAt(...)`, before `return uint64(seq), nil`) add:

```go
	s.hub.publish(message(uint64(seq), tenant, time.Now().UnixNano(), p))
```

At the END of `PostImage` (after `s.pruneAt(...)`, before `return uint64(seq), nil`) add the same line. (Both run while `s.mu` is held; `publish` is non-blocking and only takes `hub.mu`, so the consistent Store.mu→hub.mu order holds and it cannot block.)

Add the two public methods (place after `Subscribe`-less code, e.g. near `Get`):

```go
// Subscribe registers interest (by target and/or areas, scoped to tenant) and
// returns a nudge channel + cancel func. Wake-on-update for agents.
func (s *Store) Subscribe(f Filter) (<-chan Nudge, func()) {
	return s.hub.subscribe(f)
}

// MatchingSince returns the highest seq of a live message matching f with
// Seq > since, or 0 if none — used for the SSE catch-up nudge on (re)connect.
func (s *Store) MatchingSince(f Filter, since uint64) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var max uint64
	for _, m := range s.entries {
		if m.Seq > since && f.matches(m) && m.Seq > max {
			max = m.Seq
		}
	}
	return max
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./comms/ -run TestHubDeliversOnMatchAndCoalesces -v && go test ./comms/ && go vet ./comms/`
Expected: PASS; full comms suite green; vet clean.

- [ ] **Step 5: Commit**

```bash
git add comms/hub.go comms/hub_test.go comms/comms.go
git commit -m "feat(comms): in-RAM nudge hub (Subscribe/MatchingSince) fired by Post/PostImage"
```

---

### Task 2: SSE endpoint `GET /comms/subscribe`

**Files:**
- Modify: `cmd/sentrymcp/main.go` (register route in the mux; add `handleCommsSubscribe`; add `writeNudge` + a uint query helper)
- Test: `cmd/sentrymcp/main_test.go` (append)

**Interfaces:**
- Consumes: Task 1 `comms.Filter`, `comms.Nudge`, `s.chat.Subscribe`, `s.chat.MatchingSince`; existing `s.resolveTenant(r) (sentry.TenantID, bool)`, `s.chat.Post`.
- Produces: HTTP route `/comms/subscribe`; `handleCommsSubscribe`.

- [ ] **Step 1: Write the failing test**

Append to `cmd/sentrymcp/main_test.go` (adapt to the file's existing test-server helper; it builds a `*server` with `s.chat`). The test drives the handler with `httptest` and a cancelable request context:

```go
func TestCommsSubscribeSSE(t *testing.T) {
	s := newTestServer(t) // existing helper; ensure s.chat is set

	// Pre-post a matching message so catch-up has something (since=0).
	seq, _ := s.chat.Post(s.tenant, comms.MessagePayload{Area: "proj/x", From: "o", Text: "hi", Target: "w"})

	req := httptest.NewRequest("GET", "/comms/subscribe?target=w&areas=proj/x&since=0", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() { s.handleCommsSubscribe(rec, req); close(done) }()

	// Give the handler a moment to emit the catch-up nudge, then a live one.
	time.Sleep(50 * time.Millisecond)
	live, _ := s.chat.Post(s.tenant, comms.MessagePayload{Area: "proj/x", From: "o", Text: "live", Target: "w"})
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, "event: nudge") {
		t.Fatalf("no nudge event in stream:\n%s", body)
	}
	// Catch-up nudge carries the pre-existing seq; live nudge carries the new seq.
	if !strings.Contains(body, fmt.Sprintf(`"seq":%d`, seq)) && !strings.Contains(body, fmt.Sprintf(`"seq":%d`, live)) {
		t.Fatalf("expected a seq (%d or %d) in stream:\n%s", seq, live, body)
	}
}

func TestCommsSubscribeRequiresAuthAndFilter(t *testing.T) {
	s := newTestServer(t)
	s.requireAuthForTest(t) // see note: ensure auth is configured so resolveTenant can 401

	// Missing filter → 400.
	req := httptest.NewRequest("GET", "/comms/subscribe", nil)
	req.Header.Set("Authorization", "Bearer "+s.ownerTokenForTest())
	rec := httptest.NewRecorder()
	s.handleCommsSubscribe(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing filter: got %d want 400", rec.Code)
	}
}
```

> The two helper calls (`requireAuthForTest`, `ownerTokenForTest`) are illustrative — replace them with how
> `main_test.go` already constructs an authenticated request / token (the OAuth tests show the real pattern).
> If the test server runs in open/local mode (`resolveTenant` returns default, no 401), drop the auth-401
> assertion and keep only the 400-missing-filter and the SSE-stream assertions. Do NOT invent auth scaffolding.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/sentrymcp/ -run TestCommsSubscribe`
Expected: FAIL — `handleCommsSubscribe` undefined.

- [ ] **Step 3: Write minimal implementation**

In `cmd/sentrymcp/main.go`, register the route alongside the others (near `mux.HandleFunc("/admin/comms", …)`):

```go
	mux.HandleFunc("/comms/subscribe", s.handleCommsSubscribe)
```

Add the handler and helpers (place near the other `handleAdmin*` handlers):

```go
// handleCommsSubscribe is an SSE stream that pushes a "nudge" whenever comms
// activity matching the subscriber's filter (target and/or areas, tenant-scoped)
// occurs. The nudge is advisory; the client fetches via its normal read/inbox.
func (s *server) handleCommsSubscribe(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.resolveTenant(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	target := q.Get("target")
	var areas []string
	if a := q.Get("areas"); a != "" {
		areas = strings.Split(a, ",")
	}
	if target == "" && len(areas) == 0 {
		http.Error(w, "provide target and/or areas", http.StatusBadRequest)
		return
	}
	since := parseUintQuery(q.Get("since"))

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	f := comms.Filter{Tenant: tenant, Target: target, Areas: areas}

	// Catch-up: if anything matching is newer than the client's cursor, nudge once.
	if latest := s.chat.MatchingSince(f, since); latest > since {
		writeNudge(w, comms.Nudge{Seq: latest})
		flusher.Flush()
	}

	ch, cancel := s.chat.Subscribe(f)
	defer cancel()

	hb := time.NewTicker(25 * time.Second)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case n := <-ch:
			writeNudge(w, n)
			flusher.Flush()
		case <-hb.C:
			io.WriteString(w, ":hb\n\n")
			flusher.Flush()
		}
	}
}

// writeNudge emits one SSE "nudge" event with the nudge JSON as data.
func writeNudge(w io.Writer, n comms.Nudge) {
	b, _ := json.Marshal(n)
	fmt.Fprintf(w, "event: nudge\ndata: %s\n\n", b)
}

// parseUintQuery parses a non-negative integer query value, defaulting to 0.
func parseUintQuery(s string) uint64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
```

> Confirm `io`, `encoding/json`, `strconv`, `time`, `strings` are imported in `main.go` (most already are). Add any missing.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/sentrymcp/ -run TestCommsSubscribe -v && go test ./cmd/sentrymcp/ && go vet ./...`
Expected: PASS; full suite green; vet clean.

- [ ] **Step 5: Commit**

```bash
git add cmd/sentrymcp/main.go cmd/sentrymcp/main_test.go
git commit -m "feat(sentrymcp): SSE /comms/subscribe endpoint (nudge + catch-up + heartbeat)"
```

---

### Task 3: Deploy Phase A to 8808 and verify with curl (ops — no TDD)

**Files:** none.

- [ ] **Step 1: Cross-compile + ship + checksum**

```bash
GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/sentrymcp.linux-amd64 ./cmd/sentrymcp
scp /tmp/sentrymcp.linux-amd64 matrix-sentry:/root/sentrymcp.new
ssh matrix-sentry 'sha256sum /root/sentrymcp.new'; shasum -a 256 /tmp/sentrymcp.linux-amd64   # must match
```

- [ ] **Step 2: Swap + restart (keep backup)**

```bash
ssh matrix-sentry 'cp -f /root/sentrymcp /root/sentrymcp.prev && mv /root/sentrymcp.new /root/sentrymcp && chmod +x /root/sentrymcp && systemctl restart sentrymcp.service && sleep 1 && echo active=$(systemctl is-active sentrymcp.service)'
```

- [ ] **Step 3: Verify the SSE stream end-to-end**

```bash
ssh matrix-sentry 'TOK=$(grep ^SENTRY_MCP_TOKEN= /root/sentrymcp.env | cut -d= -f2-); ( curl -sN -H "Authorization: Bearer $TOK" "http://127.0.0.1:8808/comms/subscribe?areas=pushtest&since=0" & SUBPID=$!; sleep 1; curl -s -X POST http://127.0.0.1:8808/mcp -H "Authorization: Bearer $TOK" -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"post\",\"arguments\":{\"area\":\"pushtest\",\"from\":\"t\",\"text\":\"wake\"}}}" >/dev/null; sleep 1; kill $SUBPID ) 2>&1 | head'
```
Expected: an `event: nudge` line appears after the post. Then `comms_clear(pushtest)` to tidy.

---

## Phase B — blazeagent (client). Working dir: `/Users/alvinnunez/Documents/GitHub/blazeagent`

### Task 4: `matrix.Cliente.Subscribe` (SSE nudge consumer with reconnect)

**Files:**
- Create: `internal/matrix/subscribe.go`
- Create: `internal/matrix/subscribe_test.go`

**Interfaces:**
- Consumes: existing `Cliente` (`baseURL`, `token`, `http`), `trimSlash`.
- Produces:
  - `type Nudge struct { Seq int; Area, Target, Kind string }`
  - `type SubFilter struct { Target string; Areas []string }`
  - `func (c *Cliente) Subscribe(ctx context.Context, f SubFilter, cursorFn func() int) <-chan Nudge`

- [ ] **Step 1: Write the failing test**

Create `internal/matrix/subscribe_test.go`:

```go
package matrix

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSubscribeParsesNudges(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("target") != "w" {
			t.Errorf("target not forwarded: %s", r.URL.RawQuery)
		}
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ":hb\n\n") // heartbeat — must be ignored
		io.WriteString(w, "event: nudge\ndata: {\"seq\":42,\"area\":\"proj/x\",\"kind\":\"question\"}\n\n")
		if fl != nil {
			fl.Flush()
		}
		<-r.Context().Done() // hold open until client disconnects
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := c.Subscribe(ctx, SubFilter{Target: "w"}, func() int { return 0 })

	select {
	case n := <-ch:
		if n.Seq != 42 || n.Area != "proj/x" {
			t.Fatalf("parsed nudge wrong: %+v", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no nudge received")
	}
}

func TestSubscribeReconnectsResendingCursor(t *testing.T) {
	var attempts int
	gotCursor := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		gotCursor <- r.URL.Query().Get("since")
		// First connection closes immediately (simulate drop); client must reconnect.
		if attempts == 1 {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, fmt.Sprintf("event: nudge\ndata: {\"seq\":7}\n\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	c.subBackoff = 10 * time.Millisecond // fast reconnect for the test
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cursor := 5
	ch := c.Subscribe(ctx, SubFilter{Areas: []string{"a"}}, func() int { return cursor })

	select {
	case n := <-ch:
		if n.Seq != 7 {
			t.Fatalf("after reconnect seq=%d want 7", n.Seq)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no nudge after reconnect")
	}
	// At least the reconnect attempt re-sent the cursor (since=5).
	saw5 := false
	for i := 0; i < len(gotCursor); i++ {
		if <-gotCursor == "5" {
			saw5 = true
		}
	}
	if !saw5 {
		t.Fatal("reconnect did not resend cursor since=5")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/matrix/ -run TestSubscribe`
Expected: FAIL — `Subscribe`/`SubFilter`/`Nudge`/`subBackoff` undefined.

- [ ] **Step 3: Write minimal implementation**

Add a `subBackoff` field to `Cliente` in `internal/matrix/client.go` (next to `retryBackoff`):

```go
	subBackoff time.Duration // base reconnect backoff for Subscribe (default 1s)
```
and in `New` set `subBackoff: time.Second,`.

Create `internal/matrix/subscribe.go`:

```go
package matrix

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Nudge is a wake signal pushed by the server: "activity you care about, up to Seq".
type Nudge struct {
	Seq    int    `json:"seq"`
	Area   string `json:"area"`
	Target string `json:"target"`
	Kind   string `json:"kind"`
}

// SubFilter declares interest: by Target and/or Areas (at least one).
type SubFilter struct {
	Target string
	Areas  []string
}

// Subscribe opens the SSE wake stream and returns a channel of nudges. It runs
// until ctx is done, reconnecting with backoff on any drop and RE-SENDING the
// current cursor (cursorFn) as `since` so the server's catch-up covers the gap.
// Heartbeat (":" comment) lines are ignored. The channel is closed when ctx ends.
func (c *Cliente) Subscribe(ctx context.Context, f SubFilter, cursorFn func() int) <-chan Nudge {
	out := make(chan Nudge, 8)
	go func() {
		defer close(out)
		backoff := c.subBackoff
		if backoff <= 0 {
			backoff = time.Second
		}
		for ctx.Err() == nil {
			err := c.streamOnce(ctx, f, cursorFn(), out)
			if ctx.Err() != nil {
				return
			}
			if c.debugf != nil && err != nil {
				c.debugf("[matrix~] suscripción cayó: %v (reconectando en %s)", err, backoff)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}
	}()
	return out
}

// streamOnce holds one SSE connection, parsing nudge events into out until the
// connection ends or ctx is canceled.
func (c *Cliente) streamOnce(ctx context.Context, f SubFilter, since int, out chan<- Nudge) error {
	q := url.Values{}
	if f.Target != "" {
		q.Set("target", f.Target)
	}
	if len(f.Areas) > 0 {
		q.Set("areas", strings.Join(f.Areas, ","))
	}
	q.Set("since", fmt.Sprintf("%d", since))
	u := c.baseURL + "/comms/subscribe?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "text/event-stream")

	// A dedicated client with NO overall timeout — the stream is long-lived.
	client := &http.Client{Transport: c.http.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("subscribe http %d", resp.StatusCode)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var event string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "": // event boundary
			event = ""
		case strings.HasPrefix(line, ":"): // heartbeat/comment — ignore
			continue
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:") && event == "nudge":
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var n Nudge
			if json.Unmarshal([]byte(data), &n) == nil {
				select {
				case out <- n:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}
	return sc.Err()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/matrix/ -run TestSubscribe -v && go test ./internal/matrix/ && go vet ./internal/matrix/`
Expected: PASS; full matrix pkg green; vet clean.

- [ ] **Step 5: Commit**

```bash
git add internal/matrix/subscribe.go internal/matrix/subscribe_test.go internal/matrix/client.go
git commit -m "feat(matrix): SSE Subscribe client (nudge stream, reconnect resends cursor)"
```

---

### Task 5: Wire push into the worker/orchestrator loop + config

**Files:**
- Modify: `internal/config/config.go` (add `MatrixPush bool`, `MatrixPushFallback time.Duration`)
- Modify: `internal/agent/worker.go` (subscribe + trigger the inbox cycle on nudge / slow fallback)
- Modify: `internal/agent/orchestrator.go` (same for the read cycle)
- Test: `internal/agent/` (a focused test that a nudge triggers a cycle)

**Interfaces:**
- Consumes: Task 4 `c.Subscribe`, `matrix.SubFilter`, `matrix.Nudge`; existing `progreso`, `Inbox`/`Read`.
- Produces: push-driven loops; config flags.

- [ ] **Step 1: Write the failing test**

Append to an `internal/agent` test file (e.g. `internal/agent/push_test.go`). The test asserts the worker runs an inbox cycle when a nudge arrives, using a fake matrix endpoint. Mirror the harness other agent tests use to construct a `*Worker`/`*Agente` with a `*matrix.Cliente` pointed at an `httptest.Server`. Concretely, assert that after a nudge the worker calls `Inbox` (the server sees an inbox tools/call) without waiting for the slow fallback:

```go
//go:build push_wiring
package agent

// NOTE: This test documents the required behavior. If the existing agent test
// harness cannot easily inject a *matrix.Cliente against httptest, implement the
// wiring (Step 3) and assert via the integration path the harness DOES support
// (e.g. a fake client interface). Keep the assertion: a nudge triggers an inbox
// cycle before the fallback ticker fires.
```

> Reality check for the implementer: read `internal/agent/*_test.go` first. If there is already a seam for a
> fake matrix client, write a real failing test that a nudge triggers a cycle. If the only seam is the
> concrete `*matrix.Cliente`, point it at an `httptest.Server` that serves the SSE stream + inbox responses,
> and assert the inbox endpoint is hit promptly after a nudge. Do NOT build a large new mocking framework —
> if no seam exists and one would be invasive, implement Step 3, add the smallest seam that makes the trigger
> unit-testable (e.g. extract the cycle body into a method and test that method is invoked), and say so in the
> report.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -run TestPush -tags push_wiring`
Expected: FAIL (behavior not wired).

- [ ] **Step 3: Write minimal implementation**

In `internal/config/config.go`, add fields to `Config` and defaults in the loader:

```go
	MatrixPush         bool          // suscribirse al stream de nudges en vez de poll rápido
	MatrixPushFallback time.Duration // poll de respaldo cuando push está activo
```
In the loader (near `LLMTimeout`):
```go
		MatrixPush:         envBool("MATRIX_PUSH", true),
		MatrixPushFallback: envDur("MATRIX_PUSH_FALLBACK", 60*time.Second),
```
> Use the existing `envBool`/`envDur` helpers (the file already has `envDur`; if `envBool` is absent, add a tiny one mirroring `envDur`).

In `internal/agent/worker.go`, extract the inbox poll-and-process body of `Loop` into a method so both the ticker and a nudge can call it. Move the loop-local `cursor`, `polls`, `tasks`, `lastActivity` into a small struct or worker fields. Then replace the loop body:

```go
	prog := newProgreso(w.ag.mem, "worker:"+w.cfg.MatrixLabel)
	st := &loopState{cursor: prog.cursor()}

	go BridgeTelegram(ctx, w.ag.Telegram(), w.mx, w.cfg.MatrixArea, w.cfg.MatrixLabel, w.logf)

	hb := time.NewTicker(w.cfg.Heartbeat)
	defer hb.Stop()

	// Trigger source: push nudges (fast) + a slow fallback ticker (safety net),
	// OR the legacy fast poll when push is disabled.
	var nudges <-chan matrix.Nudge
	var fallback *time.Ticker
	if w.cfg.MatrixPush {
		nudges = w.mx.Subscribe(ctx, matrix.SubFilter{Target: w.cfg.MatrixLabel}, prog.cursor)
		fallback = time.NewTicker(w.cfg.MatrixPushFallback)
		w.logf("[worker] push activo (fallback=%s)", w.cfg.MatrixPushFallback)
	} else {
		fallback = time.NewTicker(w.cfg.PollInterval)
	}
	defer fallback.Stop()

	for {
		select {
		case <-ctx.Done():
			w.logf("[worker] detenido por contexto (ciclos=%d tareas=%d cursor=#%d)", st.polls, st.tasks, st.cursor)
			return ctx.Err()
		case <-hb.C:
			w.logf("[heartbeat] ciclos=%d cursor=#%d tareas=%d en_vuelo=%d | target=%s",
				st.polls, st.cursor, st.tasks, w.inflight.Load(), w.cfg.MatrixLabel)
		case <-nudges: // nil channel when push off → never fires
			w.inboxCycle(ctx, prog, st)
		case <-fallback.C:
			w.inboxCycle(ctx, prog, st)
		}
	}
```

Add `loopState` + the extracted `inboxCycle` (the exact body that used to be under `case <-poll.C:`), e.g.:

```go
type loopState struct {
	cursor       int
	polls        int
	tasks        int
	lastActivity time.Time
}

// inboxCycle fetches directed messages since the cursor and dispatches actionable
// ones. Triggered by a nudge or the fallback ticker. (Body moved verbatim from the
// previous case <-poll.C block; cursor/counters now live in st.)
func (w *Worker) inboxCycle(ctx context.Context, prog *progreso, st *loopState) {
	st.polls++
	t0 := time.Now()
	msgs, last, err := w.mx.Inbox(ctx, w.cfg.MatrixLabel, st.cursor)
	if err != nil {
		if ctx.Err() != nil {
			w.logf("[worker] inbox cancelado por apagado (ciclo %d)", st.polls)
			return
		}
		w.logf("[warn] inbox falló (ciclo %d) tras %s: %v", st.polls, time.Since(t0).Round(time.Millisecond), err)
		return
	}
	if w.cfg.HTTPDebug || len(msgs) > 0 {
		w.logf("[worker] ciclo %d: inbox %d msg(s) en %s (cursor=#%d)", st.polls, len(msgs), time.Since(t0).Round(time.Millisecond), st.cursor)
	}
	if last > st.cursor {
		st.cursor = last
		prog.setCursor(st.cursor)
	}
	pauseActive := w.ag.syncPauseState(msgs)
	for _, m := range msgs {
		if pauseActive || prog.isDone(m.Seq) {
			continue
		}
		switch ClassifyMessage(m, w.cfg.MatrixLabel) {
		case MsgIgnore:
			continue
		case MsgInformational:
			prog.markDone(m.Seq)
		case MsgActionable:
			prog.markDone(m.Seq)
			st.tasks++
			st.lastActivity = time.Now()
			w.logf("[worker] >>> TAREA #%d de %s (area %s): %s", m.Seq, m.From, m.Area, preview(m.Text, 90))
			w.inflight.Add(1)
			go func(seq int, area, text, from string) {
				defer w.inflight.Add(-1)
				w.runTask(ctx, seq, area, text, from)
			}(m.Seq, m.Area, m.Text, m.From)
		}
	}
}
```

Apply the analogous change to `orchestrator.go` (subscribe with `SubFilter{Areas: []string{o.cfg.MatrixArea}}`, extract a `readCycle`, select on nudges + fallback). Keep the orchestrator's existing read/classify/delegate body verbatim inside `readCycle`.

> Keep the diff faithful to each file's existing cycle body — move it, don't rewrite it. The ONLY behavioral change is the trigger (nudge + slow fallback vs fast ticker).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -tags push_wiring && go test ./... && go vet ./...`
Expected: PASS; full module green; vet clean.

- [ ] **Step 5: Commit + push + PR**

```bash
git add internal/config/config.go internal/agent/worker.go internal/agent/orchestrator.go internal/agent/push_test.go
git commit -m "feat(agent): wake-on-nudge loop (Matrix push) with slow fallback poll"
git push -u origin <feature-branch>
gh pr create --title "feat: Matrix push (wake-on-update) for worker/orchestrator" --body "..."
```

---

## Self-review notes (controller)

- Spec coverage: hub+match → T1; SSE endpoint+catch-up+heartbeat+auth+tenant → T2; deploy/verify → T3; SSE client+reconnect+cursor-resume → T4; loop trigger+config+fallback → T5. All spec sections covered.
- The blazeagent test harness seam (T5) is the one genuine unknown; the task instructs the implementer to read the existing harness and add the smallest seam rather than invent scaffolding — flagged explicitly, not hidden.
