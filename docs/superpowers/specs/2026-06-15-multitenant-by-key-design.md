# Multi-Tenant by Key · Design Spec

> Date: 2026-06-15 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved → plan → subagent-driven implementation → deploy to a SECOND server (current 8808 untouched).

## Problem

Today `cmd/sentrymcp` pins ONE tenant for the whole process (`-tenant` flag, default 1). The bearer token
(`SENTRY_MCP_TOKEN`) and OAuth consent authenticate access but do NOT select a tenant, so every client that
holds the credential reads/writes the SAME tenant-1 corpus — one shared brain. That is the desired default
for the owner's own machines, but there is no way to give a **team its own isolated corpus**: hand a team a
key and have all their agents (Claude Code AND claude.ai connectors) accumulate memory in their own tenant,
invisible to other teams and to the owner.

The storage layer is already fully multi-tenant: `sentry.TenantID` is a parameter on every journal op
(`Append`/`Scan`/`Record`) and every memory op (`Remember`/`Recall`/`Forget` take `tenant` and filter
`e.tenant != tenant`); the journal isolates records by tenant. The ONLY thing hardcoded is the request →
tenant routing at the server's auth layer. This feature adds that routing — and nothing else.

## Decision

A **credential → tenant registry** consulted by BOTH auth paths. One secret per team works as a static
bearer (Claude Code/agents) AND as the OAuth consent passphrase (claude.ai); both resolve to that team's
tenant. The owner's existing `SENTRY_MCP_TOKEN` is the built-in entry → the `-tenant` default (1).

**Opt-in via a flag, default = today's behavior.** The switch is the presence of `SENTRY_TOKENS_FILE`:
- **absent → STANDALONE** (default): registry holds only the owner entry; behavior is byte-for-byte identical
  to the current server (one token, one tenant, OAuth → tenant 1).
- **present → MULTITENANT**: registry = owner entry + the file's entries; `secret→tenant` resolution activates.

This makes "second server vs flip the flag on the current one" a pure deployment choice (same binary). The
initial rollout is a **second `sentrymcp` instance** (separate port + journal + `tokens.json`) for teams; the
live 8808 personal server is left exactly as-is until the owner chooses to flip it.

## Architecture (all in `cmd/sentrymcp`; storage/memory layers UNCHANGED)

### Component 1 — token registry (`cmd/sentrymcp/tenants.go`, new)

- `tokens.json` (path from `SENTRY_TOKENS_FILE`; chmod 600 on the VM; NOT in git) — a JSON array:
  `[{"secret":"<team-secret>","tenant":2,"label":"team-acme"}, …]`. Tenant ids are explicit integers the
  owner assigns (tenant 1 is reserved for the owner).
- `type Registry` with `Tenant(secret string) (sentry.TenantID, bool)`: returns the mapped tenant for a known
  secret, else `(_, false)`. The owner secret (`SENTRY_MCP_TOKEN`) is always registered → the `-tenant`
  default. A `tokens.json` entry with `tenant == 0` or a duplicate of the owner secret is rejected at load
  (fail fast, log to stderr) so a misconfig can't silently route a team into tenant 1.
- Loaded once at startup. Adding/removing a team = edit the file + restart (dynamic reload is out of scope).

### Component 2 — auth → tenant resolution (`main.go`)

Replace `func (s *server) authorized(r *http.Request) bool` with
`func (s *server) resolveTenant(r *http.Request) (sentry.TenantID, bool)`:
- bearer: `if t, ok := s.reg.Tenant(token); ok { return t, true }` (covers the owner token → 1 and every team
  secret → its tenant).
- OAuth: `if claims, ok := s.oauth.verifyToken(token, "access"); ok { return claims.Tenant, true }` (the
  tenant comes from the JWT — Component 4).
- neither configured (no token, no oauth) → open → `(s.tenant, true)` (the `-tenant` default; preserves the
  local/dev open mode).
- otherwise → `(0, false)` → 401.

