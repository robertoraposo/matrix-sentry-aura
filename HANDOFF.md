# Matrix Sentry · Handoff

> Read this first, then check the auto-loaded memory index (MEMORY.md is loaded
> every session) for the R&D details. This doc is the **operational + product
> state**; the memory files hold the verified R&D results.

**What it is:** operational memory for code agents — a from-scratch pure-Go vector
+ memory engine (zero deps, zero Postgres) that stops AI coding agents from having
amnesia. **Thesis:** *Access-Driven Rate-Distortion Indexing* — replace FAISS's
uniform `w_i=1` with a weight read off the agent's logged access process; the moat
is the access log itself.

- **Owner:** Alvin Nuñez (AlvinTLC). Comms: Spanish; code/docs: English.
- **Repo:** `github.com/AlvinTLC/matrix-sentry` (**PRIVATE**), default branch `main`.
- **Boundary:** Matrix Sentry = agent decision/change memory. MokoBlinks = runtime
  logs/observability (we USE it to debug the engine — that's allowed; don't mix the memory layers).

---

## Current state (as of this handoff)

### Engine (verified on SIFT1M, CPU-only) — `pq/`, `ivf/`
- `pq/` FROZEN PQ core: reproduces FAISS baseline (recall1@100≈0.92, 64×).
- `ivf/` production **CA-IVFADC**: Config/New/Train/Add/Search/Recall/Save/Load,
  content-addressed `Handle{Hash,Cell,Code}`, exact dedup, gob persistence.
  Build-time fix 4.4× (9m35s→2m11s). Load 538ms. Auto-deduped 14,538 SIFT vecs.
- R&D (`internal/{lab,refine}` + `cmd/{ivfdiag,ivfrefine,ivfsweep,ivfpredict}`):
  - **access-gated refinement**: ~3–7× more byte-efficient than blind (18-cell sweep, vs random control).
  - **Mechanism D (predictive/Markov)**: beats marginal up to +0.08 recall@10 at tight budgets; η=0 sanity passes.
  - Full details + numbers in memory files: `access-gated-refinement.md`, `access-driven-rd-indexing.md`, `ca-ivfadc-error-budget.md`.
