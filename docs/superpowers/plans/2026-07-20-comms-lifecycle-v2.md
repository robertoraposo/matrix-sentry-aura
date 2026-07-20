# Comms Lifecycle v2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the agent comms channel a real message lifecycle — atomic task claim/lease/done, per-message TTL + task deadline, an ephemeral presence slot for heartbeats, an eager sweeper, and per-tenant retention — all additive over the deployed v1.

**Architecture:** Event-sourced over the existing SentryLog journal. Durable state changes are new journal events (`EventMessageState = 10`) replayed on `New` (like `EventBlobPin`); presence is RAM-only. A single-process `sync.Mutex` on `comms.Store` makes the task-claim compare-and-swap trivially correct. A sweeper goroutine makes TTL/deadline/lease expiry eager. All new capability is new optional params + new MCP tools.

**Tech Stack:** Pure Go (stdlib only), `matrixsentry/sentry` journal, `matrixsentry/comms` package, `cmd/sentrymcp` MCP server.

## Global Constraints

- **Pure Go, one binary, zero new dependencies.** Build on Mac → ship linux → restart 8808 (~2s).
- **Additive & backward-compatible.** Existing tools (`post`/`read`/`inbox`/`comms_clear`/`post_image`/…) keep signatures, behavior, and output contracts. All pre-existing tests pass unchanged. New payload fields are JSON `omitempty`.
- **Append-only, event-sourced.** No record mutated in place. Durable state = new journal events replayed in seq order on `New`; last-wins per key. Presence is the only deliberately-ephemeral (RAM-only) state.
- **Lock-ordering invariant (do not regress).** `s.hub.publish(...)` MUST run **after** `s.mu` is released (commit `c87a055`). Every new mutator and the sweeper collect nudges under the lock and publish after unlocking.
- **Event-type registry:** `1=Access 2=PathMap 3=Memory 4=Forget 5=Message 6=Recall 7=CommsClear 8=Image 9=BlobPin` → add `10=MessageState`. Presence is NOT a journal event.
- **Config env (all defaulted):** `SENTRY_COMMS_LEASE_MIN=15`, `SENTRY_COMMS_SWEEP_SEC=30`, `SENTRY_COMMS_PRESENCE_STALE_SEC=900`, `SENTRY_COMMS_RETAIN_N=2000` (now per-tenant), `SENTRY_COMMS_RETAIN_DAYS=14`.
- **Test gate each task:** `go build ./...` green; `go test ./comms/... ./cmd/sentrymcp/...` green; leave the tree buildable at every commit.

---

## File Structure

| File | Owner task | Responsibility |
|---|---|---|
| `comms/comms.go` | Task 1 | New payload/Message fields; `EventMessageState`; new types (`StatePayload`, `TaskState`, `Presence`); `Store` fields (`tasks`, `presence`, `leaseTTL`, `presenceStale`); `New` init; **per-tenant** `pruneAt`; TTL `isExpired` + `New` load-skip. |
| `comms/state.go` (new) | Task 2 | `Claim`, `Resolve`, `seedTaskLocked`, `replayStateLocked`; `Post` seeds `pending` for `kind=task`; `New` calls `replayStateLocked`. |
| `comms/presence.go` (new) | Task 3 | `Heartbeat`, `PresenceList`, `pruneStalePresenceLocked`. |
| `comms/sweep.go` (new) | Task 4 | `sweepOnce(now)`, `Start(ctx)`, `Stop()`; lease-expiry flip, deadline→overdue, TTL drop, presence-stale — nudge-collect-then-publish. |
| `cmd/sentrymcp/main.go` | Tasks 5-6 | `durArg`; `post` gains `ttl`/`deadline`; new tools `claim`/`resolve`/`heartbeat`; `read`/`inbox` presence + task-state rendering; startup sweeper + env wiring. |
| `comms/*_test.go`, `cmd/sentrymcp/main_test.go` | each task | Tests colocated with their task. |

**Sequencing rule for the orchestra:** Tasks that touch `comms.go` (1) and `main.go` (5,6) are **strictly sequential** (shared file — parallel edits clobber). New-file tasks (2,3,4) run after Task 1 and are each left tree-green. Verification is a separate read-only phase after all implementation.

---

