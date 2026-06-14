# Dedup Force Escape-Hatch · Design Spec

> Date: 2026-06-13 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved → plan → subagent-driven implementation → prod deploy.
> Follows: [[2026-06-13-supersede-dedup-design]] (this is the third leg the dogfooding surfaced).

## Problem

Auto-remember's semantic dedup (squared-L2 < τ=0.85) cannot tell three within-τ cases apart, and two live
reflections proved it: (1) a *correction* of an existing fact (solved by `supersedes`), and now (2) a
**genuinely-distinct fact that is merely vocabulary-similar** to an unrelated memory. The GP-feasibility
finding deduped against memory #22 (engine config) — both mention ivf.Recommended/nprobe/rerankK/recall@10 —
even though they are different facts. There is no way to store it short of `supersedes`, which would wrongly
clobber #22. The dedup needs an explicit **force** escape-hatch: "I know this is near something; it's
distinct; store it anyway."

## Decision

Complete the within-τ triad with explicit agent intent (the project's established pattern):

- no flag → **skip** (assume redundant restatement) — default, unchanged
- `supersedes: N` → **replace** memory N (the update case)
- `force: true` → **store anyway**, bypassing the dedup gate (the distinct-but-near case)

`force` is chosen over a tag-aware dedup: the GP fact and #22 shared the `matrix-sentry` tag, so tag-scoping
would not have prevented the false positive, and it depends on fragile tag discipline. Only the agent knows a
near neighbor is actually a distinct fact, so the judgment belongs with the agent (as with `supersedes`).

## API change — options struct (stops parameter sprawl)

`Remember` is at 5 positional params; `force` would make 6 (a `uint64` next to a `bool` — an argument-order
trap). Refactor to an options struct, keeping the existing 4-tuple return (force adds no return — a forced
store is simply `deduped=false`):

```go
type RememberOpts struct {
    Tags       []string
    Src        string
    Supersedes uint64
    Force      bool
}
func (s *Store) Remember(tenant sentry.TenantID, text string, opts RememberOpts) (id uint64, deduped bool, superseded uint64, err error)
```

Decision precedence inside `Remember` (mutually exclusive):
1. `opts.Supersedes != 0` and it names an existing same-tenant memory → **supersede** (already bypasses
   dedup; if `Force` is also set, supersede wins — `Force` is moot).
2. `opts.Force` → **store without the dedup gate** (persist even when the nearest neighbor is within τ).
3. otherwise → the current dedup gate (skip when nearest same-tenant `sqL2 < DedupThreshold`).

## MCP surface (`cmd/sentrymcp`)

- The `remember` tool gains an optional boolean `force` arg.
- A `boolArg(args, key)` helper reads it (JSON `true`/`false` → bool; missing/non-bool → false).
- Handler calls `Remember(tenant, text, memory.RememberOpts{Tags: tags, Src: src, Supersedes: supersedes, Force: force})`.
- Response for a forced store is the normal `"remembered as memory #N"` (no special message needed).

## Reflection prompt (`cmd/sentry-reflect`) — closes the false-positive loop

Add one clause: if `remember` reports the fact was deduped but it is genuinely distinct (not a restatement
of the named memory), call `remember` again with `force:true`. This makes the agent self-correct exactly the
false positive observed live (it saw "deduped #22", and its fact was distinct).

## Callers to update (the same set as before)

Every `Remember(...)` call site in `memory/memory_test.go` migrates to the `RememberOpts` form
(`Remember(1, "cat", nil, "", 0)` → `Remember(1, "cat", RememberOpts{})`; `Remember(1, "feline", nil, "", id1)`
→ `Remember(1, "feline", RememberOpts{Supersedes: id1})`; tag/src cases → `RememberOpts{Tags:…, Src:…}`). The
`cmd/sentrymcp` remember handler.

## Testing (TDD)

- `memory`: **force stores a near-duplicate** — with `DedupThreshold=0.05`, `Remember(1,"kitten",
  RememberOpts{Force:true})` after storing `"cat"` persists (`deduped=false`, Count→2), vs the non-force path
  which dedups (Count stays 1). `supersedes` still takes precedence over `force`. All existing call sites
  compile under the new signature (migrated to `RememberOpts`).
- `cmd/sentrymcp`: `boolArg` parses booleans (true/false/missing); the handler forwards `force`.
- `cmd/sentry-reflect`: `reflectionPrompt` contains "force".

## Deployment

Redeploy `sentrymcp` (VM) + reinstall `sentry-reflect` (Mac). **Live verification (the real case):** re-`remember`
the GP-feasibility finding with `force:true` and confirm it is stored (not deduped against #22), closing the
case that surfaced this lever.

## Out of scope (YAGNI)

- Tag-aware dedup (fragile; would not have caught the real case).
- Bundling the return values into a result struct (the 4-tuple stays — `force` adds no return).
- A distinct MCP message for forced stores.
