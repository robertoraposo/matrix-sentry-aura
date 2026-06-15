# Multi-Tenant by Key Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route each MCP request to a tenant derived from its credential (static bearer OR OAuth consent passphrase) via an opt-in `secret→tenant` registry, so a team's key gives them an isolated corpus — without changing today's single-tenant behavior unless `SENTRY_TOKENS_FILE` is set.

**Architecture:** A `tokenRegistry` maps secrets→tenant ids; the owner's `SENTRY_MCP_TOKEN` is the built-in entry → the `-tenant` default. `resolveTenant(r)` replaces the boolean `authorized(r)` and the per-request tenant is threaded through `dispatch`→`callTool`. OAuth access JWTs gain a `tnt` claim, set from the consent passphrase's tenant. Storage/memory layers are untouched (already tenant-isolated).

**Tech Stack:** Pure Go, zero deps. Spec: `docs/superpowers/specs/2026-06-15-multitenant-by-key-design.md`. Touches only `cmd/sentrymcp`.

---

## File Structure

- **Create** `cmd/sentrymcp/tenants.go` (+ `tenants_test.go`) — the `tokenRegistry` (load `tokens.json`, `Tenant(secret)`).
- **Modify** `cmd/sentrymcp/main.go` — `server.tokens` field; build registry in `main()`; `resolveTenant`; thread tenant through `dispatch`/`callTool`; handlers use the per-request tenant.
- **Modify** `cmd/sentrymcp/oauth.go` (+ `oauth_test.go`) — `jwtClaims.Tnt`; `signToken`/`verifyToken`/`emitTokens`/`authCode` carry tenant; `newOAuth` takes a `tenantFor` resolver; consent maps passphrase→tenant.
- **Deploy (Task 4):** a SECOND `sentrymcp` instance (own journal/port/`tokens.json`); the live 8808 is NOT touched.

NOTE on naming: the `server` already has `reg *sentry.Registry` (path→id). The NEW token registry field is `tokens` — do NOT reuse `reg`.

---

## Task 1: `tokenRegistry` (`cmd/sentrymcp/tenants.go`)

**Files:** Create `cmd/sentrymcp/tenants.go`, `cmd/sentrymcp/tenants_test.go`

- [ ] **Step 1: Write the failing tests** → `cmd/sentrymcp/tenants_test.go`

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryOwnerAndTeams(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "tokens.json")
	os.WriteFile(f, []byte(`[{"secret":"team-acme-sec","tenant":2,"label":"acme"},{"secret":"team-bolt-sec","tenant":3,"label":"bolt"}]`), 0o600)

	reg, err := loadTokenRegistry(f, "owner-sec", 1)
	if err != nil {
		t.Fatal(err)
	}
	if tn, ok := reg.Tenant("owner-sec"); !ok || tn != 1 {
		t.Fatalf("owner → (%d,%v), want (1,true)", tn, ok)
	}
	if tn, ok := reg.Tenant("team-acme-sec"); !ok || tn != 2 {
		t.Fatalf("acme → (%d,%v), want (2,true)", tn, ok)
	}
	if tn, ok := reg.Tenant("team-bolt-sec"); !ok || tn != 3 {
		t.Fatalf("bolt → (%d,%v), want (3,true)", tn, ok)
	}
	if _, ok := reg.Tenant("nope"); ok {
		t.Fatal("unknown secret must be (_, false)")
	}
}

func TestRegistryStandaloneNoFile(t *testing.T) {
	// No tokens file → only the owner entry exists (standalone = today's behavior).
	reg, err := loadTokenRegistry("", "owner-sec", 1)
	if err != nil {
		t.Fatal(err)
	}
	if tn, ok := reg.Tenant("owner-sec"); !ok || tn != 1 {
		t.Fatalf("owner → (%d,%v), want (1,true)", tn, ok)
	}
	if _, ok := reg.Tenant("anything-else"); ok {
		t.Fatal("standalone registry must only know the owner secret")
	}
}

// (tenant ids are sentry.TenantID; loadTokenRegistry takes the owner tenant as
// an untyped constant 1 above, so this test needs no extra import.)

