# Comms Push (wake-on-update) · Design Spec

> Date: 2026-06-29 · Owner: Alvin Nuñez (AlvinTLC) · Repos: `AlvinTLC/matrix-sentry` (server) + `AlvinTLC/blazeagent` (client)
> Status: approved → plan → subagent-driven. A bidirectional-active integration so a blazeagent agent is WOKEN
> on comms activity it cares about (push), instead of fast-polling, while never missing a message.

## Problem

blazeagent's worker and orchestrator run a fast `time.NewTicker(PollInterval)` loop, calling `Inbox`/`Read`
with a persisted cursor, to learn about directed tasks / area activity. Fast polling is wasteful (constant
requests) and adds latency (up to one tick before reacting). We want the Matrix server to PUSH a wake signal
the instant relevant comms activity happens, so the agent reacts immediately and polls only as a safety net.

Constraints from both codebases (they share a philosophy):
- **Pure Go, zero external dependencies, one binary** on BOTH sides.
- Matrix serves a hand-rolled MCP server over `net/http` `ServeMux`; `comms.Store.Post`/`PostImage` append to the
  journal + an in-RAM index, with **no notify hook** today.
- blazeagent's `internal/matrix/client.go` already speaks the MCP POST endpoint and **already parses SSE**
  (`stripSSE`), and persists a cursor (`progreso`).

## Decisions (settled in brainstorming)

- **Subscription scope: configurable** — a subscriber declares interest by `target` AND/OR `areas[]` (covers
  worker's inbox model and orchestrator's area model with one mechanism).
- **Reliability: cursor-resume + replay-missed + slow fallback poll** — on (re)subscribe the agent sends its
  last-seen `since`; the server emits a catch-up nudge if anything matching is newer; on disconnect the agent
  reconnects with its current cursor; a slow fallback poll (~60s) covers any gap. **Zero message loss.**
- **Payload: nudge only** — the stream pushes a minimal event ("activity matching your filter up to #N"); the
  agent then does its existing `Read`/`Inbox(since=cursor)` to fetch + advance. Keeps ALL delivery / formatting
  / dedup / cursor logic in the one tested poll path (DRY); the stream stays minimal.
- **Transport: SSE** (`text/event-stream` + `http.Flusher`) — trivial in stdlib, zero-dep, and the client
  already parses SSE. The "bidirectional" need is "server pushes nudges + client sends via the existing POST
  `/mcp`", which SSE + POST covers; no WebSocket (stdlib has no WS server; framing-by-hand or a dep both break
  zero-dep) and no duplex is needed.
- **No new journal event types; the journal is never touched.** The nudge is an ephemeral in-RAM pub/sub signal
  derived from `Post`/`PostImage`; nothing is journaled. (Contrast with image transfer, which added types 8/9.)
- **blazeagent's message-handling cycle is unchanged** — only its *trigger* changes: a nudge (or a slow
  fallback ticker) fires the same `Read`/`Inbox`→ReAct→advance-cursor cycle that the fast ticker fires today.

## The wire protocol (the contract binding the two repos)

```
GET /comms/subscribe?target=<label>&areas=<a,b,c>&since=<cursor>
Authorization: Bearer <token>            # authenticates → tenant, exactly like /mcp
Response: text/event-stream
  event: nudge
  data: {"seq":N,"area":"…","target":"…","kind":"…"}   # "activity matching your filter, up to #N"
  :hb                                                   # heartbeat comment (~25s): keep-alive + half-open detect
```
- `target` and `areas` are both optional; at least one must be present. A message matches a subscriber when
  same tenant AND (`msg.Target == target` OR `msg.Area ∈ areas`).
- The client sends nothing on this connection (SSE is server→client). Its `post`/`read`/`inbox` keep using the
  existing POST `/mcp` endpoint.
- The nudge is advisory: the agent always reconciles via `Read`/`Inbox(since=its cursor)`, so a duplicate or
  coalesced nudge is harmless.

## Architecture

### Matrix side (sub-project 1) — `comms` hub + SSE endpoint

**Component 1 — in-RAM notify hub (`comms`).** A small pub/sub with its own mutex (separate from `Store.mu`):
- `Subscribe(filter Filter) (<-chan Nudge, func())` — returns a receive channel + a cancel func. `Filter{Tenant,
  Target string, Areas []string}`. Channel is **buffered size 1**.
- `publish(m Message)` — fans out to matching subscribers with a **non-blocking** send (`select { case ch<-n:
  default: }`). A full channel means a nudge is already pending; since a nudge only says "there's something ≥
  #N", coalescing loses nothing (the agent reads everything since its cursor anyway). **`publish` must never
  block `Post`** — `Post`/`PostImage` call it AFTER the journal append, outside `Store.mu`'s critical work.
