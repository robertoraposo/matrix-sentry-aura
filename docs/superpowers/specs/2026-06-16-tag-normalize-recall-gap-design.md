# Tag Normalization + Recall Gap-Truncation · Design Spec

> Date: 2026-06-16 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved → plan → subagent-driven → deploy. Two small, high-impact effectiveness fixes found by the
> live-corpus analysis: tag case-fragmentation and fixed-k recall padding.

## Problem

Analysis of the live tenant-1 corpus (227 memories) surfaced two effectiveness gaps:
1. **Tag case-fragmentation.** The same tag exists as `ashley` (84) AND `ASHLEY` (60) — tags are stored
   verbatim, so casing/whitespace variants fragment tag-based grouping (dashboard clusters, future filters).
2. **Fixed-k recall padding.** `Recall(tenant, query, k)` always returns k results even when only the top one
   is relevant. A real probe returned the exact hit at distance 1.095 then padding at 1.42+ — a clear
   relevance cliff that fixed-k ignores, injecting off-topic memories (noise) into recall consumers (incl. the
   SessionStart auto-recall hook across all 5 clients).

## Decisions

- **Normalize tags at the index boundary, both directions.** Lowercase + trim + de-duplicate (order-preserving)
  in `Remember` (so new writes persist normalized) AND in the `New` rebuild scan (so the existing mixed-case
  records are normalized in the live serving index immediately, without rewriting the append-only journal —
  disk history stays verbatim, the serving view is consistent).
- **Recall gap-truncation, ratio-based (embedder-agnostic).** After sorting ascending by squared-L2 and capping
  to k, truncate at the first "relevance cliff": the first index `i ≥ 1` where `score[i] > score[i-1] * gap`.
  Always keep ≥1 result. A RATIO (not an absolute distance) is used so the same default works for nomic-768
  (8808) and mistral-1024 (8809), whose absolute distance scales differ. Controlled by a `Store.RecallGap`
  field set from `SENTRY_RECALL_GAP` (default **1.25**; `0` or `≤1` disables → today's exact behavior). k stays
  the hard cap.
- **Default-on but reversible.** 1.25 (cut when the next hit is >25% farther) trims obvious noise while keeping
  comparably-close hits. The single probe showed a ~1.30× cliff; 1.25 is a conservative starting default and is
  env-tunable — a follow-up calibration pass (over many real queries) can tighten it like τ was tuned. Setting
  `SENTRY_RECALL_GAP=0` restores fixed-k exactly.

## Architecture (all in `memory` + `cmd/sentrymcp` wiring; storage layer unchanged)

### Component 1 — `normalizeTags` (memory/memory.go)

`func normalizeTags(tags []string) []string` — for each tag: `strings.ToLower(strings.TrimSpace(t))`; drop
empties; keep first occurrence only (order-preserving dedupe). Returns `nil` for no tags (so `omitempty` still
applies). Applied to `opts.Tags` in `Remember` (both the supersede and the normal branch, before building the
`MemoryPayload`) and to `p.Tags` in `New`'s EventMemory scan (line ~105, before appending the entry).

### Component 2 — `Store.RecallGap` + truncation (memory/memory.go)

- New exported field `RecallGap float32` on `Store` (sibling of `DedupThreshold`; zero value = disabled).
- In `Recall`, after the existing sort and the `len(scored) > k` cap, if `s.RecallGap > 1` and `len(scored) > 1`:
  walk from index 1; at the first `i` where `scored[i].Score > scored[i-1].Score * s.RecallGap`, set
  `scored = scored[:i]` and stop. (i starts at 1 so the top hit is always kept.)

### Component 3 — wiring (cmd/sentrymcp/main.go)

- New flag/env: `recallGap := flag.Float64("recall-gap", envFloat("SENTRY_RECALL_GAP", 1.25), "...")` next to
  the `dedup-tau` flag. After `s.mem` is built, set `s.mem.RecallGap = float32(*recallGap)` (alongside the
  existing `s.mem.DedupThreshold = ...`). Reuses the existing `envFloat` helper.

## Testing (TDD)

- **normalizeTags** (unit): `["ASHLEY","ashley"," Ashley "]` → `["ashley"]`; `["A","B"]` → `["a","b"]`; `nil`/`[]`
  → empty; preserves order of first occurrence.
- **Remember stores normalized tags**: `Remember(t,"x",{Tags:["ASHLEY"," Bug"]})` then `List` / `Recall` shows
  `["ashley","bug"]`.
- **Rebuild normalizes existing**: write a record with mixed-case tags via the store, reopen with `New`, assert
  the rebuilt entry's tags are lowercased (proves on-disk verbatim is normalized in the live index).
- **Recall gap-truncation**: with a deterministic test embedder placing one hit close and others far past the
  ratio cliff, `RecallGap=1.25` returns only the close hit(s); `RecallGap=0` returns all k (back-compat). Always
  ≥1 result. A cluster of comparably-close hits (steps < gap) is NOT truncated.
- **Back-compat**: default `RecallGap=0` in a freshly-`New`'d store (field zero value) → identical to today
  until main.go sets it; existing recall tests unaffected.

## Deployment

Rebuild `sentrymcp`; redeploy 8808 (personal) + 8809 (teams). On rebuild, the live index auto-normalizes the
existing tags (no migration). `SENTRY_RECALL_GAP` defaults to 1.25; no env change needed. Verify on a real
query: the dedup-τ probe returns just the exact hit (gap trims the 1.42 padding) and tags show lowercased.

## Out of scope (YAGNI / follow-ups)

- Calibrating the gap factor against a labeled query set (the honest "tighten the default" follow-up).
- Rewriting on-disk tags (unnecessary — the index is the serving truth; disk is immutable history).
- A per-call `gap` override on the recall MCP tool (env-level is enough for now).
- Absolute distance thresholds (rejected — not portable across embedder dims).
