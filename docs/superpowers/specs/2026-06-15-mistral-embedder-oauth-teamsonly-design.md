# Mistral Embedder + OAuth Teams-Only · Design Spec

> Date: 2026-06-15 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved → plan → subagent-driven implementation → deploy to server2 (8809, `matrix.blaze.net.do/mcp`).

## Problem

A second `sentrymcp` instance (server2, container `MatrixSentry2`, 10.10.10.175) will host three real teams
as isolated tenants (BlazeSphere=2, Kuadre=3, Round PlayGames=4) via the already-built multi-tenant-by-key
feature. Two gaps block that deploy:

1. **Embeddings**: today the only embedder is `memory.OllamaEmbedder` (the tesla Ollama, `nomic-embed-text-v2-moe`
   dim768). Server2 will embed via the **Mistral API** (`mistral-embed`, dim 1024) instead. There is no Mistral
   embedder in the codebase.
2. **OAuth requires the owner token**: `main.go` enables native OAuth only when `SENTRY_MCP_TOKEN` is set,
   because it reuses that secret as the JWT signing key. Server2 is **teams-only** (no owner token, no tenant-1
   corpus), yet teams need the claude.ai web connector (OAuth). The signing key must be decoupled from the
   owner token.

Everything else (the credential→tenant registry, per-request tenant routing, the comms channel, the memory
store) is already built and tenant-isolated; this spec adds only the two pieces above plus the deploy.

## Decisions (settled in brainstorming)

- **Embedder is an interface; add a sibling implementation.** The memory store depends on an embedder with
  `Embed([]string) ([][]float32, error)` and `Dim() int` (satisfied today by `OllamaEmbedder`). Add
  `memory.MistralEmbedder` mirroring `OllamaEmbedder`'s shape and validation. No change to `memory.Store`.
- **Provider selection by env, default unchanged.** A new `SENTRY_EMBED_PROVIDER` (`ollama` default | `mistral`)
  picks the embedder in `main.go`. Default stays `ollama` so the live 8808 server is byte-for-byte unchanged.
- **Mistral defaults: `mistral-embed`, dim 1024.** The API key comes from `SENTRY_MISTRAL_API_KEY` (required
  when provider=mistral), is never logged, and never lives in git/chat — set out-of-band on server2.
- **Server2 is a fresh journal** → 1024-dim from day one, no migration. Dimension consistency is per-server.
- **OAuth signing key from its own env, fallback to owner token.** Source the OAuth approval/signing secret
  from `SENTRY_OAUTH_KEY`; if empty, fall back to `SENTRY_MCP_TOKEN` (preserves 8808 behavior). This lets OAuth
  run teams-only (signing key set, no owner token). The consent passphrases are the team secrets — already
  resolved via `tenantFor = s.tokens.Tenant` against the registry, so no consent-path change is needed.
- **Server2 has no owner tenant.** No `SENTRY_MCP_TOKEN` → registry holds only the three team entries
  (`loadTokenRegistry` adds the owner entry only when `ownerSecret != ""`). The open-mode bypass fix already
  requires a valid team bearer when the registry is non-empty.

## Architecture

### Component 1 — `memory/mistral.go` (new; mirrors `memory/ollama.go`)

- `type MistralEmbedder struct { url, model, apiKey string; dim int; client *http.Client }`.
- `func NewMistralEmbedder(apiKey, model string, dim int) *MistralEmbedder` — base URL fixed to
  `https://api.mistral.ai`; default `client` timeout 2 min (matches Ollama).
- `func (e *MistralEmbedder) Dim() int { return e.dim }`.
- `func (e *MistralEmbedder) Embed(texts []string) ([][]float32, error)`:
  - `POST {url}/v1/embeddings` with header `Authorization: Bearer {apiKey}` and JSON body
    `{"model": e.model, "input": texts}`.
  - Response is OpenAI-style: `{"data": [{"embedding": [...]}, ...]}`. Decode `data[].embedding` **preserving
    request order** (Mistral returns an `index` field per item; v1 assumes API order matches input order — the
    same assumption Ollama makes — and validates count, see below).
  - Validate: non-200 → error including status (do NOT include the body, which may echo the key); decoded
    count == `len(texts)`; each vector's length == `e.dim` (catches a wrong model/dim).
  - The API key must never appear in any returned error or log line.

### Component 2 — provider selection (`cmd/sentrymcp/main.go`)

- New flag/env: `embedProvider := flag.String("embed-provider", envOr("SENTRY_EMBED_PROVIDER", "ollama"), …)`.
- New env read for the key: `SENTRY_MISTRAL_API_KEY` (no flag — secrets via env only).
- Replace the `if *ollamaURL != "" {` embedder block with a provider switch that builds an `embedder` value
  (the interface the store needs), then `memory.New(store, emb)` once:
  - `ollama` (default): unchanged — built only when `*ollamaURL != ""` (so 8808 with no provider env behaves
    exactly as today). Model/dim flags unchanged (`nomic-embed-text`, 768).
  - `mistral`: require `SENTRY_MISTRAL_API_KEY` (fatal, clear message if missing); model from
    `SENTRY_EMBED_MODEL` defaulting to `mistral-embed`; dim from `-embed-dim` defaulting handled so mistral
    uses **1024** when the operator hasn't overridden it. Build `memory.NewMistralEmbedder(key, model, dim)`.
  - unknown provider → fatal with a clear message listing the valid values.
- `moko.Info("semantic memory enabled", {"provider":…, "model":…, "dim":…})` — never log the key or URL creds.

