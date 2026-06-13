# Auto-Remember · Design Spec

> Date: 2026-06-13 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved for spec → implementation plan.

## Problem

Matrix Sentry's memory cycle has three legs; only two are automatic:

- `record_access` (PostToolUse hook → `cmd/sentry-record`): captures *what files were touched*. Automatic, high volume (~5/min).
- `recall` (SessionStart hook → `cmd/sentry-recall`): *injects* relevant project memory at session start. Automatic.
- `remember`: distills *durable knowledge* (decisions, conventions, gotchas) into the semantic store. **Still manual.**

Because `remember` is manual, the corpus does not grow on its own. The goal is to close this last leg:
detect durable knowledge during agent work and persist it automatically via `remember`, **without polluting
memory with noise**.

## Constraints (inherited from the project)

- Pure Go, zero external deps. Ollama (tesla, embeddings only) is the sole allowed external; the server runs
  **no generative model** ("papa→servidor no-Ollama generation" strategy).
- Hooks are best-effort: never block, never error out a tool use, silent no-op without `SENTRY_MCP_URL`.
- Never `sentry.Open` the live journal directory — snapshot the `.log` files first (recovery can truncate).
- Verify on real data; adversarially re-check before believing.

## Decisions (settled in brainstorming)

1. **Detection = agent self-report.** A hook asks the *already-running model* to distill what it learned and
   call `remember`. Reuses the LLM's in-context judgment — no lexical classifier (noisy), no extra generative
   model (breaks the strategy). The model already holds the session context; distillation is cheap and accurate.
2. **Cadence = activity threshold.** Fire the reflection every *K* new tool-uses since the last reflection,
   even while the session is alive — captures knowledge in long sessions instead of waiting for session end.
3. **Anti-noise = defense in depth.** The reflection prompt instructs the agent to `recall` first and only
   store genuinely-new facts; AND the server semantically deduplicates at `remember` time as a guaranteed
   invariant, so the corpus stays clean even if an agent misbehaves.

## Enabling mechanism

Claude Code's **Stop hook** runs when the agent finishes a turn. Returning `{"decision":"block","reason":"…"}`
forces the agent to continue processing with `reason` as its instruction (the same gate "keep going" uses).
The hook input carries `stop_hook_active` precisely to prevent infinite loops. `SessionEnd` cannot be used —
it fires after the session is dead and cannot ask the model to act.

**Accepted trade-off:** blocking the Stop makes the agent continue in the *user-visible* conversation to run
the reflection (not an isolated subagent) — this is intrinsic to "self-report" (it uses the running model's
context). The reflection prompt demands terseness to minimize intrusion. Isolating reflection into a separate
process is explicitly out of scope for v1.

---

## Phase 0 — Measurement first ("decide con datos")

Before wiring anything into production, measure the **yield**: what fraction of *K*-access windows / sessions
contain ≥1 durable fact worth remembering? This calibrates the mechanism; it does not merely justify it.

### Instrument: `cmd/reflectyield` (pure Go)

- Reads real Claude Code transcripts: `~/.claude/projects/-Users-alvinnunez-Downloads-matrix-sentry/*.jsonl`
  (35 sessions available as of 2026-06-13). Each `assistant`/`user` line has `message.content` as a list of
  blocks; `tool_use` blocks (with `name`) measure activity; `text` blocks are the material for judgment.
- Optionally cross-references the live journal **snapshot** (copy `.log` first) for access counts, but the
  transcript's `tool_use` count is the primary activity signal and is self-contained.
- Segments each session into windows of *K* tool-uses (sweep several candidate *K*, e.g. 20/40/80) and emits a
  JSONL of windows: `{session, window_idx, tool_use_count, text}` where `text` is the concatenated assistant/user
  prose in that window (truncated to a sane cap).

### Labeling (the honest dry-run)

The judge is the **same LLM that will perform self-report**, so labeling is a faithful preview of mechanism
quality: take a **random sample** of windows and, for each, judge "does this window contain ≥1 durable fact
(decision / convention / gotcha)?" and, if so, extract the fact(s). The extracted facts ARE what auto-remember
would capture.

### Outputs → calibration

- **Yield fraction** per candidate *K* → pick *K* so each reflection trigger has ≈1 expected durable fact
  (neither empty triggers — costly/intrusive — nor late triggers — context lost).