func TestRegistryRejectsBadEntries(t *testing.T) {
	dir := t.TempDir()
	// tenant 0 is invalid (reserved sentinel); duplicate of owner secret is a footgun.
	for _, bad := range []string{
		`[{"secret":"x","tenant":0,"label":"z"}]`,
		`[{"secret":"owner-sec","tenant":9,"label":"dup"}]`,
	} {
		f := filepath.Join(dir, "t.json")
		os.WriteFile(f, []byte(bad), 0o600)
		if _, err := loadTokenRegistry(f, "owner-sec", 1); err == nil {
			t.Fatalf("expected load error for %s", bad)
		}
	}
}
```

- [ ] **Step 2: Run, verify FAIL** — `go test ./cmd/sentrymcp/ -run Registry` → `undefined: loadTokenRegistry`.

- [ ] **Step 3: Implement** → `cmd/sentrymcp/tenants.go`

```go
package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"os"

	"matrixsentry/sentry"
)

// tokenEntry is one credential→tenant mapping (a secret usable as a static
// bearer or an OAuth consent passphrase, and the tenant it resolves to).
type tokenEntry struct {
	Secret string           `json:"secret"`
	Tenant sentry.TenantID  `json:"tenant"`
	Label  string           `json:"label"`
}

// tokenRegistry resolves a credential to its tenant. The owner secret
// (SENTRY_MCP_TOKEN) is always present → the server's default tenant; extra
// teams come from tokens.json. With no file it holds only the owner entry, so
// behavior is identical to the single-tenant server.
type tokenRegistry struct {
	entries []tokenEntry
}

// loadTokenRegistry builds the registry: the owner entry (ownerSecret →
// ownerTenant) plus, if path != "", the JSON array of team entries. It fails
// fast on a zero tenant id or a team secret that duplicates the owner's, so a
// misconfig can never silently route a team into the owner's tenant.
func loadTokenRegistry(path, ownerSecret string, ownerTenant sentry.TenantID) (*tokenRegistry, error) {
	r := &tokenRegistry{}
	if ownerSecret != "" {
		r.entries = append(r.entries, tokenEntry{Secret: ownerSecret, Tenant: ownerTenant, Label: "owner"})
	}
	if path == "" {
		return r, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tokens file: %w", err)
	}
	var teams []tokenEntry
	if err := json.Unmarshal(data, &teams); err != nil {
		return nil, fmt.Errorf("tokens file: parse: %w", err)
	}
	for _, e := range teams {
		if e.Secret == "" || e.Tenant == 0 {
			return nil, fmt.Errorf("tokens file: entry %q has empty secret or tenant 0", e.Label)
		}
		if ownerSecret != "" && e.Secret == ownerSecret {
			return nil, fmt.Errorf("tokens file: entry %q reuses the owner secret", e.Label)
		}
		r.entries = append(r.entries, e)
	}
	return r, nil
}