## Task 1: Foundations — payload/Store fields, types, per-tenant retention, TTL skip (`comms/comms.go`)

**Files:**
- Modify: `comms/comms.go`
- Test: `comms/comms_test.go` (add cases; do not alter existing)

**Interfaces:**
- Produces:
  - `const EventMessageState sentry.EventType = 10`
  - `MessagePayload` + `Message` gain `ExpiresAt int64` (json `exp,omitempty`) and `Deadline int64` (json `dl,omitempty`); `message(...)` copies both.
  - `type StatePayload struct { Seq uint64 \`json:"seq"\`; State string \`json:"state"\`; By string \`json:"by,omitempty"\`; LeaseUntil int64 \`json:"lease,omitempty"\` }`
  - `type TaskState struct { State, Holder string; LeaseUntil, Deadline int64 }`
  - `type Presence struct { Agent, Area, Status string; TS int64 }`
  - `Store` fields: `tasks map[sentry.TenantID]map[uint64]*TaskState`, `presence map[sentry.TenantID]map[string]Presence`, `leaseTTL time.Duration`, `presenceStale time.Duration`.
  - `func (s *Store) isExpired(m Message, now int64) bool` → `m.ExpiresAt > 0 && now > m.ExpiresAt`.
  - `pruneAt(now int64)` reworked to keep the last `retainN` **per tenant** (age bound unchanged).

- [ ] **Step 1: Failing tests.** In `comms_test.go`: (a) `TestMessageCarriesTTLAndDeadline` — `message(1, 1, ts, MessagePayload{Area:"a",From:"f",Text:"t",ExpiresAt:99,Deadline:88})` returns a `Message` with `ExpiresAt==99 && Deadline==88`. (b) `TestPerTenantRetention` — a `Store` with `retainN=2`: append 5 entries for tenant 1 and 1 older entry for tenant 2 (lower seq), call `pruneAt(now)`; assert tenant 2's entry SURVIVES and tenant 1 keeps its last 2. (c) `TestNewSkipsExpiredEntry` — write one `EventMessage` with `ExpiresAt` in the past and one without via the journal, `New`, assert only the non-expired is in the index (use `Recent`).
- [ ] **Step 2: Run → FAIL** (`go test ./comms/ -run 'TTL|Retention|Expired' -v`).
- [ ] **Step 3: Implement.** Add const, fields, types, `isExpired`. Init `tasks`/`presence` maps in `New`. In `New`'s message/image load callbacks, skip records where `isExpired(msg, time.Now().UnixNano())`. Rewrite `pruneAt`:

```go
func (s *Store) pruneAt(now int64) {
    if s.retainN <= 0 && s.retainAge <= 0 { return }
    var cutoff int64
    if s.retainAge > 0 { cutoff = now - int64(s.retainAge) }
    // count kept-per-tenant walking newest→oldest so the tail (highest seq) wins.
    perTenant := map[sentry.TenantID]int{}
    keep := make([]bool, len(s.entries))
    for i := len(s.entries) - 1; i >= 0; i-- {
        m := s.entries[i]
        if cutoff > 0 && m.TS < cutoff { continue }
        if s.retainN > 0 && perTenant[m.Tenant] >= s.retainN { continue }
        perTenant[m.Tenant]++
        keep[i] = true
    }
    out := s.entries[:0]
    for i, m := range s.entries { if keep[i] { out = append(out, m) } }
    s.entries = out
}
```

- [ ] **Step 4: Run → PASS**; also `go test ./comms/...` (all existing green).
- [ ] **Step 5: Commit** — `git commit -m "feat(comms) lifecycle v2 foundations: ttl/deadline fields, state/presence types, per-tenant retention"`.

---

## Task 2: Task state machine — claim/resolve/lease + rebuild replay (`comms/state.go`)

**Files:**
- Create: `comms/state.go`
- Modify: `comms/comms.go` (`Post` seeds pending for `kind=task`; `New` calls `replayStateLocked`)
- Test: `comms/state_test.go`

