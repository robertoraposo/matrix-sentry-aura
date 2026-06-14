# GP Adaptive Policy — Phase 0 Feasibility Report

> Run: `gpevolve -data /tmp/sembed-big/big -e 1000 -h 4000 -phase feasibility` on tesla (2026-06-13).
> Real 47,674×768 nomic-embed-text embeddings, exact GT. **Verdict: GATE FAIL — STOP (do not evolve).**

## Dataset
- N=47674  dim=768  learn=47674  |E|=1000  |H|=4000

## Index (ivf.Recommended)
- nlist=64  M=96  K=64  nprobe=32  rerankK=200

## Static Baseline (E set)
- recall@10 = 0.9740
- mean proxy = 331037 ops

## Oracle Min-Depth (E set)
- recall@10  = 0.9990  (queries where true NN retrievable at some grid depth)
- mean proxy = 15210 ops
- headroom vs static: 95.4%  (oracle / static = 0.046)

## Feature–Depth Correlations (Pearson)
| Feature | Corr |
|---------|------|
| f0(d_near) | 0.0315 |
| f1(ambiguity) | -0.0461 |
| f2(margin) | -0.0488 |
| f3(spread) | -0.0732 |

max |corr| = 0.0732

## Verdict
**GATE FAIL — max|corr|=0.073 <= 0.2 (features not predictive)**

---

## Interpretation (what the numbers mean)

The gate cleanly separated two questions a naive build would have conflated:

1. **Is there per-query headroom?** YES, enormous. A per-query oracle reaches **higher** recall (0.999 vs
   0.974) at **4.6%** of static's cost — 95% latency headroom. Adaptive per-query depth is a real, large
   opportunity: most queries need far less than nprobe=32/rerankK=200, and a few need more.

2. **Do the coarse-margin features predict the needed depth?** NO. All four features correlate ≈0 with the
   oracle's min-depth (max |Pearson| = 0.073, not borderline — robust to any reasonable threshold). The
   margins of the *coarse* (centroid) routing simply do not carry the information about how deep a query
   must search to surface its true NN.

**Conclusion:** a GP (or any policy) over coarse-margin features would evolve on noise — it cannot learn what
the oracle knows because the inputs are blind to it. Building/running the GP engine on these features could
not have produced a robust Pareto win. The cheap gate (minutes on tesla) prevented a ~2.5h evolution run +
engine integration that was doomed by construction. This is a successful outcome of the feasibility-first
design, not a failure.

## What survives / redirect

The 95% headroom says the **lever is real, the feature is wrong.** A future attempt should use a
**post-search** signal instead of the pre-search coarse margins — e.g. the gap/ratio between the top ADC
candidate distances of the shortlist (cheap to compute *after* the ADC scan, before the exact re-rank), which
directly reflects how contested the top-10 is. That is a *different* design (features computed mid-search →
adaptive rerank depth), warranting its own spec; it is NOT the coarse-margin GP specced here.

## Status of the built artifacts (kept, tested, green)

`internal/gp` (deterministic GP engine), `ivf.Features`/`QueryFeatures`/`SearchPolicy`, and `cmd/gpevolve`
(Phase-0 measurement + the `feasibility` phase) are committed, TDD'd, and reusable. Only the *evolve* phase
(Tasks 5–6 of the plan) is not pursued, per this gate. If the redirected feature set is tried later, the GP
engine and SearchPolicy plumbing are already in place.