// Tenant returns the tenant for a secret (constant-time per entry to avoid
// leaking which secret matched via timing), or (_, false) if unknown.
func (r *tokenRegistry) Tenant(secret string) (sentry.TenantID, bool) {
	if secret == "" {
		return 0, false
	}
	for _, e := range r.entries {
		if subtle.ConstantTimeCompare([]byte(secret), []byte(e.Secret)) == 1 {
			return e.Tenant, true
		}
	}
	return 0, false
}
```

- [ ] **Step 4: Run, verify PASS** — `go test ./cmd/sentrymcp/ -run Registry && go vet ./cmd/sentrymcp/`.

- [ ] **Step 5: Commit**

```bash
git add cmd/sentrymcp/tenants.go cmd/sentrymcp/tenants_test.go
git commit -m "feat(sentrymcp): tokenRegistry — secret→tenant map (owner built-in, teams from tokens.json, fail-fast)"
```
End the commit body with:
Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>

---

## Task 2: bearer routing — `resolveTenant` + per-request tenant threading (`main.go`)

**Files:** Modify `cmd/sentrymcp/main.go` (server struct `:58-67`; `main()` wiring; `authorized` `:181-192`; `handleHTTP` `:212-224`; stdio `dispatch` call `:144`; `dispatch` `:248`; `callTool` `:344` + its handler bodies)

- [ ] **Step 1: Write the failing test (isolation at the callTool layer)** → append to `cmd/sentrymcp/main_test.go`

Use the embedder-backed harness already in this file (the one used by the remember/recall tests — read it; it builds a `*server` with `mem` set, referred to here as `newMemServer(t)`; adapt to the real helper name). The test routes two tenants through `callTool` and asserts isolation:

```go
func TestCallToolTenantIsolation(t *testing.T) {
	s := newMemServer(t) // *server with mem + a default tenant (1)
	// remember under tenant 2 via callTool's tenant param
	rememberReq := func(text string) rpcReq {
		args, _ := json.Marshal(map[string]any{"name": "remember", "arguments": map[string]any{"text": text}})
		return rpcReq{ID: json.RawMessage("1"), Method: "tools/call", Params: args}
	}
	recallReq := func(q string) rpcReq {
		args, _ := json.Marshal(map[string]any{"name": "recall", "arguments": map[string]any{"query": q, "k": 5}})
		return rpcReq{ID: json.RawMessage("1"), Method: "tools/call", Params: args}
	}
	textOf := func(r rpcResp) string {
		b, _ := json.Marshal(r.Result)
		return string(b)
	}

	s.callTool(rememberReq("ACME-only secret roadmap fact"), 2)
	s.callTool(rememberReq("owner-only personal note"), 1)

	// tenant 2 recall sees ACME, not owner's
	got2 := textOf(s.callTool(recallReq("roadmap fact"), 2))
	if !strings.Contains(got2, "ACME-only") || strings.Contains(got2, "owner-only") {
		t.Fatalf("tenant 2 recall leaked/missing: %s", got2)
	}
	// tenant 1 recall sees owner's, not ACME
	got1 := textOf(s.callTool(recallReq("roadmap fact"), 1))
	if !strings.Contains(got1, "owner-only") || strings.Contains(got1, "ACME-only") {
		t.Fatalf("tenant 1 recall leaked/missing: %s", got1)
	}
}
```

(If the helper builds a server WITHOUT an embedder, add a tiny `newMemServer` mirroring the existing memory-test setup — a `*server{mem: memory.New(...with a fake embedder...), tenant: 1}`. Reuse whatever the file already has; do not invent a new embedder if `testEmbedder` exists.)

- [ ] **Step 2: Run, verify FAIL** — `go test ./cmd/sentrymcp/ -run TenantIsolation` → fails to compile (`callTool` takes 1 arg).

- [ ] **Step 3: Add the `tokens` field + build the registry in `main()`**

In the `server` struct (`:58-67`) add after `token string`:
```go
	tokens *tokenRegistry // secret→tenant; owner built-in, teams from SENTRY_TOKENS_FILE
```
In `main()`, AFTER the server `s` is constructed and the `-tenant`/`SENTRY_MCP_TOKEN` are known, build the registry (read `SENTRY_TOKENS_FILE`; owner secret = the same `SENTRY_MCP_TOKEN` used for `s.token`; owner tenant = the `-tenant` flag value already in `s.tenant`):
```go
	s.tokens, err = loadTokenRegistry(envOr("SENTRY_TOKENS_FILE", ""), s.token, s.tenant)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sentrymcp: %v\n", err)
		os.Exit(1)
	}
