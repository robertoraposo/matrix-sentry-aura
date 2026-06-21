# bge-m3 Recalibration (τ + recall-gap) — 2026-06-21

After migrating the 8808 corpus from nomic-768 to **bge-m3-1024** (via Ante Crucible), the two embedder-specific
knobs were re-measured in the new vector space (same data-driven method as the nomic calibrations). bge-m3 is a
**more compact / discriminative** space than nomic — both knobs moved.

## recall-gap (ratio cliff truncation)

Re-ran the 20-query harness (queries embedded via bge-m3, ranked against the live 783-memory 1024-d corpus,
reusing the preserved relevant-id labels — `cmd/reembed` kept ids):

| gap | Precision | Recall | F1 | avg returned |
|-----|-----------|--------|------|--------------|
| off (k=8) | 0.29 | 0.86 | 0.43 | 8.0 |
| **1.05** | 0.79 | 0.55 | **0.65** | 2.5 |
| **1.10 (chosen)** | 0.65 | 0.63 | **0.64** | 4.3 |
| 1.12 | 0.60 | 0.66 | 0.63 | 4.7 |
| 1.20 (old, nomic) | 0.51 | 0.69 | 0.59 | 5.5 |

**F1 peaks at 1.05–1.10** (vs 1.20 in nomic). Chose **gap=1.10** — near-peak F1 with a better recall/return
balance than the aggressive 1.05. The old 1.20 is now too loose (returns ~5.5, noisy). Query top-hit distance
in bge-m3: min 0.490 / p50 0.844 / max 1.048 (vs nomic's ~1.0–1.5 → confirms the tighter space).

## dedup τ (squared-L2)

Measured paraphrase pairs (should dedup) vs distinct-same-domain pairs (must NOT) + corpus NN distribution:
- **paraphrase**: 0.346, 0.353, 0.575 (clear cases ~0.35; one ambiguous at 0.575)
- **distinct same-domain (ashley)**: 0.618, 0.628, 0.688 (floor ~0.618)
- corpus NN sqL2 (sample): min 0.000 (some existing exact/near dups), p10 0.249, p50 0.430

The paraphrase/distinct margin is **NARROW** (0.575 vs 0.618) — dedup is riskier in bge-m3 than nomic. Per the
project rule (*bias to false-negatives: collapsing a distinct fact is silent corruption; a stored near-dup is
cheap*, see [[matrix-sentry-dedup-calibration-gotcha]]), chose **τ=0.50** — above the clear paraphrases (~0.35),
comfortably below the distinct floor (0.618). The ambiguous 0.575 paraphrase is intentionally left as a stored
near-dup (safe). Was 0.45 (nomic).

## Applied (8808, env)

`SENTRY_DEDUP_TAU=0.50`, `SENTRY_RECALL_GAP=1.10` (both env-backed). Verified: recall "cómo se calibró tau" → 1
hit (#65, dist 0.897), tightly cut. 8809 (teams, mistral-1024) unchanged — its own space, separate calibration
if it ever carries real volume.

## Caveat

The 20-query labels were authored for the older/smaller corpus; F1 ≈ 0.64 is modest and corpus-specific
(ashley-dominated, now 783 memories). Re-run when the corpus broadens or after real bge-m3 recall traffic
accumulates (analyze_recall now logs real queries — a future calibration can use those instead of the 20
hand-authored ones).
