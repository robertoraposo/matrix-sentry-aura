# GP Adaptive Per-Query Policy · Design Spec

> Date: 2026-06-13 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved for spec → implementation plan. R&D lever (not a production deploy by default).
> Builds on: the GA static tuner ([[ga-auto-tuner]], `internal/evolve` + `cmd/evolve`) and `ivf.Recommended`.

## Problem

The GA tuner found the best **static** config (`ivf.Recommended`: nlist=64, M=96, K=64, nprobe=32,
rerankK=200) — one search depth for every query. But queries are not equally hard: some land squarely in
one coarse cell (clear nearest centroid), others are ambiguous (several centroids nearly equidistant). A
single static depth over-spends on easy queries and may under-spend on hard ones. The lever: evolve a
**per-query adaptive policy** that reads cheap per-query features (coarse-distance margins) and chooses
`(nprobe, rerankK)` per query — genetic programming over policy *expressions*, not just static knobs.

## Objective

**Pareto-dominate the static frontier.** A win means the policy achieves `recall@10 ≥ static AND mean
latency ≤ static` (with at least one strict), measured the same way for both. It lowers depth on easy
queries (clear margin) and raises it on hard ones (ambiguous margin).

## Constraints (inherited)

- Pure Go, zero external deps. Deterministic given a seed (a seeded `*rand.Rand`; the kmeans seed is NOT a
  gene). Same result regardless of core count.
- Anti-self-deception is mandatory (this project has had a closed-loop run reverse a conclusion): fixed
  evolution set, pristine holdout, proxy latency during search (never wall-clock), multi-seed validation.
- Real data: 47k×768 nomic-embed-text embeddings (tesla `/tmp/sembed-big/big_*`), exact GT, same harness
  the GA tuner used.

## Why this can fail (and the gate that catches it)

A per-query policy only helps if the coarse-margin features actually *predict* the depth a query needs. If
needed-depth is uncorrelated with the margins, or if no query is "easy" (every query needs max depth), no
policy — GP or otherwise — can beat static. **Phase 0 measures this and gates the rest.**

---

## Component 1 — `internal/gp` (the GP engine)

A generic genetic-programming engine over arithmetic expression trees. One responsibility: evolve and
evaluate expression trees; it knows nothing about IVF.

- **Tree:** binary/unary nodes. Operators: `+ − × ÷ min max` (÷ is *protected*: denominator with |x|<ε
  yields 1, never NaN/Inf). Terminals: feature variables `f0..fN` and ephemeral float constants.
- **Individual:** two independent trees, `nprobeExpr` and `rerankExpr`. Evaluating a tree on a feature
  vector yields a float; the caller clamps+discretizes it (Component 2).
- **Init:** ramped half-and-half to a max init depth.
- **Variation:** tournament selection, subtree crossover, subtree + point mutation, elitism, random
  immigrants. **Bloat control:** a hard depth limit on every produced tree + parsimony pressure (fitness
  penalized by total node count × a small coefficient).
- **Determinism:** all randomness flows through a single seeded `*rand.Rand` argument; no global rand,
  no time, no map-iteration-order dependence. Same seed ⇒ identical run.
- **API:** `Eval(tree, feats []float32) float64`; `type Node`; `Run(cfg, fitness func(Individual) float64,
  rng *rand.Rand) Result` returning the best individual + per-generation stats. `Individual` exposes its two
  trees and a stable string form (for logging the learned policy).

Tests (TDD): protected division (÷0 → finite), eval determinism, crossover/mutation respect the depth limit
and produce valid trees, parsimony lowers fitness for equal-behavior bloat, a hand-built tree evaluates to a
known value.

## Component 2 — Per-query features + policy output mapping

**Features** are derived from the coarse-distance vector the IVF search already computes. With centroid
distances sorted ascending `d[0] ≤ d[1] ≤ …`:

- `f0 = d[0]` — distance to nearest centroid (query density / how well-clustered).
- `f1 = d[1] / (d[0] + ε)` — routing ambiguity ratio (≈1 ambiguous → hard; large → clear → easy).
- `f2 = d[1] − d[0]` — absolute margin/gap.
- `f3 = mean(d[0:8]) / (d[0] + ε)` — spread across the top cells.

(~4 features, computed once per query from data already produced during probing — zero extra distance
work.) A `Features(coarseDists []float32) [4]float32` helper lives in `ivf` (it owns the coarse layout).

**Output mapping:** a tree's float output is clamped to the legal range then **snapped to the nearest grid
value** — `nprobe ∈ {1,2,4,8,16,32,64}` (capped at nlist) and `rerankK ∈ {0,50,100,200,400}`. Discretization
bounds per-query latency and keeps the proxy cost well-defined. A degenerate/NaN output snaps to the static
default (safe fallback).

## Component 3 — `ivf` integration

- Expose the sorted coarse distances so features cost nothing extra. Refactor `probeCells` (or add
  `coarseSorted(query) []cd`) so a caller can get the full sorted `(cell, dist)` list once and both (a) probe
  the top-nprobe cells and (b) compute features from the same slice.
