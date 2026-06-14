# Forget / Delete (Tombstone) · Design Spec

> Date: 2026-06-13 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved → plan → subagent-driven implementation → prod deploy + cleanup.
> Follows: the within-τ triad ([[2026-06-13-supersede-dedup-design]], [[2026-06-13-dedup-force-design]]).

## Problem

The dedup triad (skip / supersede / force) curates memory by *adding or replacing*, but nothing *removes*.
The append-only journal plus `supersedes` (which replaces one-for-one, net zero) means the live index can
never **shrink** — accidental duplicates can't be cleaned. This surfaced concretely when the force-lever
verification left a near-identical pair (#41 ≈ #42) in the live corpus with no way to remove either.

## Decision

Add a **logical delete** via a tombstone (the only option under an append-only journal): a new `EventForget`
record that, on rebuild, drops the named id from the in-RAM index. The journal keeps every record on disk
(full history); only the live index (current truth that `recall` returns) shrinks.

**Governance:** the capability is **on-demand, not in the automatic reflection loop.** Forget is the one
operation that removes curated knowledge and has no additive safety net (no `un-forget` API), so the
Stop-hook reflection prompt is **not** changed — the agent does not auto-prune every 40 tool-uses. The
`forget` tool exists for deliberate curation (agent when asked, or operator) such as cleaning #41/#42.

## Architecture

### `memory` package

- `const EventForget sentry.EventType = 4` (1=Access, 2=PathMap, 3=Memory are taken).
- `type ForgetPayload struct { ID uint64 \`json:"id"\` }`.
- `func (s *Store) Forget(tenant sentry.TenantID, id uint64) (forgotten bool, err error)`:
  - under `s.mu`: if `hasEntry(tenant, id)` → `journal.Append(tenant, EventForget, ForgetPayload{ID:id})`,
    then `dropEntry(tenant, id)`, return `(true, nil)`.
  - else (id absent or another tenant's) → return `(false, nil)` with NO journal write (tenant-scoped,
    idempotent no-op, never an error).
- **Rebuild (`New`)**: today the scan filters to `EventMemory` only. Change it to scan records in seq order
  across types and switch: `EventMemory` → decode `MemoryPayload`, add to the index (keeping the existing
  vector-dim check and the `Supersedes` drop); `EventForget` → decode `ForgetPayload`, `dropEntry(tenant,
  id)`; any other type → skip. Because a forget for id N is always appended after N's creation, the entry is
  present when the tombstone is replayed. The shrink therefore survives reopen.

### `cmd/sentrymcp`

- New `forget` tool, input `{ id: integer }` (required).
- Handler: `id := uintArg(p.Args, "id")`; if `id == 0` → tool error "provide an 'id' to forget";
  `forgotten, err := s.mem.Forget(s.tenant, id)`; on error → tool error; response:
  - `forgotten` → `"forgot memory #N (removed from recall; still in the journal history)"`
  - else → `"memory #N not found for this tenant"`
  - log to MokoBlinks (`forget`, tenant, id, forgotten).
- The `mem == nil` (no embedder) guard mirrors the existing `remember`/`recall` handlers.

### Reflection prompt — unchanged

`cmd/sentry-reflect`'s `reflectionPrompt` is **not** modified. Forget stays out of the autonomous loop.

## Deployment + verification (the verification is the real cleanup)

Redeploy `sentrymcp` (VM); the Mac hook is unchanged but reinstall for parity. Then live-verify by cleaning
the duplicate the force test created: `forget(42)` → expect `"forgot memory #42 …"`; a follow-up `recall`
confirms #41 (the GP-feasibility fact) survives and #42 is gone. This both proves the feature in prod and
removes the accidental dup honestly.

## Testing (TDD)

- `memory`:
  - **forget removes from index**: store a memory, `Forget` it → `(true, nil)`, `Count` decremented, `Recall`
    no longer returns it.
  - **forget of absent/foreign id**: `Forget(tenant, 999)` and `Forget(otherTenant, id)` → `(false, nil)`, no
    change, no tombstone written.
  - **forget survives reopen (critical)**: store A and B, `Forget` A, close, reopen via `New` → only B in the
    index; A absent; ids not reused (`nextID` still advances past A).
  - tenant isolation: forgetting tenant 1's id does not touch tenant 2's identically-numbered space.
- `cmd/sentrymcp`: the `forget` handler parses `id`, calls `Forget`, and returns the forgotten / not-found /
  missing-id messages.

## Out of scope (YAGNI)

- forget-by-query (fuzzy and dangerous — id only).
- `un-forget`/restore API (the journal retains the record; reconstruction is possible later if needed).
- forget in the reflection prompt (decided out — governance).
- Journal compaction / physical deletion (the tombstone suffices; on-disk growth is irrelevant at an agent's
  memory volume).
