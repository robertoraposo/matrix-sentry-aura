# Auto-Remember · Phase-0 Yield Measurement

> Date: 2026-06-13 · Instrument: `cmd/reflectyield` over real Claude Code transcripts
> (`~/.claude/projects/-Users-alvinnunez-Downloads-matrix-sentry/*.jsonl`, 36 sessions).
> Method: segment each session into windows of K tool-uses (`internal/transcript.Windows`),
> emit windows with ≥200 chars of prose, label a deterministic spread of 15 windows per K
> with the durable-knowledge rubric (decision / convention / gotcha; strict, when-in-doubt-no).

## Sweep summary (verbatim instrument output)

```
sessions=36 windows=87 emitted(text>=200)=82 k=20
sessions=36 windows=61 emitted(text>=200)=59 k=40
sessions=36 windows=47 emitted(text>=200)=47 k=80
```

## Yield (labeled sample, 15 windows per K)

| K  | labeled | durable (≥1 fact) | yield | windows/session |
|----|---------|-------------------|-------|-----------------|
| 20 | 15      | 9                 | 60%   | ~2.3            |
| 40 | 15      | 9                 | 60%   | ~1.7            |
| 80 | 15      | 7                 | 47%   | ~1.3            |

**Gate decision: GO.** The abort threshold was yield < ~10%; the measured yield is **47–60%**,
far above it. The captured facts are genuine durable knowledge, not noise (samples below).

**Non-obvious calibration finding:** yield does **not** fall as K shrinks (60/60/47%) — this
project's windows (R&D experiments + security reviews) are fact-dense, many holding 3–4 distinct
durable facts. That decouples K from yield and reduces the choice to *interruption cost*.

## Chosen K = 40

- ~1.7 reflections per session (vs 2.3 at K=20, 1.3 at K=80) — modest interruption.
- A 40-tool-use window is small enough that a single reflection doesn't have to surface a large
  pile of facts at once, but large enough not to fire constantly.
- Confirms the coded default `reflectEvery = 40` in `cmd/sentry-reflect` — **no code change needed**.
  The measurement validated the default rather than overturning it.

## Example extracted facts (quality evidence — these are what auto-remember would capture)

- **gotcha:** `claude mcp add --header "Authorization: Bearer $TOKEN"` leaks the token via process
  argv (`/proc/<pid>/cmdline`) while the command runs. (appeared at K=20, 40, AND 80 — recurrent)
- **decision:** At real η≈0.30–0.33 (Claude's measured access-repetition rate), Markov predictive
  bit allocation beats the popularity-marginal baseline by +2.1pp recall@10 at tight budget.
- **gotcha:** OPQ/random rotation is neutral on real embeddings (subspace variance already flat,
  σ²≈0.022–0.032); anisotropic PQ (ScaNN, h=2) is the first mechanism that actually helps (+2.1pp);
  h>2 over-corrects and hurts.
- **convention:** `sentry-recall` must register `async:false` so its stdout injects; it skips
  injection on `source=compact/clear`; truncates `additionalContext` to the 10k-char hook limit.
- **gotcha:** Claude Code's settings watcher auto-reloads hooks on save — a PostToolUse hook
  activates immediately after editing `settings.json`, no `/hooks` command or restart needed.
- **gotcha:** MokoBlinks client batches at size 50 — in HTTP mode low-volume telemetry never ships
  without a deferred `go s.moko.Flush()` per request.
- **decision:** IVFADC with `nprobe=16` beats flat PQ on all recall metrics and is 5.2× faster on
  SIFT1M (scans 1.8%); residual quantization (encode `v−centroid`) improves recall by cutting
  variance — the engine's architectural baseline.
- **gotcha:** `oracle@100=0.9983` proves the SIFT1M 4.8pp R@100 gap is cross-cell distractor loss,
  not quantization error — exact re-rank of the full shortlist recovers it; R@1/R@10 ARE
  quantization-limited (oracle@1=0.52).
- **gotcha:** Cloudflare Bot Fight Mode must be disabled on `blazesphere.net` — Anthropic's MCP
  broker is server-to-server and gets silently bot-blocked otherwise.

## τ (dedup threshold) — seeding note

τ is the squared-L2 novelty radius for server-side dedup (`memory.Store.DedupThreshold`). Real
`nomic-embed-text-v2-moe` vectors are dim-768 and unnormalized, so the absolute L2 scale differs
from the unit-test geometry — τ cannot be set from this transcript prose. It will be set at deploy
(Task 7) by embedding several of the example facts above against the live store and measuring:
paraphrases of the SAME fact must fall below τ, distinct facts comfortably above it. Until then,
the dedup unit tests pin behavior with a synthetic τ (geoEmbedder: 0.01 < τ=0.05 < 0.25), and the
server default `SENTRY_DEDUP_TAU=0` keeps dedup off until calibrated.
