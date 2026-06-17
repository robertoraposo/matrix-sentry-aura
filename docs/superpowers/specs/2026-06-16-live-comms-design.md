# Live Comms in the Dashboard (#comms v2) · Design Spec

> Date: 2026-06-16 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved → plan → subagent-driven → deploy. Wire the dashboard's Comms kanban to the REAL agent
> messages (it currently shows the empty stub even though agents are actively using comms).

## Problem

Agents ARE using the comms channel — 23 real `EventMessage` records on tenant 1 (08-architecture, 09-voice,
orchestrator, … across areas like `ashley/coherence`, `ASHLEY/COMMS/09->08`). But the dashboard's Comms tab is
empty: when live data was wired (v2), `/api/comms` was left as a hardcoded stub `{columns:[],agents:[]}` and
real comms content was deferred. This makes the real-time agent coordination invisible in the UI.

## Decisions

- **New `GET /admin/comms` on sentrymcp** (mirrors `/admin/journal`): scans the tenant's `EventMessage` records
  and returns raw messages `{messages:[{seq,ts,area,from,kind,text,target,ref}]}`, last N (cap 300). Auth +
  tenant via `resolveTenant`. Server-to-server.
- **`sentryadmin /api/comms` becomes real** (replaces the stub): fetches `/admin/comms` with the server-side
  bearer and GROUPS messages into the exact shape the dashboard's `corpus.js` mock produced:
  `{columns:[{key,label,color,cards:[{id,author,type,typeColor,target,text,mins,reply,promotable}]}], agents:[…]}`.
  On upstream error → fall back to the empty `{columns:[],agents:[]}` (never break the UI).
- **No frontend change.** `live.js` already swaps `MatrixCorpus.comms` to the live payload when
  `Array.isArray(live.columns)`; with columns now populated the existing kanban render shows them.
- **Mapping** (real `Message` → dashboard card):
  - column = area (one per distinct area); `label` = the area string, HTML-unescaped (`&gt;`/`&lt;`/`&amp;` →
    `>`/`<`/`&` — real area names got HTML-escaped on post); `color` = dashboard palette by area index;
    `cards` = that area's messages in seq order.
  - card: `id`=seq, `author`=from, `type`= kind mapped (`question`→`pregunta`, `answer`→`respuesta`,
    `info`→`info`, else→`nota`), `typeColor` by type (pregunta `#35E6FF`, respuesta `#34E5A0`, info `#9B6CFF`,
    nota `#7C8AA5`), `target`=target, `text`=text, `mins`= minutes since ts (clamped ≥0), `reply`= ref≠0,
    `promotable`= type≠`pregunta`.
  - `agents` = distinct `from` values.

## Architecture

### Component 1 — `sentrymcp GET /admin/comms` (cmd/sentrymcp/main.go)

Registered next to `/admin/journal`. `resolveTenant`→401. `limit` default 100, cap 300. Scan
`Filter{Tenant:&t, Type:&comms.EventMessage}`, decode `comms.MessagePayload`, emit
`{messages:[{seq,ts(ms),area,from,kind,text,target,ref}]}` (last N). Pure read, tenant-scoped, no CORS.

### Component 2 — `sentryadmin /api/comms` (cmd/sentryadmin/api.go)

Replace `handleComms`'s stub body: GET `{mcpURL}/admin/comms` with the bearer; on error/non-200 → write the
empty `{columns:[],agents:[]}` stub (UI-safe fallback). On success, decode the messages, group + map to the
dashboard shape (Component "Mapping" above), and write it. Add a small `unescapeHTML` + `kindToType`/type-color
helper. The palette already exists in api.go (reuse `palette`).

## Testing (TDD)

- **cmd/sentrymcp** (`/admin/comms`): post (or append) 2 messages in area "X" + 1 in "Y" for a tenant; the
  endpoint returns 3 messages with seq/area/from/kind/text; another tenant's messages are excluded; no-bearer
  (when configured) → 401; `limit` caps.
- **cmd/sentryadmin** (`/api/comms`): against an httptest stub MCP returning a `{messages:[…]}` fixture (areas
  X,Y; kinds question/answer/info), `handleComms` returns `{columns,agents}` where columns group by area, each
  card has the mapped `type`/`typeColor`/`promotable`/`reply`, `agents` lists the distinct froms, and an HTML-
  escaped area label is unescaped. Upstream 500 → the safe empty `{columns:[],agents:[]}` (status 200, UI-safe).

## Deployment

Rebuild sentrymcp (8808+8809) + sentryadmin (server2). Verify with Playwright (local binary vs real 8808):
open the dashboard, switch to the Comms tab, confirm the real areas/messages render (09-voice, orchestrator,
etc.), 0 console errors.

## Out of scope (YAGNI)

- A live poll/refresh of the comms tab (it loads on prime/tenant-switch like the galaxy; a periodic refresh can
  come later if needed).
- Wiring the dashboard's "promote" button to the real backend (it stays a local UI action for now).
- Threading/reply rendering beyond the existing `reply` indent flag.
- Fixing the upstream HTML-escaping of area names at the POST path (cosmetic; unescaped at display here).