```
(Place it where `err` is in scope; if no `err` is declared yet there, use `var err error` or `:=`. Match the file's existing error-handling style.)

- [ ] **Step 4: Replace `authorized` with `resolveTenant`** (`:181-192`):

```go
// resolveTenant maps an HTTP request to its tenant via the credential: a static
// bearer secret in the registry, or an OAuth access token's tnt claim. Returns
// (_, false) → 401. Open/local mode (no static token and no OAuth) → default.
func (s *server) resolveTenant(r *http.Request) (sentry.TenantID, bool) {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		secret := strings.TrimPrefix(auth, "Bearer ")
		if t, ok := s.tokens.Tenant(secret); ok {
			return t, true
		}
		if s.oauth != nil {
			if cl, ok := s.oauth.verifyToken(secret, "access"); ok {
				if cl.Tnt != 0 {
					return cl.Tnt, true
				}
				return s.tenant, true // tnt-less (legacy) token → default tenant
			}
		}
	}
	if s.token == "" && s.oauth == nil {
		return s.tenant, true // open/local mode (unchanged)
	}
	return 0, false
}
```
(This uses `s.oauth.verifyToken` returning `(jwtClaims, bool)` — Task 3 changes its return type. Until then it won't compile; that's expected and resolved in Task 3. Build the whole module green at the end of Task 3.)

- [ ] **Step 5: Thread the tenant through `handleHTTP` → `dispatch` → `callTool`**

`handleHTTP` (`:212-224`), replace the `if !s.authorized(r) {…}` block + the dispatch call with:
```go
	tenant, ok := s.resolveTenant(r)
	if !ok {
		if s.oauth != nil {
			w.Header().Set("WWW-Authenticate", s.oauth.wwwAuthenticate())
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	resp, ok := s.dispatch(body, tenant)
```
Change `dispatch` signature (`:248`) to `func (s *server) dispatch(line []byte, tenant sentry.TenantID) (rpcResp, bool)` and its `tools/call` case (`:269`) to `return s.callTool(req, tenant), true`.
Change the stdio caller (`:144`) `if resp, ok := s.dispatch(line); ok {` → `if resp, ok := s.dispatch(line, s.tenant); ok {` (stdio is single-tenant: the `-tenant` flag).

- [ ] **Step 6: `callTool` takes the tenant; handlers use it**

Change `func (s *server) callTool(req rpcReq) rpcResp` → `func (s *server) callTool(req rpcReq, tenant sentry.TenantID) rpcResp`. Inside, replace EVERY use of `s.tenant` with the `tenant` parameter (the `record_access`, `analyze_access`, `remember`, `recall`, `forget` handlers and their `moko.Info` logs — grep `s.tenant` within `callTool`). Do NOT change `s.tenant` references outside `callTool` (the stdio startup log etc.).

- [ ] **Step 7: Commit** (module may still be red on oauth until Task 3 — that's expected; run `go test ./cmd/sentrymcp/ -run 'Registry|TenantIsolation'` to confirm these pass once Task 3 lands, but commit the main.go changes now)

Actually: to keep each commit compiling, do Task 3 BEFORE running the module build. Commit main.go + Task 3 oauth.go together if needed. Recommended: implement Task 3 next, THEN run `go build ./... && go test ./...`, THEN commit Tasks 2+3 together:
```bash
git add cmd/sentrymcp/main.go cmd/sentrymcp/main_test.go
git commit -m "feat(sentrymcp): per-request tenant routing — resolveTenant + thread tenant through dispatch/callTool"
```
End the commit body with the Co-Authored-By trailer. (If you commit before Task 3, the package won't build; prefer committing 2+3 together.)

---

## Task 3: OAuth multi-tenant — `tnt` claim + consent→tenant (`oauth.go`)

**Files:** Modify `cmd/sentrymcp/oauth.go` (struct `:35-67`, `newOAuth` `:51`, `jwtClaims` `:99`, `signToken` `:107`, `verifyToken` `:120`, `handleAuthorize` consent `:318-327`, `handleToken` `:370,372,377`, `emitTokens` `:383`); `oauth_test.go`

- [ ] **Step 1: Write the failing test** → `cmd/sentrymcp/oauth_test.go`

```go
package main

import (
	"testing"
	"time"

	"matrixsentry/sentry"
)

func TestSignVerifyCarriesTenant(t *testing.T) {
	o := newOAuth("https://x.example", "owner-sec", func(string) (sentry.TenantID, bool) { return 0, false })
	tok := o.signToken("owner", "access", time.Minute, sentry.TenantID(7))
	cl, ok := o.verifyToken(tok, "access")
	if !ok || cl.Sub != "owner" || cl.Tnt != 7 {
		t.Fatalf("verify = %+v ok=%v, want Sub=owner Tnt=7", cl, ok)
	}
}

func TestVerifyTenantlessTokenIsZero(t *testing.T) {
	// A token signed with tenant 0 (legacy/owner) decodes Tnt==0; the caller maps 0→default.
	o := newOAuth("https://x.example", "owner-sec", func(string) (sentry.TenantID, bool) { return 0, false })
	tok := o.signToken("owner", "access", time.Minute, 0)
	cl, ok := o.verifyToken(tok, "access")
	if !ok || cl.Tnt != 0 {
		t.Fatalf("legacy token Tnt = %d, want 0", cl.Tnt)
	}
}

func TestVerifyRejectsWrongKind(t *testing.T) {
	o := newOAuth("https://x.example", "owner-sec", func(string) (sentry.TenantID, bool) { return 0, false })
	tok := o.signToken("owner", "refresh", time.Minute, 0)
	if _, ok := o.verifyToken(tok, "access"); ok {
		t.Fatal("refresh token must not verify as access")
	}
}
```

- [ ] **Step 2: Run, verify FAIL** — `go test ./cmd/sentrymcp/ -run 'SignVerify|Tenantless|WrongKind'` → `newOAuth`/`signToken`/`verifyToken` arity & return mismatches.

- [ ] **Step 3: Add the tenant resolver to the provider + `tnt` claim**

`oauthProvider` struct (`:43-50`): add a field
```go
	tenantFor func(secret string) (sentry.TenantID, bool) // consent passphrase → tenant
```
`newOAuth` (`:51`): change signature to `func newOAuth(issuer, approveSecret string, tenantFor func(string) (sentry.TenantID, bool)) *oauthProvider` and set `tenantFor: tenantFor` in the returned struct. Update the caller in `main.go` (`s.oauth = newOAuth(*oauthIssuer, secret, s.tokens.Tenant)`).

`jwtClaims` (`:99`): add
```go
	Tnt sentry.TenantID `json:"tnt,omitempty"`
```
`signToken` (`:107`): add a `tenant sentry.TenantID` param and set it:
```go
func (o *oauthProvider) signToken(subject, kind string, ttl time.Duration, tenant sentry.TenantID) string {
	...
	cl := jwtClaims{Sub: subject, Iss: o.issuer, Kind: kind, Iat: now.Unix(), Exp: now.Add(ttl).Unix(), Tnt: tenant}
	...
}
```
`verifyToken` (`:120`): change return to `(jwtClaims, bool)` — on every failure `return jwtClaims{}, false`; on success `return cl, true` (instead of `cl.Sub`).

- [ ] **Step 4: Thread tenant through consent + token issuance**

`authCode` struct (`:35`): add `tenant sentry.TenantID`.
`handleAuthorize` POST consent (`:318-327`): replace the single-secret compare with a registry lookup:
```go
	tnt, ok := o.tenantFor(get("passphrase"))
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<p>Incorrect passphrase. <a href="javascript:history.back()">Try again</a>.</p>`)
		return
	}
	code := o.issueCode(authCode{
		clientID: get("client_id"), redirectURI: redirectURI,
		codeChallenge: challenge, scope: get("scope"), tenant: tnt,
	})
```
`handleToken` (`:370`): authorization_code → `o.emitTokens(w, "owner", c.tenant)`. refresh (`:372,377`):
```go
	cl, ok := o.verifyToken(r.Form.Get("refresh_token"), "refresh")
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	o.emitTokens(w, cl.Sub, cl.Tnt)
```
`emitTokens` (`:383`): add a `tenant sentry.TenantID` param and pass it to both `signToken` calls:
```go
func (o *oauthProvider) emitTokens(w http.ResponseWriter, subject string, tenant sentry.TenantID) {
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  o.signToken(subject, "access", accessTokenTTL, tenant),
		"refresh_token": o.signToken(subject, "refresh", refreshTokenTTL, tenant),
		...
	})
}
```
(`approveSecret`/`signKey` stay as-is — the JWT signing key is still derived from the owner secret. The consent check now goes through `tenantFor`, which in standalone mode only knows the owner secret → tenant 1, identical to the old `approveSecret` compare.)

- [ ] **Step 5: Build + vet + test the WHOLE module (the integration point for Tasks 2+3)**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all green — `tenants_test`, `TestCallToolTenantIsolation`, the oauth tests, and every existing test. Fix any remaining `verifyToken`/`signToken` caller mismatch.

- [ ] **Step 6: Commit** (Tasks 2 + 3 together so every commit builds)

```bash
git add cmd/sentrymcp/main.go cmd/sentrymcp/main_test.go cmd/sentrymcp/oauth.go cmd/sentrymcp/oauth_test.go
git commit -m "feat(sentrymcp): OAuth multi-tenant — tnt JWT claim from consent passphrase; per-request tenant routing"
```
End the commit body with the Co-Authored-By trailer.

---

## Task 4: Deploy to a SECOND server + verify (controller-executed)

The live 8808 personal server is NOT touched. Stand up a second instance in MULTITENANT mode.

- [ ] **Step 1: Full green gate** — `go build ./... && go test ./...` (all green).
- [ ] **Step 2: Back-compat smoke (standalone)** — run the new binary locally with NO `SENTRY_TOKENS_FILE` and confirm a bearer with the owner token still resolves to the `-tenant` default and recall works (or rely on `TestRegistryStandaloneNoFile` + `TestCallToolTenantIsolation` + the existing suite — the standalone path is the registry with only the owner entry, already pinned).
- [ ] **Step 3: Deploy the second instance.** Cross-compile + ship; pick a free port (e.g. 8809) and a SEPARATE journal dir; create `tokens.json` (chmod 600) with one test team:
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/sentrymcp ./cmd/sentrymcp
scp /tmp/sentrymcp matrix-sentry:/root/sentrymcp-mt
ssh matrix-sentry 'mkdir -p /root/sentry-journal-mt && printf "%s\n" "[{\"secret\":\"TEAMTEST-<random>\",\"tenant\":2,\"label\":\"test\"}]" > /root/sentry-tokens.json && chmod 600 /root/sentry-tokens.json && \
  SENTRY_TOKENS_FILE=/root/sentry-tokens.json SENTRY_MCP_TOKEN=<owner-tok> SENTRY_OLLAMA_URL=http://100.93.11.62:11434 SENTRY_EMBED_MODEL=nomic-embed-text-v2-moe \
  setsid /root/sentrymcp-mt -http :8809 -dir /root/sentry-journal-mt -tenant 1 >/root/sentrymcp-mt.log 2>&1 & sleep 2 && curl -s localhost:8809/ && echo'
```
(Use the real owner token + a fresh random team secret. `setsid` so it survives the SSH session — see the deploy gotcha in memory. A systemd unit `sentrymcp-mt` is the durable form; the inline `setsid` is fine to validate first.)
- [ ] **Step 4: Verify isolation + back-compat live** against `:8809` via raw JSON-RPC:
  - owner-token `remember "owner X"` then team-secret `recall "owner X"` → team must NOT see it; owner-token `recall` → sees it. And team-secret `remember "team Y"` → owner `recall "team Y"` must NOT see it. (Two tenants on ONE second-server journal, isolated by the registry routing.)
  - Unknown bearer → 401.
- [ ] **Step 5: Decide durability** — if good, write a `sentrymcp-mt` systemd unit (Restart=always) mirroring the existing `sentrymcp` unit but with the `-http :8809 -dir …-mt` + `SENTRY_TOKENS_FILE` env; expose via a tunnel path if teams need public access. Update `HANDOFF.md` + memory (multi-tenant LIVE on the 2nd instance; 8808 unchanged). Commit.

---

## Notes for the implementer

- **Don't touch the storage/memory layers** — they are already tenant-isolated; this is purely auth+routing.
- **Field name:** the new registry is `s.tokens` (the existing `s.reg` is the path→id Registry — different thing).
- **Commit Tasks 2 and 3 together** — they cross-depend (`verifyToken` return type), so the package only builds once both land.
- **Back-compat is the contract:** no `SENTRY_TOKENS_FILE` ⇒ registry = owner-only ⇒ identical to today. `TestRegistryStandaloneNoFile` pins it; do not weaken it.
- **The 8808 live server is out of scope to redeploy** — Task 4 is a NEW instance. Leaving the owner's corpus untouched is the whole point.
