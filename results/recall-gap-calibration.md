# Recall Gap-Truncation Calibration (2026-06-16)

**Goal:** pick the default for `SENTRY_RECALL_GAP` — the ratio at which `Recall` truncates results at the
first relevance cliff (`dist[i] > dist[i-1] · gap`) — from real data, not a single probe.

## Method (data-first, like the τ recalibration)

- 20 representative real queries (spanning the live tenant-1 corpus's actual topics: matrix infra, Ashley
  cognition/determinism/LVQ/sleep, three.js, blazeteams, lacuarta-pos, C/SIMD refs, engine config).
- Embedded each query with the PRODUCTION embedder (tesla Ollama `nomic-embed-text-v2-moe`, dim 768) and scored
  squared-L2 against all 227 live memories on 8808 — full top-8 rankings (no truncation).
- Hand-labeled each retrieved result relevant/irrelevant from its text (160 labels).
- Swept the gap factor; for each, simulated the truncation and computed macro precision / recall (of retrieved
  relevants kept) / F1 across the 18 queries that have ≥1 relevant hit; tracked avg results returned and how
  many queries lost a relevant.

## Result

| gap | Precision | Recall | F1 | avg returned | queries losing a relevant |
|-----|-----------|--------|------|--------------|---------------------------|
| off (fixed-k=8) | 0.36 | 1.00 | 0.50 | 8.0 | 0/18 |
| 1.10 | 0.72 | 0.73 | 0.57 | 4.5 | 8/18 (too aggressive) |
| **1.15–1.20** | **0.57** | **0.89** | **0.58 (peak)** | 6.2 | 3/18 |
| 1.25 (prior default) | 0.52 | 0.89 | 0.54 | 6.5 | 3/18 |
| 1.30 | 0.42 | 0.97 | 0.52 | 7.5 | 1/18 |
| 1.60 | 0.36 | 1.00 | 0.50 | 8.0 | 0/18 |

**Decision: default 1.25 → 1.20.** F1 peaks flat at 1.15–1.20, and **1.20 dominates 1.25** — identical recall
(0.89) and identical relevant-loss (3/18) but higher precision (0.57 vs 0.52 = less off-topic padding). 1.10
maximizes precision but loses a relevant in 8/18 queries (too aggressive). Gap-truncation overall lifts
precision 0.36→0.57 for a small recall cost (1.00→0.89).

## Honest limits / bigger levers (the #4 frontier)

- The cliff ratios genuinely conflict by query regime: sharp single-answer queries have a wide cliff (dedup-τ
  1.29, sleep 1.32) while tight relevant clusters have a narrow one (three.js 1.11, NEON 1.14). No single ratio
  cuts every cliff without occasionally fragmenting a tight cluster — 1.20 is the best compromise, not perfect.
- **~5/20 queries are ranking/coverage misses** the gap CANNOT fix: the relevant memory is buried below
  irrelevant ones (e.g. "embedder of server2" → top-1 was a user-pref memory; the real answer #198 ranked 3rd)
  or absent from top-8 ("comms channel", "GA tuner"). For "nothing-relevant-found" queries an ABSOLUTE
  distance floor would help where a ratio gap can't. This is the real effectiveness frontier (recall RELEVANCE
  + coverage), not the gap value.
- Labels are one judge's calls; F1 ≈ 0.58 is modest and corpus-specific (63% Ashley-dominated). Re-run when the
  corpus broadens or the embedder changes (mistral-1024 on 8809 has a different absolute scale — the ratio gap
  is embedder-agnostic, but its ideal value should be re-checked there).