**Interfaces:**
- Consumes: Task 1's `EventMessageState`, `StatePayload`, `TaskState`, `Store.tasks`, `Store.leaseTTL`.
- Produces:
  - `func (s *Store) Claim(tenant sentry.TenantID, seq uint64, by string, now time.Time) (TaskState, bool, error)` — atomic CAS; `ok=false` = DENIED.
  - `func (s *Store) Resolve(tenant sentry.TenantID, seq uint64, by, state string, now time.Time) error` — `state ∈ {done,cancel}`; holder or owner only.
  - `func (s *Store) TaskOf(tenant sentry.TenantID, seq uint64) (TaskState, bool)` — read live state (used by read/inbox rendering).
  - `func (s *Store) SetOwnerLabel(label string)` — the label allowed to override `Resolve` (default `""` = only holder).
  - unexported: `seedTaskLocked(tenant, seq, deadline)`, `replayStateLocked()`.

- [ ] **Step 1: Failing tests** in `state_test.go`:
  - `TestClaimSingleWinner` — post `kind=task`, launch N=50 goroutines all `Claim` at the same `now`; exactly one returns `ok=true` (run whole package under `-race`).
  - `TestClaimLeaseSteal` — A claims with `now=t0` (lease `t0+leaseTTL`); B `Claim` at `now=t0+leaseTTL+1ns` succeeds and Holder becomes B.
  - `TestClaimRenewByHolder` — A claims; A `Claim` again at `now=t0+1m` → still A, LeaseUntil extended.
  - `TestResolveHolderOnly` — non-holder `Resolve` → error; holder `Resolve("done")` → ok; further `Claim` → DENIED (terminal).
  - `TestResolveOwnerOverride` — `SetOwnerLabel("alvin")`; `Resolve` by `"alvin"` on a task held by someone else → ok.
  - `TestStateRebuild` — post task → claim → resolve; `New` on the same journal → `TaskOf` shows terminal `done` (last-wins replay).
- [ ] **Step 2: Run → FAIL.**
- [ ] **Step 3: Implement** `state.go`. `Claim` sketch (mutator pattern — collect nudge, publish after unlock):

```go
func (s *Store) Claim(tenant sentry.TenantID, seq uint64, by string, now time.Time) (TaskState, bool, error) {
    if by == "" { return TaskState{}, false, fmt.Errorf("comms: 'by' required to claim") }
    s.mu.Lock()
    ts, ok := s.tasks[tenant][seq]
    if !ok { s.mu.Unlock(); return TaskState{}, false, fmt.Errorf("comms: #%d is not a task for this tenant", seq) }
    nn := now.UnixNano()
    claimable := ts.State == "pending" || ts.State == "overdue" ||
        (ts.State == "claimed" && (nn > ts.LeaseUntil || ts.Holder == by))
    if !claimable { cur := *ts; s.mu.Unlock(); return cur, false, nil }
    ts.State, ts.Holder, ts.LeaseUntil = "claimed", by, nn+int64(s.leaseTTL)
    s.appendStateLocked(tenant, seq, ts) // journals EventMessageState
    cur := *ts
    s.mu.Unlock()
    return cur, true, nil
}
```

  `appendStateLocked` appends `EventMessageState` (caller holds `s.mu`). `Resolve` sets terminal state under lock. Add `s.replayStateLocked()` at the end of `New` (after messages/clears loaded so `Deadline` is known); it `journal.Scan`s `EventMessageState` in seq order applying last-wins into `s.tasks`. Wire `Post`: after appending, if `p.Kind == "task"`, `seedTaskLocked(tenant, seq, p.Deadline)`.
- [ ] **Step 4: Run → PASS** incl. `go test -race ./comms/...`.
- [ ] **Step 5: Commit** — `git commit -m "feat(comms) atomic task claim/lease/resolve via EventMessageState + rebuild replay"`.

---

## Task 3: Presence — ephemeral heartbeat slot (`comms/presence.go`)

**Files:**
- Create: `comms/presence.go`
- Test: `comms/presence_test.go`

**Interfaces:**
- Consumes: Task 1's `Store.presence`, `Presence`, `presenceStale`.
- Produces:
  - `func (s *Store) Heartbeat(tenant sentry.TenantID, agent, area, status string, now time.Time) error` — overwrites the slot; never journals.
  - `func (s *Store) PresenceList(tenant sentry.TenantID) []Presence` — live slots, sorted by agent.
  - unexported `pruneStalePresenceLocked(now int64)`.

