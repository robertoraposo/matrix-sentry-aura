# Supersede-Dedup · Design Spec

> Date: 2026-06-13 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved for spec → implementation plan.
> Follows: [[2026-06-13-auto-remember-design]] (this is the next lever it surfaced).

## Problem

The auto-remember dedup gate (shipped 2026-06-13) skips a new memory when its nearest same-tenant
neighbor is within squared-L2 τ=0.85. On the **first live reflection** it immediately exposed a limit:
**semantic dedup is truth-blind.** A fact and its *correction* are semantically near — "auto-remember is
still manual / pending" (memory #23) and "auto-remember is LIVE" embed close together — so the correction
was deduped against #23 and **not stored**. The corpus keeps the stale fact; legitimate updates are
suppressed exactly like redundant restatements.

Distance alone cannot separate the three cases inside τ:
- (a) **redundant restatement** of the same fact → correctly *skip*;
- (b) **update/correction** of an existing fact → should *supersede* (replace);
- (c) **distinct-but-close** fact → must *store* (and must NOT be clobbered).

Case (c) is why a distance-only "newest wins" rule is unsafe: it would destroy a distinct fact. The
signal that separates (b) from (c) lives with the agent, which knows whether it is correcting or adding.

## Decision (settled in brainstorming)

**Explicit agent intent.** The agent declares supersession. `remember` gains an optional `supersedes: <id>`
pointer; after `recall` surfaces a stale/contradicted memory, the agent calls `remember(text, supersedes:id)`.
This puts the truth-judgment where the knowledge is and eliminates distance-based clobbering of distinct facts.

## Constraints (inherited)

- Pure Go, zero external deps. The SentryLog journal is **append-only** — records are never deleted on disk.
- Best-effort: no path may error or block the agent; invalid input degrades gracefully.
- Multi-tenant: a tenant can only supersede its own memories.
- Backward compatible: existing `EventMemory` records (no supersede field) must still rebuild.

## Architecture

### Why this is cheap and clean

Because the journal is append-only, a superseded record is **never removed from disk** — the full history
stays auditable no matter what. "Supersede" therefore only affects the **in-RAM index** (the current truth
that `recall` returns). That yields both properties at once — current-truth recall and complete on-disk
history — without a separate version structure. We replace-in-index rather than maintain a visible version
chain; the journal already provides versioning for free.

### Component 1 — `memory` package

**Schema.** `MemoryPayload` gains `Supersedes uint64 \`json:"sup,omitempty"\``. Old records omit it → decode
to 0 (backward compatible).

**`Remember` signature** becomes `(tenant, text, tags, src string, supersedes uint64) (id uint64, deduped bool, superseded uint64, err error)`:
- `supersedes == 0`: unchanged path — embed → dedup gate (skip near-dup within τ) → store. Returns
  `(id, deduped, 0, nil)`.
- `supersedes == id`, and `id` exists for `tenant`: **bypass the dedup gate**, append
  `EventMemory{ID:nextID, …, Supersedes:id}`, and remove `id` from the in-RAM `entries`. Returns
  `(newID, false, id, nil)`.
- `supersedes != 0` but `id` does not exist for `tenant` (missing or other tenant): degrade to a normal
  store (embed → dedup → store) and return `(id, deduped, 0, nil)` — the caller reports "superseded id not
  found; stored as new". Never an error.

**Rebuild (`New`).** Replays `EventMemory` records in journal order; when a record carries `Supersedes=X`,
remove `X` from the in-RAM index after adding the new entry. `nextID` stays monotonic across all records
(including superseded ones — ids are never reused). Result: index = current truth, disk = full history.

**Tenant isolation.** The supersede lookup only matches entries with `e.tenant == tenant`; a foreign id is
treated as "not found" → graceful store-as-new.

### Component 2 — `cmd/sentrymcp` MCP surface

The `remember` tool gains an optional integer arg `supersedes`. Handler passes it through and reports:
- stored new: `"remembered as memory #N"`
- deduped: `"already known as memory #N (deduped, not stored again)"` (unchanged)
- superseded: `"remembered as memory #N, superseding #M"`
- supersede target missing: `"superseded id #M not found for this tenant; remembered as memory #N"`
MokoBlinks log gains the `superseded` field.

### Component 3 — `cmd/sentry-reflect` reflection prompt

`reflectionPrompt` gains one clause: if `recall` surfaces a memory that is now outdated or wrong, call
`remember` with `supersedes:<its id>` to replace it rather than storing a contradicting duplicate. Shipping
this requires rebuilding and reinstalling the hook binary (Mac), then it activates on the next session/stop.

## Data flow

```
agent: recall("X")                → sees stale memory #M
agent: remember(text', supersedes=M)
  sentrymcp.remember               → memory.Remember(t, text', tags, src, M)
    embed text'
    M exists for t? ── no ──→ normal store (dedup) ──→ (newID, deduped, 0)
                    └─ yes ─→ append EventMemory{Supersedes:M}; drop M from index ──→ (newID, false, M)
  reply: "remembered as memory #newID, superseding #M"
reopen (New): replay journal; record with Supersedes=M drops M → recall returns text', not #M
```

## Testing (TDD, adversarial)

Unit (`memory`):
- **supersede replaces:** store A (id1); `Remember(... supersedes=id1)` with A'; `Recall` returns A' not A;
  `superseded==id1`; `Count` net unchanged (drop id1 + add new).
- **supersede bypasses dedup:** a text within τ of an existing memory, called WITH `supersedes` of that
  memory, is stored (the new id) and replaces it — not skipped.
- **invalid/foreign supersede id → graceful store-as-new:** `supersedes` of a non-existent id stores fresh,
  returns `superseded==0`; `supersedes` of another tenant's id does the same (tenant isolation).
- **rebuild drops superseded (critical):** persist A then A'(supersedes A), reopen via `New`, assert the
  index/recall return only A' and id A is absent — current truth survives the reopen, history is on disk.
- **monotonic ids:** superseding does not reuse the dropped id.

Surface (`cmd/sentrymcp`): existing tests updated to the new `Remember` arity; a test asserts the
superseding tool message.

Adversarial check: superseding a *valid-but-wrong* id loses nothing — the original record is still on disk
(grep the journal), recoverable; only the live index changed.

## Out of scope (YAGNI)

- Automatic contradiction resolution without explicit intent (the rejected distance-only approaches).
- A visible version chain in `recall` (the append-only journal is the audit trail; recall shows current truth).
- A separate `update_memory(id, text)` tool (an optional `supersedes` arg on `remember` is the chosen surface).
- Retroactively fixing the stale #23 in the live corpus (it self-heals when a future reflection supersedes it).
