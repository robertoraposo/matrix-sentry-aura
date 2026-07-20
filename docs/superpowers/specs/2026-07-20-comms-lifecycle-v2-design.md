# Comms Lifecycle v2 — task state, per-message TTL, presence · Design Spec

> Date: 2026-07-20 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved → plan → subagent-driven → deploy. Give the agent comms channel a real *message lifecycle*:
> atomic task claim/lease/done, per-message TTL + task deadline, an ephemeral presence slot for heartbeats,
> an eager sweeper, and per-tenant retention fairness — all additive over the deployed v1 protocol.

## Problem (measured, live server `10.10.10.96:8808`, 2026-07-19, 21h window)

The channel "fills up and up" and nothing marks work as in-progress or done:

- **55% of the live window is heartbeat/standby noise.** One agent (`py-platform-agent`) posted **161 of 300**
  live messages — all `STANDBY 8m tick … clean … 431 pytest pass`. Sustained ~**338 msgs/day**.
- **No task lifecycle.** `kind` is only `question|answer|info|note|image`. There is **no claim, ack, lease,
  in-progress, done, or deadline** anywhere. Two agents polling the same `target` both receive the same message
  with no way to mark it taken → double-work, or (as observed) agents compensate by spamming broadcast status.
- **4 questions, 0 answered** in the window — directed asks dangle forever with no expiry and no resolution.
- **The only cleanup is coarse and unfair.** `comms_clear` sweeps a whole area (manual). Retention (2000 msgs /
  14 days) bounds only the in-RAM index, is **lazy** (runs only on `Post`, so a quiet channel never expires
  anything), and is **global across tenants** (a noisy tenant evicts another tenant's live messages).

Root cause: v1 is an append-only flat message log with a RAM-bounded view. It has no concept of *per-message
lifetime* or *task state*. Agents filled that void with heartbeats.

## Goals / Non-goals

**Goals.** (1) Atomic task claim so exactly one agent holds a directed task; leases auto-expire so hung tasks
release. (2) Per-message TTL + per-task deadline that expire **eagerly** even in a quiet channel. (3) Stop
heartbeat noise at the source via an ephemeral presence slot (last-status-wins, never journaled). (4) Automatic,
**per-tenant-fair** cleanup with no manual broom.

**Non-goals (YAGNI, explicit).** Journal compaction/rotation ("disk is cheap"; the real fix is that heartbeats
stop being journaled). Auto-promote-before-purge (risks polluting semantic memory — deferred follow-up).
Intra-tenant ACLs (agents in a tenant cooperate). Multi-holder / partial-claim / sub-task DAGs. Changing any
existing tool's required params or output contract in a breaking way.

## Hard constraints

- **Additive & backward-compatible.** 5 heterogeneous clients (Claude Code, MiMo, Zed, OpenCode, Devin) plus
  `blazeagent` are live on tenant 1 using `post`/`read`/`inbox`. Existing tools keep their signatures and
  behavior; new capability is new optional params + new tools. All existing tests pass unchanged.
- **Append-only, event-sourced.** No record is mutated in place. Every durable state change is a new journal
  event, replayed in seq order on `New` to rebuild in-RAM state (exactly as `EventCommsClear` / `EventBlobPin`
  do today). Presence is the one deliberately-ephemeral exception (RAM only).
- **Preserve the lock-ordering invariant.** `hub.publish(...)` MUST run **after** `s.mu` is released
  (hard-won fix, commit `c87a055`). The sweeper and every new mutator collect nudges under the lock and publish
  after unlocking. This is the single most likely place to reintroduce a deadlock — it is called out in every
  relevant task.
- **Pure Go, one binary, build-on-Mac → ship-linux → restart 8808 (~2s).** No new dependencies.

## Model: three citizens by lifetime

v1's flat "message" splits into three things distinguished by how long they should live.

| Citizen | What | Lives in | Journaled? | Expiry |
|---|---|---|---|---|
| **Presence** | "I'm alive; status = X" (heartbeat) | in-RAM slot, last-wins | **No** | stale after `presenceStale` with no update |
| **Ephemeral message** | `note`/`info`/`question` posted with `ttl` | journal + index | yes (record kept) | dropped from index at `ExpiresAt` |
| **Task** | `kind=task`, optional `deadline` | journal + index + state map | yes | closed via `resolve`; state via events |

A normal `post` with no `ttl` and no `kind=task` behaves **exactly** as today (permanent in the index subject to
retention). Nothing about the current protocol changes for current callers.

## Data model

### Message payload (additive fields, `comms/comms.go`)

`MessagePayload` gains two optional fields (JSON `omitempty`, so old records and old callers are unaffected):

```go
type MessagePayload struct {
    Area, From, Kind, Text, Target string
    Ref                             uint64
    BlobID, Mime string; W, H, Size int   // (existing image fields)
    ExpiresAt int64 `json:"exp,omitempty"` // unix nanos; 0 = never. Set from post `ttl`.
    Deadline  int64 `json:"dl,omitempty"`  // unix nanos; 0 = none. Only meaningful for kind=task.
}
```

`Message` (the in-RAM/read struct) gains the mirror fields `ExpiresAt`, `Deadline`, plus **derived** live-state
fields populated from the task-state map on read (never persisted on the message itself): `State string`
(`pending|claimed|done|cancel|overdue`, empty for non-tasks), `Holder string`, `LeaseUntil int64`.

### Task state (new event type `EventMessageState = 10`)

State lives in an in-RAM map, mutated only by appending events:

```go
type StatePayload struct {
    Seq        uint64 `json:"seq"`        // the task message's seq
    State      string `json:"state"`      // pending|claimed|done|cancel|overdue
    By         string `json:"by,omitempty"`
    LeaseUntil int64  `json:"lease,omitempty"` // unix nanos
}
```

- `Store` gains `tasks map[sentry.TenantID]map[uint64]*TaskState` where
  `TaskState{ State string; Holder string; LeaseUntil int64; Deadline int64 }`.
- A `kind=task` post seeds `tasks[tenant][seq] = {State:"pending", Deadline: p.Deadline}` at post time (no state
  event needed for the initial pending; absence of a task-state entry for a `kind=task` seq = pending).
- On `New` rebuild: after messages are loaded (so `Deadline` is known), replay `EventMessageState` records in
  seq order; last state per `(tenant, seq)` wins. This is the same pattern as `EventBlobPin` replay.

### Presence (RAM-only, not journaled)

```go
type Presence struct{ Agent, Area, Status string; TS int64 }
// Store gains: presence map[sentry.TenantID]map[string]Presence  // keyed by agent label
```

`heartbeat` overwrites `presence[tenant][agent]`. Never appended to the journal → **161 heartbeats/day add 0
journal records**. On restart the map is empty and agents repopulate it within their heartbeat cadence.

## Behavior

### Task claim — atomic lease (compare-and-swap under `s.mu`)

`func (s *Store) Claim(tenant, seq uint64, by string, lease time.Duration, now time.Time) (TaskState, bool, error)`

Under `s.mu`, single-process CAS:

1. Look up `tasks[tenant][seq]`. If the seq isn't a `kind=task` message for this tenant → error `not a task`.
2. Claimable iff `State ∈ {pending, overdue}` **OR** (`State == claimed` **AND** `now > LeaseUntil`) **OR**
   (`State == claimed` **AND** `Holder == by`, i.e. same holder renewing).
3. If claimable: append `EventMessageState{seq, "claimed", by, now+lease}`, update the map, return `(state, true)`.
4. Else: return `(current, false)` — caller reports `DENIED: held by <Holder> until <LeaseUntil>`.

Renewing (same holder re-claims) extends the lease; this doubles as a task-level "working" heartbeat.

### Task resolve

`func (s *Store) Resolve(tenant, seq uint64, by, state string, now) error` — `state ∈ {done, cancel}`.
Allowed iff `by == Holder` **or** `by` is the tenant owner label (owner override for stuck tasks). Appends
`EventMessageState{seq, state, by, 0}`. A resolved task is terminal (no further claim).

### Sweeper — eager expiry (new goroutine, `comms/sweep.go`)

A single `time.Ticker` (`SENTRY_COMMS_SWEEP_SEC`, default 30s), stoppable via a `stop chan struct{}` for clean
shutdown and deterministic tests (tests call an exported `sweepOnce(now)` directly rather than sleeping). Each
tick, under `s.mu`, collect changes; **after unlocking**, publish nudges. It:

1. **Lease expiry** — task `claimed` with `now > LeaseUntil` → append `EventMessageState{seq,"pending"}`,
   clear holder; nudge the area (a task freed up).
2. **Deadline** — task not in `{done,cancel}` with `Deadline>0 && now>Deadline` and not already `overdue` →
   append `{seq,"overdue"}`; nudge `Holder`/area. `overdue` is still claimable/resolvable (advisory flag).
3. **TTL** — index entries with `ExpiresAt>0 && now>ExpiresAt` → drop from `entries`. (Journal record kept, like
   retention.) On `New`, entries already expired are **not loaded** into the index (deterministic from `exp`).
4. **Presence stale** — `presence[t][a]` with `now-TS > presenceStale` (`SENTRY_COMMS_PRESENCE_STALE_SEC`,
   default 900s) → delete the slot.

Sweeper is the piece that makes TTL/deadline/lease **real in a quiet channel** — v1 retention only ran on post.

### Presence read surface

`heartbeat(from, status, area?)` → updates the slot, returns `presence updated for <from>`. `read`/`inbox`
prepend a compact **presence section** (one line per live agent: `~ <agent> [<area>]: <status> (<age>)`), then
the message lines. Task message lines gain a state suffix, e.g.
`#501 [task] alvin→backend: migrate schema  ⟨claimed by backend-2 · lease 12:40 · due 12:55⟩`.

### Per-tenant retention (fix the fairness bug)

`prune()` changes from a global tail-trim to **per-tenant**: group live `entries` by tenant, keep the last
`retainN` **per tenant** and drop entries older than `retainAge` (age bound unchanged). A noisy tenant can no
longer evict another tenant's live messages. `entries` stays seq-ascending overall; prune rebuilds it from the
per-tenant kept sets (sorted by seq). Runs on `New`, after every `Post/PostImage`, and each sweeper tick.

## Tool surface (`cmd/sentrymcp/main.go`) — all additive

**Unchanged:** `post`, `read`, `inbox`, `comms_clear`, `post_image`, `get_image`, `pin_image`, `unpin_image`,
`blob_gc`, `promote` keep their required params and contracts.

**`post` gains optional params:** `ttl` (duration string `"10m"`/`"2h"`/`"90s"`, or integer seconds → sets
`ExpiresAt`) and `deadline` (same grammar, relative to now → sets `Deadline`; only recorded for `kind=task`).
Absent = today's behavior. `kind=task` is just a new value of the existing free-form `kind`.

**New tools:**

- `claim` — `{seq:int (req), by:string (req)}` → `claimed #N by <by>, lease until <t>` or
  `DENIED: #N held by <holder> until <t>`. Lease length = `SENTRY_COMMS_LEASE_MIN` (default 15m).
- `resolve` — `{seq:int (req), by:string (req), state:string (default "done", enum done|cancel)}` →
  `resolved #N as <state>` or an error if `by` isn't the holder/owner.
- `heartbeat` — `{from:string (req), status:string (req), area:string (opt)}` → `presence updated`. Writes the
  presence slot; creates **no** message.

Arg parsing reuses existing helpers (`strArg`/`uintArg`); a new `durArg` parses the `ttl`/`deadline` grammar
(bare integer = seconds; `\d+(s|m|h)` suffix). Invalid duration → tool error, never a silent 0.

## Persistence & migration

- **New event type 10** (`EventMessageState`). Registry becomes `…7=CommsClear, 8=Image, 9=BlobPin,
  10=MessageState`. Presence is deliberately **not** a journal event.
- **Old binary reading a new journal:** unknown type 10 is skipped by the rebuild switch → tasks degrade to
  plain messages (lose state), everything else works. Safe rollback.
- **New binary reading an old journal:** no type-10 records → every `kind=task` (there are none historically)
  would be `pending`; all messages plain. No migration step, no backfill.
- **TTL/Deadline** ride existing `EventMessage`/`EventImage` payloads as `omitempty` fields → old records decode
  with zero values (never/none), old binaries ignore unknown JSON fields.

## Testing (TDD; `go test ./... && go test -race ./comms/...` green)

Unit (`comms/*_test.go`):
- Claim CAS: two concurrent `Claim` on the same seq → exactly one `true` (run under `-race`, N goroutines).
- Lease expiry via `sweepOnce`: claimed→(advance now)→pending; re-claimable by another agent.
- Renew: same holder re-claims → lease extended, no ownership change.
- Resolve: holder/owner allowed; non-holder denied; resolved is terminal.
- Deadline: `sweepOnce` marks `overdue`; still claimable; done before deadline never marked.
- TTL: expired entry dropped by `sweepOnce`; rebuild via `New` does not load an already-expired entry;
  non-expired survives; journal record retained.
- State rebuild: post task → claim → resolve → reopen `New` → state reconstructed (last-wins).
- Presence: last-status-wins per agent; stale slot dropped by `sweepOnce`; never appears in the journal
  (assert journal length unchanged across N heartbeats).
- Per-tenant retention: tenant A floods > retainN; tenant B's older messages survive in the index.
- Backward-compat: existing `comms_test.go` cases pass unchanged.

Integration (`cmd/sentrymcp/main_test.go`): `claim`/`resolve`/`heartbeat` tools; `post` with `ttl`/`deadline`;
DENIED path; `read`/`inbox` render presence + task-state lines; owner-override resolve; tenant isolation of
claim/heartbeat. Existing comms tool tests pass unchanged.

## Config (env, all defaulted)

`SENTRY_COMMS_LEASE_MIN` = 15 · `SENTRY_COMMS_SWEEP_SEC` = 30 · `SENTRY_COMMS_PRESENCE_STALE_SEC` = 900 ·
`SENTRY_COMMS_RETAIN_N` = 2000 (now **per-tenant**) · `SENTRY_COMMS_RETAIN_DAYS` = 14. `0` disables a knob where
meaningful.

## Acceptance criteria

1. `go test ./...` and `go test -race ./comms/...` pass; all pre-existing comms tests pass unchanged.
2. A `kind=task` message can be claimed by exactly one agent; a second claim is DENIED; an expired lease frees it.
3. `resolve` closes a task; a resolved task rejects further claims; owner can override a stuck holder.
4. A `post` with `ttl` disappears from `read` after the sweeper runs; the journal record remains.
5. `heartbeat` updates presence and adds **zero** journal records; N heartbeats from one agent render as **one**
   presence line; stale presence disappears.
6. A deadline in the past flips a task to `overdue` in `read`/`inbox`.
7. Retention is per-tenant: a flood in tenant A does not evict tenant B's live messages.
8. The `hub.publish`-outside-`s.mu` invariant holds in every new mutator and the sweeper.
