# Admin Dashboard — Live Data (v2) · Design Spec

> Date: 2026-06-15 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved → plan → subagent-driven → deploy. Scope: wire the Vector Galaxy dashboard to REAL memories
> from the personal server (tenant 1, mcp.blazesphere.net / 8808). Teams (8809) come for free via the same path.

## Problem

The admin dashboard (cmd/sentryadmin) renders a Claude Design artifact whose data is 100% synthetic
(`corpus.js`, a seeded PRNG). The owner wants to SEE his real memories — the personal tenant-1 corpus, where
he has the most. The MCP server exposes only `recall` (k-NN) and `stats` (counts); there is no way to
enumerate a tenant's full corpus with vectors, which the galaxy needs (every memory is a point).

## Decisions (settled in brainstorming)

- **Token stays server-side.** The browser never holds an MCP bearer. `sentryadmin` (Go, on internal server2)
  fetches the corpus from the MCP using a configured bearer and serves already-projected JSON to the frontend.
- **New authenticated read endpoint on `sentrymcp`.** `GET /admin/corpus` → `resolveTenant(r)` (existing bearer
  → tenant routing) → the tenant's memories with vectors + stats. Requires rebuilding and redeploying the live
  8808 (and 8809) servers — additive, append-only journal, ~2s restart (owner authorized).
- **Projection server-side, deterministic.** `sentryadmin` projects the 768-d vectors → 3D with PCA (power
  iteration, implicit covariance) and clusters with deterministic k-means; emits the EXACT shape `corpus.js`
  produces so the frontend swap is a thin shim.
- **Graceful fallback.** If the live fetch fails, the frontend falls back to the original mock — the dashboard
  never breaks.
- **Scope tenant 1 first.** Same code serves any tenant (the MCP bearer selects it); start with personal.
- **Stays internal + basic-auth** (owner choice from v1). No public route.

## Architecture

```
browser (LAN) ──GET /api/galaxy?tenant=personal──▶ sentryadmin (server2, holds bearer)
                                                      │  GET /admin/corpus  (Authorization: Bearer <token>)
                                                      ▼
                                                   sentrymcp 8808 (resolveTenant → tenant 1)
                                                      │  memory.Store.List(1)
                                                      ▼  {memories:[{id,text,tags,src,vec}], stats}
                          PCA(vec→3D) + kmeans + labels → {tenant,clusters,points} (corpus.js shape)
```

### Component 1 — `memory.Store.List(tenant) []MemoryPayload` (memory/memory.go)

Read-only enumeration of a tenant's live entries (under the store mutex), returning a copy slice of
`MemoryPayload` (id, text, vec, tags, src). Other tenants' entries are excluded. No embedding, no ordering
guarantee beyond insertion order.

### Component 2 — `GET /admin/corpus` (cmd/sentrymcp)

- Registered in `serveHTTP`'s mux BEFORE the catch-all `/`.
- `resolveTenant(r)`; on `!ok` → 401 (same as MCP). On ok and `s.mem == nil` → 503 "memory disabled".
- Body: `{"tenant":<n>,"dim":<embedder dim>,"count":<n>,"memories":[{"id","text","tags","src","vec":[...]}]}`.
- Server-to-server only (sentryadmin calls it); no CORS needed.

### Component 3 — projection (`cmd/sentryadmin/galaxy.go`)