- [ ] **Step 1: Failing tests** in `presence_test.go`:
  - `TestHeartbeatLastWins` — 5 heartbeats from agent `x` → `PresenceList` has exactly 1 entry with the latest status.
  - `TestHeartbeatNotJournaled` — capture `journal` length, 10 heartbeats, assert length unchanged (heartbeats add zero records).
  - `TestPresenceTenantScoped` — tenant 1's heartbeat not visible to tenant 2.
  - `TestPresenceStaleDropped` — heartbeat at `t0`; `pruneStalePresenceLocked(t0 + presenceStale + 1)` removes it.
- [ ] **Step 2: Run → FAIL.**
- [ ] **Step 3: Implement.** `Heartbeat` requires `agent` and `status`; under `s.mu` sets `s.presence[tenant][agent] = Presence{agent, area, status, now.UnixNano()}`. `PresenceList` copies + sorts. No `hub.publish`, no `journal.Append`.
- [ ] **Step 4: Run → PASS.**
- [ ] **Step 5: Commit** — `git commit -m "feat(comms) ephemeral presence slot (heartbeat/PresenceList) — never journaled"`.

---

## Task 4: Sweeper — eager lease/deadline/TTL/presence expiry (`comms/sweep.go`)

**Files:**
- Create: `comms/sweep.go`
- Test: `comms/sweep_test.go`

**Interfaces:**
- Consumes: Tasks 1-3 (`isExpired`, `tasks`, `presence`, `appendStateLocked`, `pruneStalePresenceLocked`, `hub`).
- Produces:
  - `func (s *Store) sweepOnce(now time.Time) []Message` — returns nudges to publish (for tests + the goroutine); does NOT publish inside the lock.
  - `func (s *Store) Start(ctx context.Context, interval time.Duration)` — ticker goroutine: each tick `nudges := s.sweepOnce(now); for _, n := range nudges { s.hub.publish(n) }`; stops on `ctx.Done()`.

- [ ] **Step 1: Failing tests** in `sweep_test.go`:
  - `TestSweepExpiresLease` — claim at `t0`; `sweepOnce(t0+leaseTTL+1)` flips state to `pending`, clears Holder.
  - `TestSweepMarksOverdue` — post task `Deadline=t0`; `sweepOnce(t0+1)` → state `overdue`; a task with future deadline untouched; a `done` task never marked.
  - `TestSweepDropsTTL` — post with `ExpiresAt=t0`; `sweepOnce(t0+1)` removes it from the index (`Read` no longer returns it); journal length unchanged.
  - `TestSweepDropsStalePresence` — stale slot removed.
  - `TestSweepPublishesOutsideLock` — subscribe to the hub; `Start` a sweeper; trigger a lease expiry; receive a nudge without deadlock (bounded timeout).
- [ ] **Step 2: Run → FAIL.**
- [ ] **Step 3: Implement.** `sweepOnce`: `s.mu.Lock()`; walk `tasks` (lease→pending, deadline→overdue, appending state events + collecting area nudges), drop `isExpired` entries from `s.entries`, `pruneStalePresenceLocked(now)`, `pruneAt(now)`; `s.mu.Unlock()`; return collected nudges. `Start`: `context`+`time.Ticker`; publish returned nudges after unlock. **Never call `hub.publish` while holding `s.mu`.**
- [ ] **Step 4: Run → PASS** incl. `-race`.
- [ ] **Step 5: Commit** — `git commit -m "feat(comms) eager sweeper: lease/deadline/ttl/presence expiry, publish-outside-lock"`.

---

## Task 5: MCP — post ttl/deadline + claim/resolve/heartbeat tools (`cmd/sentrymcp/main.go`)

**Files:**
- Modify: `cmd/sentrymcp/main.go` (tools list ~717-765; dispatch ~1025-1123; arg helpers ~1334+)
- Test: `cmd/sentrymcp/main_test.go`

**Interfaces:**
- Consumes: `comms.Store.Claim/Resolve/Heartbeat`, `MessagePayload.ExpiresAt/Deadline`.
- Produces: `func durArg(args map[string]any, key string, now time.Time) (int64, bool, error)` (unix-nanos absolute from a relative duration; `""`→(0,false,nil); `"90s"|"10m"|"2h"|bare-int-seconds`→(abs,true,nil); junk→error). New tools `claim`, `resolve`, `heartbeat`.

