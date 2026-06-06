# Matrix Sentry

> Operational memory for code agents. A from-scratch vector-search + memory engine
> in pure Go — **zero ML libraries, zero Postgres, zero external dependencies** —
> that gives AI coding agents (Claude Code, Cursor, …) a persistent, curated memory
> of *what was done, why, and what changed*, so they stop repeating work and losing
> the thread across long sessions and across repos.

In one line for a human: **it stops the AI from having amnesia.**

- **Owner:** Alvin Nuñez (Blaze / Telemarch)
- **Status:** Semantic engine built and validated at 1M scale. Product layer (SentryLog) in design.
- **Boundary:** Matrix Sentry = the agent's *decision/change memory*. Runtime app logs are a separate system (MokoBlinks). Don't mix.

---

## The thesis — Access-Driven Rate-Distortion Indexing

General ANN engines (FAISS, ScaNN, PQ) minimize rate-distortion under a **uniform**
usage prior: `min Σ_i w_i·D_i(b_i)` with `w_i = 1` for every item and every query.

Matrix Sentry replaces that uniform weight with one read off the **agent's logged
access process**:

```
min_alloc  Σ_i  w_i(access) · D_i(alloc)      s.t.  Σ_i bytes_i ≤ B
```

The index — bit budget, codebook geometry, and topology — is optimized against how
the memory is *actually used*, not a uniform query prior. **The moat is the access
log itself: general ANN libraries structurally lack it.** Everything below is a
corollary of this idea.

---

## What's built (and measured on SIFT1M, CPU-only)

| Layer | What it is | Result (verified) |
|---|---|---|
| **`pq/`** | Hand-written Product Quantization (ADC, parallel, deterministic) | Reproduces the FAISS/Jégou SIFT1M baseline: recall1@100 ≈ 0.92, 64× compression |
| **`ivf/`** | IVFADC (inverted file + residual PQ) on top of frozen `pq` | nprobe=16 Pareto-dominant over flat scan: ~5× lower latency, scans 2% of the index |
| build-time fix | FAISS-style coarse subsample + fewer iters | train **9m35s → 2m11s (4.4×)**, recall unchanged |
| **CA-IVFADC** | *Content-addressed* index — vector identity is intrinsic to its quantized geometry | Save 29 MB, **load 538 ms** (build-once/serve-instantly); auto-deduped 14,538 byte-identical SIFT vectors for free |
| **access-gated refinement** | A 2nd residual code given only to access-hot items | **~3–7× more byte-efficient than blind allocation** at improving access-weighted recall (robust across 18 configs, vs an equal-budget random control) |
| **Mechanism D** | *Predictive* allocation — refine what's about to be accessed (online Markov) | Beats the marginal baseline by up to **+0.08 recall@10** at tight budgets when access is sequential; **η=0 sanity passes exactly** (no false positive) |

All of it runs CPU-only, no GPU, no Ollama — the "validate on the weakest hardware"
strategy: recall is hardware-independent, so faster hardware only improves speed.

---

## Repository layout

```
pq/                 FROZEN PQ core: New/Train/Encode/Search/Save/Load (parallel, deterministic)
ivf/                Production CA-IVFADC: Config/New/Train/Add/Search/Recall/Save/Load,
                    content-addressed Handle{Hash,Cell,Code}, exact dedup, gob persistence
internal/lab/       shared, verified-identical engine math for experiments (geometry, ADC, k-means)
internal/refine/    novel cores (TDD'd): access model (Zipf/popularity/gating), Markov predictor
cmd/sift1m          SIFT1M benchmark for the flat PQ engine
cmd/ivf1m           IVFADC benchmark + nprobe sweep
cmd/ivfindex        CA-IVFADC demo: build → Save → Load → search + content-address demo
cmd/ivfdiag         error-budget diagnostic (oracle-cell recall, distortion→rank, etc.)
cmd/ivfrefine       access-gated refinement experiment (vs random control)
cmd/ivfsweep        efficiency-curve sweep (M2 × zipf × hotfrac × rerankK)
cmd/ivfpredict      Mechanism D: predictive vs marginal allocation on a self-exciting stream
sentry/             (next) SentryLog Event Log: append-only journal + keydir + recovery + tenant
docs/superpowers/   design specs and implementation plans
```

`pq` and `ivf` are the engine; `internal/*` + `cmd/*` are the R&D harness; `sentry/`
is the product layer being built next.

---

## Build & test

Requires Go ≥ 1.24, no other dependencies.

```bash
go build ./...
go test ./...           # ivf + internal/refine unit tests (incl. cross-core determinism)
go vet ./...
```

Benchmarks need the SIFT1M TEXMEX dataset (`*_base/learn/query.fvecs`,
`*_groundtruth.ivecs`) under a data dir; they cross-compile to a static Linux binary
for a headless test box:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o ivf1m ./cmd/ivf1m
./ivf1m -dir /data/sift -prefix sift -nlist 1024 -m 8 -k 256 -nprobe 16
```

---

## Roadmap

- **SentryLog (Lite MVP)** — append-only journal + keydir + crash recovery + per-tenant
  isolation + an `Access` event, plus an analyzer that measures whether a *real* agent
  access stream has the sequential structure Mechanism D exploits. Closes the
  synthetic-access caveat and starts the usable product in one move. *(spec written.)*
- Object store (content-addressed blobs), dedup index, MCP tools — later specs.
- Mechanism B (co-access **topology**) — the structural dark horse of the thesis.
- Scenario B — real embeddings (Ollama, `nomic-embed-text`) for end-to-end semantic recall.

See `docs/superpowers/specs/` for the design specs and `docs/superpowers/plans/` for
implementation plans.

---

## Design principles

- **Build it from scratch** — own the engine end to end; no FAISS/Qdrant/Postgres.
- **Single-user / append-only by nature** — no need for multi-user DB durability.
- **Multi-tenant from day one** — every record carries a tenant; queries never cross tenants.
- **Determinism** — same result regardless of core count (verified GOMAXPROCS=1 vs 8).
- **Measure, don't assume** — every claim is verified on real data, and adversarially
  re-checked before it's believed.