> Dim default nuance: `-embed-dim` currently defaults to 768. To avoid silently sending 768 to Mistral, the
> plan resolves dim as: if the operator passed `-embed-dim`/it's non-default use it; for `mistral` with the
> default still in place, use 1024. Implemented by treating the mistral branch's dim as
> `if *embedDim == 768 (the ollama default) && provider==mistral { 1024 } else { *embedDim }`. Documented in
> code so it isn't mistaken for a bug.

### Component 3 — OAuth signing-key decoupling (`cmd/sentrymcp/main.go`)

Replace the OAuth-enable block (lines ~134–142):

```go
if *oauthIssuer != "" {
    secret := os.Getenv("SENTRY_OAUTH_KEY")
    if secret == "" {
        secret = os.Getenv("SENTRY_MCP_TOKEN") // back-compat: 8808 reuses the owner token
    }
    if secret == "" {
        fmt.Fprintln(os.Stderr, "sentrymcp: -oauth-issuer requires SENTRY_OAUTH_KEY (or SENTRY_MCP_TOKEN) as the JWT signing key")
        os.Exit(1)
    }
    s.oauth = newOAuth(*oauthIssuer, secret, s.tokens.Tenant)
    moko.Info("native OAuth enabled", map[string]string{"issuer": *oauthIssuer})
}
```

- `newOAuth`, the consent passphrase→tenant path (`tenantFor = s.tokens.Tenant`), JWT `tnt` claim, and
  `verifyToken` are all unchanged — team secrets already work as consent passphrases via the registry.
- 8808 back-compat: it sets `SENTRY_MCP_TOKEN` and no `SENTRY_OAUTH_KEY` → falls back to the owner token →
  identical signing key and behavior as today.

## Testing (TDD)

- **`memory/mistral_test.go`** (httptest mock, no network):
  - `Embed` posts `model`+`input`, sets `Authorization: Bearer <key>`, decodes `data[].embedding` in order,
    returns the vectors.
  - count mismatch (server returns fewer items than inputs) → error.
  - dim mismatch (vector length != configured dim) → error mentioning the dim.
  - non-200 → error including the status, and the error string does NOT contain the API key.
  - `Dim()` returns the configured dimension.
- **`cmd/sentrymcp` provider selection** (unit, no network — assert wiring, not live embeds):
  - unknown `SENTRY_EMBED_PROVIDER` → the resolver returns an error (extract a small testable
    `resolveEmbedder(provider, …) (embedder, error)` helper so this is unit-testable without `main`).
  - `mistral` without `SENTRY_MISTRAL_API_KEY` → error.
  - `mistral` with key → a `*memory.MistralEmbedder` whose `Dim()==1024` by default.
  - `ollama` with a URL → a `*memory.OllamaEmbedder` (back-compat).
- **OAuth signing-key** (unit): a helper `oauthSigningKey(env)` (or equivalent) returns `SENTRY_OAUTH_KEY`
  when set, else `SENTRY_MCP_TOKEN`, else "" (caller fatals). Round-trip: `newOAuth` with a team-secret
  registry mints an access token for a team passphrase whose `tnt` == the team tenant (reuses existing OAuth
  test helpers).

## Deployment (server2 — 8809, leaves 8808 untouched)

1. **Build + ship**: `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/sentrymcp ./cmd/sentrymcp`;
   `scp /tmp/sentrymcp root@10.10.10.175:/root/sentrymcp`.
2. **`tokens.json`** generated ON server2 (secrets via `openssl rand -hex 16`, never in chat), one line per
   team (tenant 2/3/4), `chmod 600`; print once for out-of-band delivery; thereafter
   `ssh matrix-sentry2 'cat /root/sentry-tokens.json'` retrieves them.
3. **env `/root/sentrymcp-mt.env`** (chmod 600): `SENTRY_TOKENS_FILE=/root/sentry-tokens.json`,
   `SENTRY_EMBED_PROVIDER=mistral`, `SENTRY_MISTRAL_API_KEY=<key>` (set by owner), `SENTRY_EMBED_MODEL=mistral-embed`,
   `SENTRY_OAUTH_KEY=<openssl rand -hex 32>`, `SENTRY_OAUTH_ISSUER=https://matrix.blaze.net.do`,
   `SENTRY_DEDUP_TAU=0.45`. **No** `SENTRY_MCP_TOKEN`.
4. **systemd `sentrymcp-mt`** (`Restart=always`): `ExecStart=/root/sentrymcp -http 0.0.0.0:8809 -dir /root/sentry-journal-mt`,
   `EnvironmentFile=/root/sentrymcp-mt.env`.
5. **Cloudflare tunnel** `matrix.blaze.net.do` → `http://localhost:8809` (serves `/mcp` and the OAuth
   `/.well-known/*`, `/authorize`, `/token`, `/register` at the host root). Disable Bot Fight Mode for the zone.
6. **Verify**: isolation (remember as BlazeSphere bearer → recall as Kuadre bearer must NOT see it); each team
   sees 9 tools; OAuth consent with a team passphrase mints a tenant-scoped token (claude.ai connect).

## Out of scope (YAGNI)

- Retry/backoff on Mistral 429/5xx (v1 surfaces a clear error; add if rate limits bite in practice).
- Per-text Mistral batching/token-limit splitting (memory embeds 1 text per remember/recall; batches are tiny).
- Migrating tenant-1 (8808) to Mistral (different server, different journal; out of scope by design).
- A flag for the Mistral base URL (fixed to `https://api.mistral.ai`; add only if a proxy is needed).
- Dynamic provider switch at runtime (set by env at startup; restart to change).