`handleHTTP` calls `resolveTenant`; on `!ok` → 401 (unchanged response). On ok it passes the tenant into
dispatch.

### Component 3 — per-request tenant threading (`main.go`)

`callTool(req)` becomes `callTool(req, tenant sentry.TenantID)`; every handler (`record_access`,
`analyze_access`, `remember`, `recall`, `forget`, `stats`) uses the passed `tenant` instead of `s.tenant`.
The stdio transport (no HTTP request) keeps using `s.tenant` (the `-tenant` flag) — stdio is inherently
single-tenant/local. `s.tenant` remains the default tenant used by the owner-token and open-mode paths.

### Component 4 — OAuth multi-tenant (`oauth.go`)

- **Consent**: the authorize/consent step looks up the entered passphrase in the registry → tenant; if
  unknown, consent is rejected. The minted authorization code / access token carries the tenant.
- **JWT**: access tokens gain a `tnt` claim (the tenant id). `verifyToken` returns it (e.g. a small `claims`
  struct with `Tenant`). A token WITHOUT `tnt` (issued before this change) → the `-tenant` default (1), so
  already-issued owner tokens keep working.
- In STANDALONE mode, the only valid consent passphrase is `SENTRY_MCP_TOKEN` → tenant 1 (today's behavior).

## Isolation guarantee (the property the owner asked about)

`memory.Recall(tenant, …)` filters `e.tenant != tenant`; the journal scans filter by tenant. A team only ever
sees its own tenant's memories; the owner only sees tenant 1. No cross-tenant read is possible through the
API. Confirmed by an isolation test below.

## Back-compat guarantee (the owner's corpus is untouched)

- STANDALONE mode (no `SENTRY_TOKENS_FILE`) = identical behavior to the current server — pinned by a test.
- Even in MULTITENANT mode, `SENTRY_MCP_TOKEN` → tenant 1, and tenant-1 records are never moved/rewritten
  (append-only journal). The change is auth/routing only; zero data migration.

## Testing (TDD)

- **Registry** (`tenants_test.go`): a `tokens.json` fixture → `Tenant(teamSecret)` = its tenant;
  `Tenant(ownerSecret)` = default; `Tenant(unknown)` = `(_, false)`; load rejects `tenant:0` and a duplicate
  of the owner secret.
- **resolveTenant**: owner bearer → 1; team bearer → its tenant; OAuth JWT with `tnt` → that tenant; unknown
  bearer → `(0,false)`; open mode (no token/oauth) → default.
- **Isolation (the key test)**: route a `remember` as team-secret → stored under the team tenant; `recall` as
  team-secret returns it; `recall` as owner returns tenant-1 memories and NOT the team's, and vice versa.
- **OAuth**: consent with a team passphrase mints a JWT whose `tnt` = team tenant; `verifyToken` returns it;
  a `tnt`-less token → default tenant.
- **Back-compat**: with no `SENTRY_TOKENS_FILE`, the owner token + OAuth behave exactly as before (single
  tenant); existing tenant-1 recall is unaffected.

## Deployment

Build once. Rollout = a **second `sentrymcp` instance** on the VM (or tesla): its own `-dir`
(separate journal), its own port, `SENTRY_TOKENS_FILE=/root/sentry-tokens.json`, behind its own route/tunnel
path. The current 8808 personal server is **not redeployed** — it stays on its current binary and standalone
behavior until the owner chooses to update it. (When ready, flipping the current server is just: drop in the
new binary + add `SENTRY_TOKENS_FILE` + restart.)

## Out of scope (YAGNI)

- Dynamic reload of `tokens.json` (restart to change the team set).
- A UI/CLI to mint/rotate keys (edit the JSON; rotate = replace the line; revoke = delete it + restart).
- Per-tenant rate limits / quotas.
- Migrating/merging existing tenant-1 memories into team tenants (teams start empty by design).
- Sharing memory ACROSS tenants (isolation is the point).