- **Scenario B** (`cmd/sembed`, merged PR #4): engine validated on REAL 768-d Ollama
  embeddings (nomic-embed-text): flat PQ recall1@100=1.000 at 32×. Needs Ollama to run.

### Product layer — `sentry/`, `sentry/access/`, `mokoblinks/`, `cmd/sentrymcp/`
- **SentryLog Event Log** (built by Devin per our spec, PR #1; Qodo-fixed PRs #2/#3): append-only
  journal + keydir + crash recovery + per-tenant isolation. Reviewed by us: sound, spec-faithful.
  Spec: `docs/superpowers/specs/2026-06-05-sentrylog-event-log.md`.
  - `sentry/record.go` 27-byte framed record (crc32|seq|tstamp|type|tenant|len|JSON payload).
  - `sentry/store.go` Open/Append/Read/Scan/Close. `sentry/access/analyze.go` = the convergence
    instrument: measures `lift = Markov − marginal` next-access hit-rate (the SAME Markov as Mechanism D).
  - `cmd/sentrydemo` proves it end-to-end (append→crash→recover→analyze, lift detected, tenant isolation).
- **`mokoblinks/`** (NEW, this session, TDD'd): pure-Go fire-and-forget client for the MokoBlinks
  log platform (`POST /v1/ingest`). Env-driven, no-op without keys, never panics.
- **`cmd/sentrymcp/`**: minimal pure-Go MCP server, two transports — stdio (local) and
  **Streamable HTTP** (`-http :PORT`, remote). Tools: `record_access`, `analyze_access`, `stats`.
  Every call → journal + MokoBlinks. Optional bearer auth via `SENTRY_MCP_TOKEN`.
  `record_access` now accepts `item` (int, synthetic), `path` (string) or `paths` ([]string, batch)
  plus `src` (originating tool); path(s) go through the registry below.
- **Path registry** (`sentry/registry.go`, NEW): maps file paths → stable **sequential** item ids
  per tenant. First sight of a path writes an `EventPathMap{id,path}` record (schema v2:
  `EventType EventPathMap=2`), so the **journal is the single source of truth** for the dictionary;
  `NewRegistry` rebuilds the map on `Open` by scanning. `AccessPayload` gained `Source` (back-compat).
- **`cmd/sentry-record/`** (NEW, this session): the **PostToolUse hook** (the convergence payoff).
  Pure-Go, reads the hook JSON on stdin, extracts the *existing regular files* a tool touched
  (Read/Edit/Write/MultiEdit/NotebookEdit via file_path; Grep/Glob via response; Bash via command
  tokens — all gated by `os.Stat`), and batch-POSTs them to `record_access` with `src`=tool.
  Fire-and-forget, no-op without `SENTRY_MCP_URL`, never blocks/breaks the tool use. Config from env
  or `~/.matrix-sentry.env`.

### Live deployment (the bridge for Claude Code)
- **MCP server running on the homelab VM** as systemd service `sentrymcp` (Restart=always,
  survives reboot), listening `0.0.0.0:8808`, journal at `/root/sentry-journal`, tenant 1.
- **Endpoint:** `http://10.10.10.96:8808/mcp` (reachable across Alvin's whole network).
- **Auth token** + **MokoBlinks key** live in `/root/sentrymcp.env` on the VM (chmod 600, NOT in git).
  The MCP token is also in the Mac's `~/.claude.json` (retrieve via `claude mcp get matrix-sentry`).
- **Claude Code is registered** (user scope): `matrix-sentry: ✓ Connected`. Tools appear in NEW sessions.
- MokoBlinks verified reporting with the matrix key (`app: matrix-sentry`, `{"status":"ok"}`).

### Infrastructure
- **Homelab VM:** `ssh matrix-sentry` → root@10.10.10.96 (8 cores/14 GiB, has SIFT1M at /data/sift). Hosts the live MCP server.
- **Tesla box:** `ssh tesla` → alvintlc@100.93.11.62 (24c/31GiB, NVIDIA A2 GPU; for LOPQ/Ollama). No SIFT yet.
- Compile-on-Mac → ship-binary (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64`). Both VMs lack a Go toolchain.

---

## Done this session
1. ✅ **Live journal reset** — `/root/sentry-journal` is pristine (0 events) on the redeployed server.
2. ✅ **sentrymcp redeployed** to the VM with schema v2 (`item`/`path`/`paths`/`src` + path registry).
   Verified live: `record_access {paths}` assigns sequential ids; `tools/list` shows the new args.
3. ✅ **PostToolUse hook built + wired** (`cmd/sentry-record`, full TDD). Binary at
   `~/.local/bin/sentry-record`, secrets at `~/.matrix-sentry.env` (chmod 600), hook registered
   **global** in `~/.claude/settings.json` (matcher Read|Edit|Write|MultiEdit|NotebookEdit|Grep|Glob|Bash,
   `async`). End-to-end verified: helper → HTTP → registry → journal → analyze. The hook auto-activated
   mid-session; it is LIVE — natural work streams real access into tenant 1.
4. ✅ **MokoBlinks flush fix** (`cmd/sentrymcp` handleHTTP) — HTTP mode never flushed; logs buffered
   forever so the live mirror looked empty. Now `defer go s.moko.Flush()` per request. (The journal
   was always correct — the journal and MokoBlinks are SEPARATE channels.)
5. ✅ **Engine squeezed at the REAL η** (the "does it help Claude" proof). Live access measured
   lift 10.3%→14.8% (climbs with volume). `cmd/streamlift`+`refine.StreamLift` calibrate eta→lift:
   real η ≈ **eta 0.30–0.33**. Axis 1 (ivfpredict / Mechanism D): at that point **D−static = +0.021
   recall@10** @frac0.002, eta=0 sanity perfect, headroom +0.096@eta0.9. Axis 2 (ivfsweep / access-gated):
   **5.6–9.5× byte-efficiency** vs random at ESS=4028 (valid); ESS guardrail kills zipf=1.0. Both beat
   FAISS-uniform at the real access pattern. Caveat: SIFT vectors still synthetic (only η real).
6. ✅ **One-shot installer** for other machines: `cmd/distserve` (no-auth static server, systemd
   `sentrydist` on VM :8810) + `cmd/distserve/install.sh`. `curl -fsSL http://10.10.10.96:8810/install.sh
   | SENTRY_MCP_TOKEN=<tok> sh` does everything (arch-detect, sha256-pinned download, env, MCP, hook).
   Token passed inline (never on the no-auth URL). Hardened per security review: allowlist + symlink
   reject + sha256 pinning. All this session's work on branch `feat/posttooluse-access-hook` → **PR #5**.

## Real-embeddings track — DONE this session, with a major finding
Ran on tesla (Ollama nomic-embed-text, 768-d). `cmd/sembed -emit` wrote a 10k/1k real dataset
(`/tmp/sembed/real_*`, still live). Results (see [[access-driven-rd-indexing]] memory for full detail):
- **Access-gated refinement does NOT transfer from SIFT — it HURTS on real embeddings** (acc@10w < base
  across m=8/16/32; ivfpredict D−static negative; oracle@10 < base). The SIFT +2pp was geometry-specific.
- **nprobe check refuted "routing bottleneck"**: full-scan recall@10=0.64 ≈ nprobe16 → routing is fine @10.
- **Real bottleneck = PQ quantization on ANISOTROPIC data** (flat-PQ@10=0.65 vs exact 0.91; ρ̄·D=207).
- ivfdiag's "oracle@10=0.956" is within-cell-optimistic; trust the flat full-scan (0.646) as the ceiling.

## SCALE TEST settled the allocation thesis (47k real vectors, 27 books)
`tesla:/tmp/sembed-big/big_*` (47,674 base / 5,000 query). Re-ran ivfsweep + anisotest (2 seeds) +
ivfpredict at 5× scale — **scale CONFIRMS, does not change, the 10k verdict**: access-gated HURTS
(acc<base<rnd), F-anisotropic is NEUTRAL (the 10k +2pp was seed noise: 2 seeds → −0.3/−0.04pp),
Mechanism D HURTS (worse with predictability), oracle@10<base, bytes dominate.

**FINAL VERDICT:** no per-item allocation / quantization-shape mechanism (access-driven OR general)
gives a robust recall gain on real 768-d text embeddings at any scale. SIFT's +2pp/6× was
geometry-specific and does NOT transfer. **The access log's value is OPERATIONAL/TEMPORAL (caching,
prefetching — the live lift IS a cache hit-rate gain), NOT ALLOCATIONAL (bits per item).** See
[[access-driven-rd-indexing]] for the full chain. Merged: PR #5, PR #6. Open branch
`feat/scale-real-embeddings` (sembed glob + this HANDOFF).

## Pending / next steps (Alvin's queue)
1. **Merge `feat/scale-real-embeddings`** (sembed `*.txt` glob + HANDOFF/scale verdict).
2. **Operational reframe (the now-clear path for the moat)**: access-driven exact-tier caching +
   Mechanism-D prefetching, measured by cache hit-rate / cost-per-query under the real access
   distribution — where the proven lift (~19% live) directly pays off and there is NO ranking-distortion
   problem. This, not RD bit-allocation, is where the access log delivers on real data.
3. Durable assets stand: the live capture (hook + journal + analyze) and the Markov next-access predictor.
4. Engine quantization on real text: just spend bytes (M) — no smart-allocation win to chase.
5. Later: SentryLog roadmap (CAS, dedup `task.check`, more MCP tools, `memory.recall`).

## Operational notes
- VM service: `systemctl {status,restart} sentrymcp`; binary `/root/sentrymcp` (prev `/root/sentrymcp.old`),
  env `/root/sentrymcp.env`, journal `/root/sentry-journal`. Reset = stop, `rm *.log`, start.
- MCP endpoint `http://10.10.10.96:8808/mcp`; token via `claude mcp get matrix-sentry`.
- Hook scope is global (every project → tenant 1, the honest "agent's working life" stream). The
  `Source` tag on each access lets `analyze_access` be segmented by tool later (e.g. Read-only lift).

## How to test the bridge right now
New Claude Code session on any project → ask it to use `record_access` while working, then
`analyze_access` → watch MokoBlinks Log Explorer (`app: matrix-sentry`) live.

## Key decisions / constraints
- Pure Go, zero external deps (Ollama is the only allowed external, for embeddings). JSON payload (not CBOR) for v1.
- Determinism: same result regardless of core count (engine property; verified).
- Multi-tenant day one. Single-user/append-only by nature.
- Verify everything on real data; adversarially re-check before believing (3 review workflows this
  session caught 2 metric bugs + 1 statistical confound — that rigor IS the product).

---

## RESUME PROMPT (paste into a fresh session)

> Retomamos Matrix Sentry (motor de memoria para agentes, Go puro, repo privado
> github.com/AlvinTLC/matrix-sentry). Lee HANDOFF.md y la memoria auto-cargada
> (MEMORY.md) ANTES de actuar. Estado: el motor (pq/ivf/CA-IVFADC) y la I+D
> (access-gated refinement, Mecánica D predictiva, tesis "access-driven RD
> indexing") están validados; el SentryLog (sentry/) lo construyó Devin sobre
> nuestra spec y lo revisamos (sólido); MokoBlinks (mokoblinks/) y el puente MCP
> (cmd/sentrymcp, Streamable HTTP) están construidos y **desplegados vivos** en la
> VM homelab como systemd `sentrymcp` en http://10.10.10.96:8808/mcp, con Claude
> Code ya conectado (user scope). Secretos en /root/sentrymcp.env de la VM y el
> token en ~/.claude.json (claude mcp get matrix-sentry). Pendientes: (1) resetear
> el journal /root/sentry-journal, (2) commit+push de mokoblinks/ y cmd/sentrymcp/
> (sin secretos), (3) construir el hook PostToolUse para capturar acceso REAL
> automático y medir η real con analyze_access. Confirma que `go build ./... &&
> go test ./...` está verde y que la VM sigue sirviendo (`ssh matrix-sentry
> systemctl is-active sentrymcp`; `curl -s http://10.10.10.96:8808/`), y sigue por
> el pendiente que te diga. Acceso VMs: `ssh matrix-sentry` (homelab, 10.10.10.96),
> `ssh tesla` (100.93.11.62).