- `pca3(vectors [][]float32) [][3]float32`: mean-center; 3 components by power iteration using the implicit
  covariance action `C·v = Xᵀ(X·v)` (no 768×768 matrix); Gram-Schmidt-orthogonalize each candidate against the
  already-found components every iteration; project. Deterministic (fixed init vectors). Output scaled so the
  cloud spans roughly ±15 (matches the mock's visual scale): divide each axis by its std-dev, multiply by ~9.
- `kmeans(points [][3]float32, k int) (assign []int, centers [][3]float32)`: deterministic farthest-point init
  + ~25 Lloyd iterations. `k = min(6, max(2, n/12))`.
- These are pure, unit-testable functions (no I/O).

### Component 4 — `GET /api/galaxy` (cmd/sentryadmin)

- Fetches `/admin/corpus` from `SENTRY_ADMIN_MCP_URL` with `Authorization: Bearer $SENTRY_ADMIN_MCP_TOKEN`
  (both new env; the URL is the LAN MCP, the token is the tenant's bearer). 30s client timeout.
- Projects (Component 3), clusters, then builds the **exact `corpus.js` output shape**:
  - `tenant`: `{key,name,glyph,accent}` (from a small static map; "personal" for tenant 1).
  - `clusters`: `[{key,label,color,center:[x,y,z],count}]` — label = most-common first-tag in the cluster (else
    `"grupo N"`); color from the design palette by cluster index; center = 3D centroid.
  - `points`: `[{id,tenant,cluster,clusterKey,clusterLabel,color,pos:[x,y,z],text,tags,source,access,heat,createdAt,dim}]`
    — `id` = `"m"+memID`; `access`/`heat` derived from id-rank as a recency proxy (newer = hotter; documented as
    a proxy until real access counts are wired); `createdAt` = now (timestamps are a follow-up); `dim` = corpus dim.
- On any error (fetch/parse) → HTTP 502 with a small JSON error so the frontend can fall back.
- `GET /api/comms?tenant=` (optional, thin): returns `[]` for now (real comms wiring is a follow-up) so the
  frontend's comms view degrades to empty rather than erroring.

### Component 5 — frontend shim (`cmd/sentryadmin/assets/live.js` + minimal index.html edits)

- `live.js` defines `window.MatrixLive`: `prime(tenantKey)` fetches `/api/galaxy?tenant=<key>` and caches the
  result; after a successful prime it OVERRIDES `window.MatrixCorpus.generate(key)` to return the cached live
  data and `window.MatrixCorpus.comms(key)` to return live comms — each **falling back to the original mock
  function** if no live data is cached for that tenant (fetch failed).
- `index.html` `_boot`: add `"live.js"` to the script list (after `corpus.js`), and `await
  window.MatrixLive.prime(this.state.tenant)` immediately before the first `generate(...)` call. In
  `_switchTenant`, `await window.MatrixLive.prime(key)` before its `generate(...)`. These are the only edits to
  the artifact; everything else is untouched. (`generate` stays synchronous — prime populates the cache first.)
- Net effect: real galaxy when the backend is reachable; identical mock when it isn't.

## Testing (TDD)

- `memory`: `List(tenant)` returns only that tenant's entries with vectors; excludes other tenants; reflects a
  Forget (dropped entry absent).
- `cmd/sentrymcp`: `/admin/corpus` with a valid bearer → 200 + the tenant's memories (count matches); wrong/no
  bearer → 401; isolation (team bearer never sees tenant-1 rows); `mem==nil` → 503.
- `cmd/sentryadmin` (`galaxy_test.go`): `pca3` on a known anisotropic blob puts the dominant axis on component
  0 (variance ordering); output is finite and centered. `kmeans` on 3 well-separated 3D blobs recovers 3 pure
  clusters (each cluster's points share a true label). Determinism: same input → same assignment twice.
- `cmd/sentryadmin` (`api_test.go`): `/api/galaxy` against an `httptest` stub MCP returns the corpus.js shape
  (points have `pos` length 3, `clusterLabel`, `color`; clusters non-empty; count == stub count); stub 500 →
  `/api/galaxy` returns 502.

## Deployment

1. Build `sentrymcp` + ship; **redeploy 8808 (personal) and 8809 (teams)** (`systemctl restart`). Additive
   endpoint; journal untouched; clients reconnect.
2. On server2, add to a new `/root/sentryadmin.env` lines: `SENTRY_ADMIN_MCP_URL=http://<server1-LAN>:8808`,
   `SENTRY_ADMIN_MCP_TOKEN=<personal SENTRY_MCP_TOKEN>` (set out-of-band; never in chat/git). Rebuild + ship
   `sentryadmin`; `systemctl restart sentryadmin`.
3. Verify with Playwright over the SSH tunnel: the galaxy shows real memories (cluster labels match real tags;
   clicking a node shows real text; the metric count == `stats`); confirm a known recent memory appears.

## Out of scope (YAGNI / follow-ups)

- Real access-frequency heat (needs journal Access aggregation) — using an id-rank proxy for now.
- Real per-memory timestamps / createdAt (journal has them; not surfaced yet).
- Live journal stream + real access-control key panel + live comms content.
- Multi-tenant selector wired to all team corpora at once (sentryadmin would hold each team bearer); start with
  the single configured tenant.
- Public exposure / stronger auth (stays internal + basic-auth per the v1 decision).