- `Nudge{Seq uint64, Area, Target, Kind string}`.
- Lock order is always `Store.mu` → `hub.mu` (Subscribe/publish only ever take `hub.mu`), never the reverse →
  no inversion.

**Component 2 — SSE endpoint (`cmd/sentrymcp`).** `mux.HandleFunc("/comms/subscribe", s.handleCommsSubscribe)`:
1. Authenticate via the existing bearer/OAuth path → `tenant` (reject 401 if unauthenticated). Tenant-scoped:
   a subscriber only ever receives its own tenant's nudges.
2. Parse `target`, `areas` (comma-split), `since`. Require at least one of target/areas (400 otherwise).
3. Set SSE headers (`Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`),
   obtain `http.Flusher` (500 if the writer doesn't support flushing).
4. **Catch-up:** scan the in-RAM index for anything matching the filter with `Seq > since`; if found, write one
   `nudge` with the max matching seq, then flush.
5. `Subscribe(filter)` → channel. Loop on `select`:
   - channel → write `event: nudge\ndata: {…}\n\n`, flush.
   - 25s ticker → write `:hb\n\n`, flush (keep-alive + detect dead client via write error → return).
   - `r.Context().Done()` → cancel the subscription, return.

### blazeagent side (sub-project 2) — subscribe client + loop trigger

**Component 3 — `matrix.Cliente.Subscribe`.** `Subscribe(ctx, filter, cursorFn func() int) (<-chan Nudge,
error)`: opens the SSE `GET`, parses `nudge` events into a channel; on disconnect reconnects with exponential
backoff (capped), **re-sending `cursorFn()`** (the current persisted cursor) as `since` so the catch-up covers
the gap. Heartbeat/`:hb` lines are ignored. Honors `ctx` for shutdown.

**Component 4 — loop trigger (`worker.go` / `orchestrator.go`).** The fast `time.NewTicker(PollInterval)`
becomes a `select` over: {nudge channel → run one `Read`/`Inbox(since=cursor)` cycle immediately; **slow**
fallback ticker (`MATRIX_PUSH_FALLBACK`, default 60s) → same cycle; `ctx.Done()` → exit}. The cycle body
(fetch, ReAct, advance cursor via `progreso`) is unchanged.

**Config:** `MATRIX_PUSH` (default `true`; when `false` or the subscribe endpoint is unreachable, fall back to
today's fast polling — backward compatible) and `MATRIX_PUSH_FALLBACK` (fallback poll interval, default 60s).

## Edge cases / reliability

- **Reconnect:** exponential backoff with a cap; every reconnect re-sends the cursor → catch-up nudge closes
  any gap.
- **Heartbeat (~25s):** keeps the Cloudflare tunnel connection alive and lets the client detect a dead
  connection (read error → reconnect).
- **Slow fallback poll:** final safety net — even if the stream dies silently, worst case is ~60s latency,
  never a lost message.
- **Multi-tenant:** the stream is tenant-scoped by the bearer; a subscriber never sees another tenant.
- **publish never blocks Post:** buffered-1 channel + non-blocking send + coalescing.

## Testing (TDD)

- **comms hub:** target-match and area-match deliver; non-match doesn't; a full (slow-consumer) channel does not
  block `publish`/`Post` (coalescing); `cancel()` stops delivery; tenant isolation.
- **SSE endpoint:** 401 without auth; 400 with neither target nor areas; tenant-scoped delivery; catch-up nudge
  emitted when `since < latest matching`; heartbeat written; client disconnect cancels the subscription.
- **blazeagent `Subscribe`:** parses `nudge` events into the channel; ignores `:hb`; reconnects with backoff
  re-sending the cursor; the loop runs a read cycle on a nudge and on the fallback ticker; `MATRIX_PUSH=false`
  uses the legacy fast poll.

## Deployment & sequencing

- **Sequence:** Matrix side first (hub + SSE endpoint) — deployable and testable alone with `curl`. Then
  blazeagent side (it now has an endpoint to connect to).
- Matrix: rebuild sentrymcp, redeploy 8808 (+ 8809 if team agents will subscribe). Additive: one new route, no
  journal change, existing tools untouched. Verify the SSE stream with `curl -N` (catch-up + live nudge +
  heartbeat).
- blazeagent: CI builds the binary; redeploy. With `MATRIX_PUSH=true`, latency drops and poll traffic falls to
  the slow fallback; `MATRIX_PUSH=false` reverts to today's behavior.

## Out of scope (YAGNI)

- WebSocket / full duplex (POST already covers client→server; no need).
- Pushing message payloads over the stream (nudge + existing Read/Inbox is DRY and tested).
- Persisting subscriptions / journaling nudges (ephemeral in-RAM pub/sub by design).
- A new journal event type (none needed).
- Per-subscriber rate limiting / backpressure beyond buffered-1 coalescing (the nudge is idempotent).
