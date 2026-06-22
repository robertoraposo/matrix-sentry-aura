# Comms Retention + comms_clear · Design Spec

> Date: 2026-06-22 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved → plan → subagent-driven → deploy. Bound the comms channel (append-only, ~666 msgs/day,
> unbounded in-RAM growth) with automatic retention + an explicit area sweep.

## Problem

Comms is append-only with NO cleanup: every `post` accumulates forever in the journal AND in
`comms.Store.entries` (in-RAM). Measured: 605 messages, ~666/day, many areas. The in-RAM index grows without
bound (≈20k/month) and there is no way to clear a finished area. Agents have no cleanup mechanism — and
shouldn't (no delete tool; LLM housekeeping is unreliable). Comms is transient by design (the durable path is
`promote`-to-memory), so cleanup should be automatic, not manual.

## Decisions

- **Automatic retention on the in-RAM index (count ∩ time).** A message stays in the live index only if it is
  BOTH within the last N messages AND newer than T days. Configured via `SENTRY_COMMS_RETAIN_N` (default 2000)
  and `SENTRY_COMMS_RETAIN_DAYS` (default 14); either at 0 disables that knob (both 0 = today's unbounded
  behavior). Applied on `New` (rebuild) and after every `Post`. **The journal is never touched** — it keeps the
  full append-only record (audit); only the live view (read/inbox/dashboard) is bounded. Promote-to-memory
  remains the way to keep anything durable.
- **`comms_clear(area)` — explicit area sweep via a tombstone.** Mirrors memory's `forget`: appends an
  `EventCommsClear` record for the area; the in-RAM index drops that area's messages posted before the clear.
  Survives restart (the rebuild replays clears). Messages posted to the area AFTER a clear survive (point-in-time
  sweep, so an area can be reused). Tenant-scoped (an agent/owner clears only its own tenant's areas).
- **Agents don't clean.** They post + `promote` what matters; retention ages out the rest; `comms_clear` is the
  owner/orchestrator's manual broom for a closed area. The "signal to clean" disappears because it's automatic.

## Architecture (all in `comms` + `cmd/sentrymcp` wiring)

### Component 1 — retention (`comms/comms.go`)

- `Store` gains `retainN int` and `retainAge time.Duration` (zero = that knob off), set by
  `func (s *Store) SetRetention(n int, age time.Duration)`.
- `func (s *Store) prune()` (caller holds `s.mu`): keep entries that pass BOTH knobs — index ≥ `len-retainN`
  (count) AND `TS >= now - retainAge` (time). `entries` is seq-ascending (append order) so the count cut is the
  tail slice; the time cut is a filter. Result reassigned to `s.entries`.
- Called at the end of `New` (after rebuild) and at the end of `Post`. Global across tenants (RAM bound is the
  goal; a single owner-dominated tenant makes per-tenant fairness moot — noted as a limitation).

### Component 2 — comms_clear (`comms/comms.go`)

- `const EventCommsClear sentry.EventType = 7` (5=Message, 6=Recall taken). `type ClearPayload struct { Area string \`json:"area"\` }`.
- `func (s *Store) Clear(tenant sentry.TenantID, area string) (cleared int, err error)`: append
  `EventCommsClear{Area: area}` → get seq `S`; drop in-RAM entries where `Tenant==tenant && Area==area && Seq < S`;
  return count dropped. (Using the clear record's own seq `S` as the cutoff needs no `Before` field.)
- `New` rebuild becomes two passes (like memory): pass 1 scan `EventMessage` → `entries`; pass 2 scan
  `EventCommsClear` → for each clear at seq `S` for `(tenant, area)`, drop entries `Tenant==tenant &&
  Area==area && Seq < S`. Then `prune()`.

### Component 3 — wiring (`cmd/sentrymcp/main.go`)

- After building `s.chat`, call `s.chat.SetRetention(envInt("SENTRY_COMMS_RETAIN_N", 2000),
  time.Duration(envInt("SENTRY_COMMS_RETAIN_DAYS", 14))*24*time.Hour)` (add a small `envInt` helper if absent,
  mirroring `envFloat`). NOTE: `comms.New` must apply retention, so either `New` reads defaults or `main` calls
  `SetRetention` then a re-prune — simplest: `SetRetention` also prunes immediately. Document that `New` starts
  with retention off (0/0) until `SetRetention` is called, so existing `comms.New` callers/tests are unaffected.
- New tool `comms_clear`: args `{area (req)}` → `s.chat.Clear(tenant, area)` → "cleared N messages from <area>".
  Tenant from the per-request dispatch. Description: "Sweep a finished area — drops its messages from the live
  channel (the durable journal is retained). Use for closed coordination areas; promote anything worth keeping
  first."

## Testing (TDD)

- **retention**: post >N messages → index holds only the last N (count); post messages with old TS (inject via
  a low-level append or a settable clock) → those beyond T days are dropped (time); a message must pass BOTH;
  `SetRetention(0,0)` = no pruning. (For the time test, allow injecting `now` or set retainAge tiny and sleep,
  or test `prune` directly with crafted `entries`.)
- **Clear**: post 2 in area X + 1 in Y; `Clear(t, "X")` drops the 2 X (returns 2), Y untouched; a post to X
  AFTER the clear survives; reopening via `New` (which replays the EventCommsClear) still shows X cleared +
  the post-clear message. Tenant isolation: clearing tenant A's area doesn't touch tenant B.
- **cmd/sentrymcp**: `comms_clear` tool round-trip (post → clear → read returns only post-clear); missing area →
  error; tenant-scoped.

## Deployment

Rebuild sentrymcp; redeploy 8808 + 8809. Defaults (N=2000, 14d) bound the live index immediately on restart
(the 605 current messages are all recent, so nothing visible drops yet — retention just caps future growth).
Verify: comms still reads/inboxes; `comms_clear` on a test area works; `/admin/comms` + dashboard intact.

## Out of scope (YAGNI)

- Per-tenant retention quotas (global is enough; owner-dominated).
- Journal compaction/segment rotation (disk is cheap — 605 msgs ≈ 0.9MB; separate concern).
- Per-message delete (area sweep + retention cover the need; promote keeps the keepers).
- Auto-promote-before-clear (the agent/owner promotes explicitly first).