- [ ] **Step 1: Failing tests** in `main_test.go`:
  - `TestPostTTLSetsExpiry` — `post` with `ttl:"10m"` → the stored message has `ExpiresAt≈now+10m` (assert via `Read`/`GetBySeq`).
  - `TestClaimToolAtomic` — `post kind=task`; `claim` by A → "claimed"; `claim` by B → "DENIED".
  - `TestResolveTool` — holder `resolve` done → ok; further claim DENIED.
  - `TestHeartbeatToolNoMessage` — `heartbeat` → "presence updated"; `Recent` count unchanged (no new message).
  - `TestDurArg` — table: `""`→none, `"90s"/"10m"/"2h"`, `"5"`→5s, `"nope"`→error.
- [ ] **Step 2: Run → FAIL.**
- [ ] **Step 3: Implement.** Add `durArg`. Extend `post` handler to parse `ttl`/`deadline` and set payload fields. Add `claim`/`resolve`/`heartbeat` to the tools list (schemas per spec §Tool surface) and dispatch cases; format `LeaseUntil`/`Deadline` with `time.Unix(0, x).Format("15:04")`. Reuse `moko.Info` logging. Owner label via `s.tenant`/`SetOwnerLabel` wiring done in Task 6.
- [ ] **Step 4: Run → PASS.**
- [ ] **Step 5: Commit** — `git commit -m "feat(sentrymcp) post ttl/deadline params + claim/resolve/heartbeat tools"`.

---

## Task 6: MCP — read/inbox rendering + startup sweeper & env wiring (`cmd/sentrymcp/main.go`)

**Files:**
- Modify: `cmd/sentrymcp/main.go` (read ~1043-1080; inbox ~1091-1123; startup ~124-144)
- Test: `cmd/sentrymcp/main_test.go`

**Interfaces:**
- Consumes: `comms.Store.PresenceList/TaskOf/Start/SetOwnerLabel`, env helpers `envInt`.

- [ ] **Step 1: Failing tests** in `main_test.go`:
  - `TestReadShowsPresenceAndTaskState` — heartbeat + post `kind=task` + claim; `read` output contains a `~ <agent>` presence line and a `⟨claimed by …⟩` suffix on the task line.
  - `TestReadBackwardCompatPlainMessage` — a plain `note` renders exactly as before (`#N [note] from→all: text`) — guards the existing contract.
- [ ] **Step 2: Run → FAIL.**
- [ ] **Step 3: Implement.** In `read`/`inbox`, prepend `PresenceList` lines (`~ agent [area]: status (age)`), and for messages where `TaskOf` is present, append the state suffix. Keep the plain-message format byte-identical when there's no presence and no task state. In `main` startup: parse the new env vars, `s.chat.SetOwnerLabel(...)`, and `s.chat.Start(ctx, sweepInterval)` using the server's lifecycle context; ensure `SetRetention` now documents per-tenant N.
- [ ] **Step 4: Run → PASS**; then `go test ./... && go test -race ./comms/...`.
- [ ] **Step 5: Commit** — `git commit -m "feat(sentrymcp) render presence + task state in read/inbox; start sweeper + lifecycle env"`.

---

## Self-Review notes (author)

- **Spec coverage:** claim/lease (T2,T5) · resolve+owner override (T2,T5) · deadline→overdue (T4) · TTL (T1 skip, T4 drop, T5 post-param) · presence (T3,T5,T6) · sweeper (T4,T6) · per-tenant retention (T1) · new event type 10 (T1,T2) · backward-compat (T6 guard + Global Constraints) · lock-order (T2,T4 Interfaces + Global Constraints). All spec §§ map to a task.
- **Type consistency:** `Claim/Resolve/Heartbeat/TaskOf/Start/SetOwnerLabel`, `TaskState{State,Holder,LeaseUntil,Deadline}`, `StatePayload{Seq,State,By,LeaseUntil}`, `Presence{Agent,Area,Status,TS}`, `isExpired`, `pruneAt`, `durArg` are named identically across tasks.
- **Deploy is a separate, human-gated step** — this plan lands committed code on a feature branch; it does NOT restart 8808.
