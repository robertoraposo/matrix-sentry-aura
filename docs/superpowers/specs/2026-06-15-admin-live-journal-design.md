# Admin Dashboard — Live Journal (v2.1) · Design Spec

> Date: 2026-06-15 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved → plan → subagent-driven → deploy. Scope: replace the dashboard's SIMULATED journal panel
> with the REAL append-only journal (semantic events) for the configured tenant.

## Problem

After v2, the dashboard's galaxy/inspector/recall/metrics are real, but the **Journal** panel is still the
synthetic event generator (`_simEvent` fabricates "agent-X recall → …" lines from corpus + fake agents). The
owner noticed. The real SentryLog journal has the events; surface them.

## Decisions (settled in brainstorming)

- **Show the SEMANTIC events** — `EventMemory` (3, a memory was stored/superseded), `EventForget` (4,
  tombstone), `EventMessage` (5, agent comms). **Exclude `EventAccess` (1) and `EventPathMap` (2):** Access is
  bulk path-access telemetry (~26k records on the personal journal) that would drown the last-N, and there is
  no cheap id→path reverse lookup (`Registry` only does path→id). The semantic events are the meaningful,
  readable journal. **`recall` is NOT journaled** (read-only), so the real feed has no "recall →" lines — that
  was mock-only; this is the honest trade-off the owner accepted.
- **Same proxy pattern as v2.** `sentrymcp GET /admin/journal` (auth via `resolveTenant`) → `sentryadmin
  GET /api/journal` (gated behind basic-auth, holds the bearer) → frontend.
- **Keep real user-action events.** The component's `pushEvent` calls on select/recall/forget/toggle are
  genuine UI feedback — leave them. Only the ambient SIMULATED stream (`_tickJournal`→`_simEvent`) is replaced.
- **Live feel via polling.** Seed from the real journal on boot/tenant-switch; poll `/api/journal` on the
  existing tick cadence and append only events with a higher seq than already shown (dedupe by seq). Falls back
  to the synthetic generator if the live journal is unavailable (same resilience as v2).

## Architecture

### Component 1 — `sentrymcp GET /admin/journal?limit=N` (cmd/sentrymcp)

- Registered in `serveHTTP`'s mux before the catch-all `/` (next to `/admin/corpus`).
- `resolveTenant(r)` → 401 on `!ok`. `limit` default 60, cap 200.
- `s.store.Scan(sentry.Filter{Tenant: &tenant}, fn)` ascending; for each record switch on `rec.Type`:
  - `memory.EventMemory` → decode `memory.MemoryPayload` → `text = "#<ID> <trunc(Text,80)>"`, type `"Memory"`.
  - `memory.EventForget` → decode `memory.ForgetPayload` → `text = "tombstone #<ID>"`, type `"Forget"`.
  - `comms.EventMessage` → decode `comms.MessagePayload` → type `"Message"`,
    `text = "<From>"` + (` → <Target>` if set) + ` @<Area>: <trunc(Text,60)>`.
  - else: skip.
  Keep only the last N matches (trim the slice as it grows). Return JSON
  `{"events":[{"seq":<n>,"ts":<ms>,"type":"Memory|Forget|Message","text":"…"}]}` with `ts = Tstamp/1e6`.
- Server-to-server (sentryadmin calls it); no CORS.

### Component 2 — `sentryadmin GET /api/journal` (cmd/sentryadmin/api.go)

- `apiServer.handleJournal`: GET `{mcpURL}/admin/journal?limit=N` with the bearer; on upstream error → 502;
  else stream the JSON straight through (it's already the right shape). Gated behind basic-auth in main.go
  (same as `/api/galaxy`).

### Component 3 — frontend (cmd/sentryadmin/assets/live.js + index.html)

- `live.js`: `prime(tenantKey)` additionally fetches `/api/journal?tenant=<k>` into `journalCache[tenantKey]`.
  Expose `MatrixLive.journalEvents(tenantKey)` → the cached `events` array (or `null`), and
  `MatrixLive.fetchJournal(tenantKey)` → async refetch returning fresh `events` (for polling). Both no-throw.
- `index.html`:
  - `_seedJournal()`: if `window.MatrixLive.journalEvents(this.state.tenant)` returns a non-empty array, push
    those real events (oldest→newest) via `pushEvent(ev.type, ev.text)` and remember the max `seq` in
    `this._jSeq`; otherwise run the existing synthetic seed.
  - `_tickJournal()`: if a live journal is active (`this._jSeq != null`), refetch via
    `MatrixLive.fetchJournal(tenant)` and `pushEvent` only events with `seq > this._jSeq` (update `_jSeq`);
    otherwise call the existing `_simEvent()`. Keep the same recurring `setTimeout` cadence so the live feel
    is preserved.
  - On tenant switch, reset `this._jSeq` so the new tenant re-seeds. (The switch already re-primes via v2.)

## Testing (TDD)

- `cmd/sentrymcp` (`/admin/journal`): seed 2 memories + 1 forget on a tenant; the endpoint returns events of
  types Memory/Memory/Forget with the ids in the text; an `EventAccess` record (record_access) does NOT appear;
  `limit` caps the count; no-bearer-when-configured → 401; isolation (another tenant's events absent).
- `cmd/sentryadmin` (`/api/journal`): against an httptest stub MCP returning `{events:[…]}`, `handleJournal`
  passes the events through (count + fields); upstream 500 → 502.
- Frontend: covered by the controller's Playwright check (0 console errors; the journal shows real memory
  ids/text, not the fabricated "agent-X recall →" lines).

## Deployment

1. Rebuild `sentrymcp` + redeploy 8808 (personal) and 8809 (teams) — additive endpoint, ~2s restart.
2. Rebuild `sentryadmin` + redeploy server2.
3. Verify with Playwright (local server against the real MCP, then confirm server2): the Journal panel shows
   real Memory/Forget events (real memory ids/text), 0 console errors, still ticking (poll appends new events).

## Out of scope (YAGNI / follow-ups)

- Surfacing `EventAccess` with resolved paths (needs an id→path reverse index in `Registry`).
- Server-Sent-Events push for the journal (polling is enough at this cadence).
- Real per-event author/agent attribution for Memory/Forget (the journal records id + text, not the writing
  client).