- `type Policy interface { Plan(feats [4]float32) (nprobe, rerankK int) }`.
- `func (ix *Index) SearchPolicy(query []float32, p Policy, topK int) []Hit`: compute coarse-sorted once →
  `Features` → `p.Plan` → run the existing residual-ADC search over the top-nprobe cells + exact re-rank of
  rerankK, reusing the already-sorted cells. Behavior identical to `Search`+`SearchRerank` at the chosen
  depth (asserted by test).

Tests: `SearchPolicy` with a constant policy returning `(np, rk)` returns exactly what `Search(query, np,
topK)` + `SearchRerank(... rk ...)` return; coarse distances are computed once (not recomputed for features).

## Component 4 — `cmd/gpevolve` (runner, mirrors `cmd/evolve`)

1. Load real 47k×768 (`-data /tmp/sembed-big/big_*`), build the index at `ivf.Recommended` geometry.
2. Split queries: fixed evolution set **E** (1000) and pristine holdout **H** (4000). Precompute exact
   top-10 GT for both (brute-force once; abort the run if the proxy ever admits brute force as "cheap").
3. **Phase 0 — feasibility gate** (Component 5). If it fails, print the verdict and exit without evolving.
4. Evolve on E with the Pareto fitness (Component 6). Proxy latency only.
5. Validate the champion on H across ≥3 kmeans seeds; champion = best worst-seed. Measure its `(recall@10,
   mean latency)` and compare to static `ivf.Recommended` measured identically. Confirm with single-thread
   wall-clock (anti-DCE) on the champion only.
6. Emit `results/gp-adaptive-*.json` + `.log` (the learned policy's string form, the Pareto points, the
   Phase-0 ceiling, E–H gap).

## Component 5 — Phase 0 feasibility gate

For each query in E, compute the **per-query minimum-depth oracle**: the cheapest grid `(nprobe, rerankK)`
(by proxy cost) that still retrieves the true top-10. Then:

- **Oracle-Pareto ceiling:** mean recall / mean latency if an oracle set per-query depth = the GP's upper
  bound. If this ceiling does not meaningfully beat static (e.g. < ~1pp recall at equal latency, or no
  latency cut at equal recall), **STOP** — there is no headroom.
- **Predictability:** correlation between the margin features and the oracle's needed depth (rank
  correlation / mutual information bucketed). If features don't separate easy from hard queries, **STOP** —
  the GP cannot learn what the oracle knows from these features.

Write the finding to `results/gp-adaptive-feasibility.md` (ceiling numbers, correlation, decision). Only
proceed past the gate if both checks pass.

## Component 6 — Fitness (Pareto via latency-budget)

Evolve on E. For an individual: run `SearchPolicy` over all E queries (proxy latency), compute mean
`recall@10` and mean proxy latency. Fitness:

```
budget   = mean proxy latency of static ivf.Recommended on E
penalty  = max(0, meanLatency − budget) · LAMBDA_LAT      // hard pressure to stay within budget
fitness  = meanRecall@10 − penalty − PARSIMONY · totalNodes
```

This searches for "max recall at ≤ static latency" — one corner of Pareto dominance; the runner also reports
the raw `(recall, latency)` point so a latency *cut* at equal recall is visible too. `LAMBDA_LAT` is set so
exceeding budget is never worth it; `PARSIMONY` is small (tie-break toward simpler trees). Both are constants
in `cmd/gpevolve`, documented inline.

## Anti-self-deception protocol (non-negotiable)

- **E fixed for fitness; H pristine for the verdict.** The champion's win must hold on H — queries never
  seen during evolution — proving the feature→depth mapping generalizes rather than memorizing E.
- **Proxy latency during evolution; wall-clock only on the champion** (single-thread, anti-DCE).
- **Multi-seed kmeans validation;** champion = best worst-seed; report E–H gap (overfit detector).
- **Bloat/overfit guards:** depth limit + parsimony.
- **Anchor test:** a constant policy must reproduce static `Search`/`SearchRerank` exactly (the η=0 analog).
- **Brute-force abort:** if the proxy ever ranks full-scan as within budget, abort (the GT is precomputed;
  the engine must not "win" by quietly doing brute force).

## Testing summary (TDD)

- `internal/gp`: protected ops, determinism, depth-limited variation, parsimony, known-value eval.
- `ivf`: `SearchPolicy` ≡ `Search`+`SearchRerank` at constant depth; coarse computed once; `Features` values
  on a hand-built coarse vector.
- `cmd/gpevolve`: the oracle-min-depth and recall@10 helpers on a tiny fixture; the static-baseline measure;
  the constant-policy anchor equals static.

## Out of scope (YAGNI)

- NSGA-II / explicit multi-objective front (latency-budget scalar is simpler and matches "dominate").
- **Production deployment.** This is R&D: Phase 0 + holdout decide whether the policy is real before any
  wiring into the live engine. A separate spec handles deployment if it wins.
- Features beyond coarse margins (PQ-code stats, query norm, etc.).
- More than two trees / multi-stage policies.
