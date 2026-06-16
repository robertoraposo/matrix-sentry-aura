# Recall Observability (#4 v1) · Design Spec

> Date: 2026-06-16 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved → plan → subagent-driven → deploy. Make recall observable so its relevance + coverage can be
> measured on REAL traffic — the genuine effectiveness frontier (#4).

## Problem

The calibration work showed recall RANKING is already strong, but recall RELEVANCE + COVERAGE on real usage is
unmeasured because `recall` is not journaled at all. We have zero data on what agents actually ask, how often
recall finds nothing useful (coverage gaps), or whether results were relevant. You cannot improve what you
cannot observe. v1 instruments recall and measures coverage; v2 (deferred) adds LLM-judged relevance over the
same log.

## Decisions

- **Journal every recall** as a new `EventRecall` (type 6), written at the MCP handler layer (NOT inside
  `memory.Store.Recall`, which stays a pure read). Captures the query, k, and each hit's (id, distance).
- **On by default, env-disablable** (`SENTRY_RECALL_LOG`, default true; `0/false` disables). Append is cheap
  (buffered, fsync batched); a logging failure must never fail the recall (best-effort, log to MokoBlinks).
- **Tenant-scoped** like every journal op — a tenant only ever sees its own recall log.
- **`analyze_recall` tool** mirrors `analyze_access`: reports recall volume, the top-hit DISTANCE distribution
  (the coverage signal — high top-distance = "found nothing good"), the count of "weak" recalls above a floor,
  and the most recent real queries. This is what lets us (a) see the real query distribution and (b) calibrate
  a per-embedder "no good match" floor from real data later.
- **v2 deferred (explicit):** an LLM-judge pass over the EventRecall log scoring whether the returned memories
  were relevant to each real query (closes the relevance loop). Built on v1's log; heavier (judge workflow).

## Architecture

### Component 1 — `memory` event type + payload (memory/memory.go)

```go
const EventRecall sentry.EventType = 6 // a recall query + its hits (observability)

type RecallHit struct {
	ID   uint64  `json:"id"`
	Dist float32 `json:"dist"`
}
type RecallPayload struct {
	Query string      `json:"q"`
	K     int         `json:"k"`
	Hits  []RecallHit `json:"hits"`
}
```
No change to `Recall` itself. `New` does NOT replay EventRecall (it is observability, not state) — the existing
EventMemory/EventForget scans ignore other types already, so no change needed there.

### Component 2 — log on recall + `server.logRecall` (cmd/sentrymcp/main.go)

- `server` gains `logRecall bool`, set in `main()` from `envBool("SENTRY_RECALL_LOG", true)` (add a tiny
  `envBool` helper if absent, mirroring `envFloat`).
- In the `recall` tool handler, after `hits, err := s.mem.Recall(...)` succeeds and `s.logRecall` is true: build
  a `memory.RecallPayload{Query: query, K: k, Hits: [...]}` from `hits` (each `RecallHit{ID, Dist: h.Score}`)
  and `s.store.Append(tenant, memory.EventRecall, payload)`. Best-effort: on append error, `moko.Info` a warning
  and continue (the recall response is unaffected).

### Component 3 — `analyze_recall` tool (cmd/sentrymcp/main.go)

- Tool def `analyze_recall` (no args), description: "Measure recall coverage: how many recalls, the top-hit
  distance distribution (high = found nothing relevant), and the most recent real queries — for this tenant."
- Handler: `s.store.Scan(Filter{Tenant:&tenant, Type:&EventRecall})`, decode each `RecallPayload`; collect the
  top hit's `Dist` (hits[0].Dist; if no hits, treat as "empty"). Report a single text line + a short tail:
  `recall coverage (tenant N): total=<n> empty=<e> topDist min/p50/p90/max=… | recent: "q1"(d=..) "q2"(d=..) …`
  (last ~8 queries, truncated). Pure read; tenant-scoped.

## Testing (TDD)

- **memory**: `RecallPayload` marshals/unmarshals via `sentry.MarshalPayload`/`UnmarshalPayload` round-trip
  (query, k, hits preserved). `EventRecall == 6` and distinct from the other event consts.
- **cmd/sentrymcp**: after a `recall` tool call (logging on), a `Scan` for `EventRecall` finds one record with
  the query + the hit ids/dists; with `s.logRecall=false`, none is written; `analyze_recall` returns stats
  reflecting the logged recalls (total count, a recent query string present); `analyze_recall` on an empty log
  returns a sane "total=0" line; recall STILL succeeds if the append path is exercised (best-effort).

## Deployment

Rebuild `sentrymcp`; redeploy 8808 + 8809. `SENTRY_RECALL_LOG` defaults on. After a day of real traffic, run
`analyze_recall` (and the auto-recall hook will populate it) to see the real query distribution + coverage
(top-dist) — then calibrate a "no good match" floor and plan v2 (LLM relevance judging).

## Out of scope (YAGNI / follow-ups)

- v2 LLM-judge relevance over the EventRecall log (the real closed-loop relevance metric).
- Surfacing "no strong match" in the recall RESPONSE (needs the calibrated floor from v1 data first).
- Sampling/retention/rotation of the recall log (log everything for now; revisit if volume bites).
- `New` replaying EventRecall (it is telemetry, not state — never replayed).