- **Fact quality** (read the sampled extractions) → go/no-go: if the yield is noise, rethink before building.
- A rough **dedup distance distribution**: embed a few extracted facts and measure pairwise L2 to seed the
  dedup threshold τ (refined in Component 2's tests).

This phase produces a short written finding (yield numbers + example facts + chosen *K*, τ seed) appended to
the handoff/memory, gating the build.

---

## Component 1 — `cmd/sentry-reflect` (new Stop hook, TDD)

Parallel in spirit and robustness to `cmd/sentry-record` / `cmd/sentry-recall`.

### Input (hook JSON on stdin)

```
{ "session_id": "...", "transcript_path": "/abs/.../<id>.jsonl",
  "stop_hook_active": false, "cwd": "..." }
```

### State

Per-session counter file: `~/.cache/matrix-sentry/reflect/<session_id>` storing the `tool_use` count at the
last reflection. Created on first sight.

### Logic

1. Best-effort: any error (bad JSON, unreadable transcript, missing config) → exit 0 (allow the stop). No-op
   without `SENTRY_MCP_URL` (config via env or `~/.matrix-sentry.env`, same loader pattern as `sentry-record`).
2. Count current `tool_use` blocks across `assistant` messages in `transcript_path`. `delta = current − stored`.
3. If `stop_hook_active == true` → exit 0 (we are inside a reflection pass we triggered; never re-fire — loop guard).
4. If `delta ≥ K` → write `current` to the state file and print to stdout
   `{"decision":"block","reason":<REFLECTION_PROMPT>}`, exit 0. Else → exit 0 (allow stop).

### The reflection prompt (`reason`)

Precise, mirrors the project's memory guidance:

> Pause before finishing. Reflect on the work since your last memory checkpoint. If — and only if — you
> learned **durable knowledge** (a decision made, a convention adopted, a gotcha discovered) that a future
> session would benefit from, persist it: first call `recall` to avoid duplicating what's already stored, then
> call `remember` once per genuinely-new fact, each fact self-contained and concise. Do **not** store transient
> state, file contents, task progress, or anything already in the code/git. If nothing durable was learned,
> store nothing. Be terse — do not narrate this to the user. Then finish.

### Robustness invariants (test-pinned)

- No-op (exit 0, no output) when `SENTRY_MCP_URL` unset.
- Never blocks when `stop_hook_active` is true (loop guard).
- Never blocks when `delta < K`.
- Blocks exactly once per *K*-window: after firing, the stored counter advances to `current`.
- Malformed/missing transcript → exit 0, no output.
- Output, when blocking, is valid JSON with `decision:"block"` and a non-empty `reason`.

---

## Component 2 — Semantic dedup in `memory` package (TDD)

`Remember` gains a novelty gate so repeated/near-identical facts don't accumulate.

### Behavior

Before persisting a new memory, search the store for the nearest existing memory (the exact-L2 search already
present). If `nearestDist < τ` → **skip** persisting and return the existing memory's id with `deduped = true`.
Otherwise persist as today and return `deduped = false`.

### API

Widen the return: `Remember(tenant, text, tags, src) (id uint64, deduped bool, err error)`. On a dedup hit
`id` is the existing memory's id and `deduped == true`. Update both callers in the same change (`cmd/sentrymcp`
and the `memory` tests) — the argument list is unchanged, only the result widens. τ is a configurable field on
the `Store` (exported, default seeded from Phase 0) so tests can pin the boundary deterministically.

### MCP surface

`cmd/sentrymcp`'s `remember` tool reports the `deduped` status in its response so the agent (and logs) can see
when a fact was a no-op duplicate. No new tool required.

### Test-pinned invariants

- A fact near-identical to a stored one (`dist < τ`) is NOT persisted; store count unchanged; existing id returned.
- A genuinely-distinct fact (`dist ≥ τ`) IS persisted; store count increments.
- Boundary at τ behaves deterministically.
- Dedup does not suppress distinct facts (false-positive guard) — verified with hand-picked distinct pairs.
- Persisted vectors still reopen without re-embedding (existing invariant preserved).

---

## Wiring & deployment

- New binary `sentry-reflect` built `CGO_ENABLED=0 GOOS=darwin` for the Mac (hooks run on the Mac), installed to
  `~/.local/bin/sentry-reflect`, registered as a **Stop** hook in `~/.claude/settings.json` (`async:false` so its
  stdout `block` decision is honored; short timeout; same secrets via `~/.matrix-sentry.env`).
- The dedup change ships in the `memory` pkg + `cmd/sentrymcp`, redeployed to the homelab VM (build linux/amd64,
  ship binary, `systemctl restart sentrymcp`, keep `.bak`). Journal preserved.
- *K* and τ are set from Phase 0 findings.

## Adversarial verification (before believing)

- Unit tests above (hook gating + loop guard + no-op; dedup novelty + boundary + false-positive guard).
- On real data: confirm the auto-remembered facts are genuinely durable (not noise) by inspecting what a live
  threshold-triggered reflection actually stores in a real session; confirm dedup isn't silently dropping
  distinct facts (τ false positives). If either fails, recalibrate before trusting.

## Out of scope (YAGNI)

- Isolating reflection into a separate subagent/process (v1 uses the visible conversation).
- Lexical/LLM classifier for detection (self-report chosen).
- Updating/merging near-duplicates instead of skipping (skip is the v1 dedup; merge is a later refinement).
- Improving the `recall` query (git-remote/README) — tracked separately in the handoff queue.
