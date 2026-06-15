# Mistral Embedder + OAuth Teams-Only Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a Mistral API embedder and let native OAuth run without the owner token, so server2 (8809, `matrix.blaze.net.do/mcp`) can serve three isolated teams (BlazeSphere/Kuadre/Round PlayGames) embedding via Mistral.

**Architecture:** `memory.MistralEmbedder` mirrors `memory.OllamaEmbedder` against the OpenAI-style Mistral `/v1/embeddings` API. `cmd/sentrymcp` gains a provider switch (`SENTRY_EMBED_PROVIDER`, default `ollama`) via a unit-testable `resolveEmbedder` helper, and sources the OAuth signing key from `SENTRY_OAUTH_KEY` (falling back to `SENTRY_MCP_TOKEN`). The 8808 server is byte-for-byte unchanged.

**Tech Stack:** Go (stdlib `net/http`, `encoding/json`, `httptest`), the existing `memory.Embedder` interface (`Embed([]string) ([][]float32, error)`, `Dim() int`), the existing `tokenRegistry` + `oauthProvider`.

---

## File Structure

- Create: `memory/mistral.go` — `MistralEmbedder` (sibling of `memory/ollama.go`).
- Create: `memory/mistral_test.go` — embedder unit tests (httptest).
- Modify: `cmd/sentrymcp/main.go` — `resolveEmbedder` helper, provider switch, OAuth signing-key decoupling.
- Create: `cmd/sentrymcp/embed.go` — the `resolveEmbedder` + `oauthSigningKey` helpers (kept out of `main.go` so they're unit-testable without running `main`).
- Modify: `cmd/sentrymcp/main_test.go` (or new `embed_test.go`) — provider-selection + signing-key unit tests.

---

### Task 1: Mistral embedder

**Files:**
- Create: `memory/mistral.go`
- Test: `memory/mistral_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// memory/mistral_test.go
package memory

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMistralEmbedHappyPath(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		io.WriteString(w, `{"data":[{"index":0,"embedding":[1,2,3]},{"index":1,"embedding":[4,5,6]}]}`)
	}))
	defer srv.Close()

	e := NewMistralEmbedder("sk-secret", "mistral-embed", 3)
	e.url = srv.URL // override base URL for the test

	out, err := e.Embed([]string{"alpha", "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sk-secret" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"mistral-embed"`) || !strings.Contains(gotBody, `"alpha"`) {
		t.Fatalf("body = %q", gotBody)
	}
	if len(out) != 2 || out[0][0] != 1 || out[1][2] != 6 {
		t.Fatalf("vectors = %v", out)
	}
}

func TestMistralEmbedCountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"embedding":[1,2,3]}]}`) // asked 2, got 1
	}))
	defer srv.Close()
	e := NewMistralEmbedder("k", "mistral-embed", 3)
	e.url = srv.URL
	if _, err := e.Embed([]string{"a", "b"}); err == nil {
		t.Fatal("expected count-mismatch error")
	}
}

func TestMistralEmbedDimMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"embedding":[1,2]}]}`) // dim 2, want 3
	}))
	defer srv.Close()
	e := NewMistralEmbedder("k", "mistral-embed", 3)
	e.url = srv.URL
	_, err := e.Embed([]string{"a"})
	if err == nil || !strings.Contains(err.Error(), "dim") {
		t.Fatalf("expected dim error, got %v", err)
	}
}

func TestMistralEmbedErrorHidesKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"message":"unauthorized: sk-secret"}`) // body echoes a key-shaped string
	}))
	defer srv.Close()
	e := NewMistralEmbedder("sk-secret", "mistral-embed", 3)
	e.url = srv.URL
	_, err := e.Embed([]string{"a"})
	if err == nil {
		t.Fatal("expected non-200 error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error should include status: %v", err)
	}
	if strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("error must NOT leak the api key: %v", err)
	}
}

func TestMistralDim(t *testing.T) {
	if NewMistralEmbedder("k", "mistral-embed", 1024).Dim() != 1024 {
		t.Fatal("Dim mismatch")
	}
}

var _ = json.Marshal // keep import if unused after edits
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./memory/ -run TestMistral -v`
Expected: FAIL — `undefined: NewMistralEmbedder`.

- [ ] **Step 3: Write the implementation**

```go
// memory/mistral.go
package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// MistralEmbedder embeds text via the Mistral API's OpenAI-style
// POST /v1/embeddings endpoint. The expected dimension is fixed at construction
// (mistral-embed = 1024) so a misconfigured model surfaces immediately rather
// than corrupting recall. The API key is never included in errors or logs.
type MistralEmbedder struct {
	url    string // base URL, no trailing slash (default https://api.mistral.ai)
	model  string
	apiKey string
	dim    int
	client *http.Client
}

// NewMistralEmbedder builds an embedder for the Mistral API. dim is the model's
// output dimension (mistral-embed = 1024).
func NewMistralEmbedder(apiKey, model string, dim int) *MistralEmbedder {
	return &MistralEmbedder{
		url:    "https://api.mistral.ai",
		model:  model,
		apiKey: apiKey,
		dim:    dim,
		client: &http.Client{Timeout: 2 * time.Minute},
	}
}

func (e *MistralEmbedder) Dim() int { return e.dim }

type mistralEmbedReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type mistralEmbedResp struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (e *MistralEmbedder) Embed(texts []string) ([][]float32, error) {
	body, err := json.Marshal(mistralEmbedReq{Model: e.model, Input: texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, e.url+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mistral embed: %w", err) // net errors don't carry the key
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Deliberately do NOT include the response body: it can echo the key.
		return nil, fmt.Errorf("mistral embed: status %d", resp.StatusCode)
	}
	var out mistralEmbedResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("mistral embed decode: %w", err)
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("mistral embed: asked %d, got %d embeddings", len(texts), len(out.Data))
	}
	vecs := make([][]float32, len(out.Data))
	for _, d := range out.Data {
		if len(d.Embedding) != e.dim {
			return nil, fmt.Errorf("mistral embed: vector dim %d, want %d (wrong model?)", len(d.Embedding), e.dim)
		}
		// Mistral returns an explicit index; honor it so order can't drift.
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, fmt.Errorf("mistral embed: index %d out of range", d.Index)
		}
		vecs[d.Index] = d.Embedding
	}
	for i, v := range vecs {
		if v == nil {
			return nil, fmt.Errorf("mistral embed: missing embedding for index %d", i)
		}
	}
	return vecs, nil
}
```

> Note: indexing by `d.Index` makes the count check + the nil sweep jointly guarantee a complete, ordered set. Remove the `var _ = json.Marshal` line from the test if it causes an "unused" issue — it's only a safety net.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./memory/ -run TestMistral -v`
Expected: PASS (all 5).

- [ ] **Step 5: Commit**

```bash
git add memory/mistral.go memory/mistral_test.go
git commit -m "feat(memory): MistralEmbedder — OpenAI-style /v1/embeddings, dim-validated, key never logged"
```

---

### Task 2: Provider selection + helpers (`resolveEmbedder`, `oauthSigningKey`)

**Files:**
- Create: `cmd/sentrymcp/embed.go`
- Test: `cmd/sentrymcp/embed_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// cmd/sentrymcp/embed_test.go
package main

import (
	"testing"

	"matrixsentry/memory"
)

func TestResolveEmbedderOllama(t *testing.T) {
	e, err := resolveEmbedder("ollama", "http://x:11434", "nomic-embed-text", 768, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.(*memory.OllamaEmbedder); !ok {
		t.Fatalf("want *OllamaEmbedder, got %T", e)
	}
}

func TestResolveEmbedderOllamaNoURL(t *testing.T) {
	e, err := resolveEmbedder("ollama", "", "nomic-embed-text", 768, "")
	if err != nil {
		t.Fatal(err)
	}
	if e != nil {
		t.Fatalf("ollama with no URL means memory disabled → nil embedder, got %T", e)
	}
}

func TestResolveEmbedderMistral(t *testing.T) {
	e, err := resolveEmbedder("mistral", "", "mistral-embed", 1024, "sk-key")
	if err != nil {
		t.Fatal(err)
	}
	m, ok := e.(*memory.MistralEmbedder)
	if !ok {
		t.Fatalf("want *MistralEmbedder, got %T", e)
	}
	if m.Dim() != 1024 {
		t.Fatalf("dim = %d, want 1024", m.Dim())
	}
}

func TestResolveEmbedderMistralNoKey(t *testing.T) {
	if _, err := resolveEmbedder("mistral", "", "mistral-embed", 1024, ""); err == nil {
		t.Fatal("mistral without api key must error")
	}
}

func TestResolveEmbedderUnknown(t *testing.T) {
	if _, err := resolveEmbedder("pinecone", "", "m", 768, ""); err == nil {
		t.Fatal("unknown provider must error")
	}
}

func TestOAuthSigningKey(t *testing.T) {
	if got := oauthSigningKey("oauthkey", "ownertoken"); got != "oauthkey" {
		t.Fatalf("SENTRY_OAUTH_KEY should win, got %q", got)
	}
	if got := oauthSigningKey("", "ownertoken"); got != "ownertoken" {
		t.Fatalf("fallback to owner token, got %q", got)
	}
	if got := oauthSigningKey("", ""); got != "" {
		t.Fatalf("both empty → empty, got %q", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/sentrymcp/ -run 'TestResolveEmbedder|TestOAuthSigningKey' -v`
Expected: FAIL — `undefined: resolveEmbedder`, `undefined: oauthSigningKey`.

- [ ] **Step 3: Write the implementation**

```go
// cmd/sentrymcp/embed.go
package main

import (
	"fmt"

	"matrixsentry/memory"
)

// resolveEmbedder builds the embedder for the configured provider, or returns a
// nil embedder (memory disabled) for the ollama provider when no URL is set —
// preserving the long-standing "no -ollama ⇒ memory tools off" behavior. dim is
// the caller-resolved dimension (1024 for mistral, 768 for ollama by default).
func resolveEmbedder(provider, ollamaURL, model string, dim int, mistralKey string) (memory.Embedder, error) {
	switch provider {
	case "", "ollama":
		if ollamaURL == "" {
			return nil, nil // memory disabled; not an error
		}
		return memory.NewOllamaEmbedder(ollamaURL, model, dim), nil
	case "mistral":
		if mistralKey == "" {
			return nil, fmt.Errorf("provider mistral requires SENTRY_MISTRAL_API_KEY")
		}
		return memory.NewMistralEmbedder(mistralKey, model, dim), nil
	default:
		return nil, fmt.Errorf("unknown SENTRY_EMBED_PROVIDER %q (valid: ollama, mistral)", provider)
	}
}

// oauthSigningKey returns the JWT signing/consent secret: SENTRY_OAUTH_KEY when
// set, else the owner token (back-compat for the single-tenant 8808 server),
// else "" (the caller fatals — OAuth needs a signing key).
func oauthSigningKey(oauthKey, ownerToken string) string {
	if oauthKey != "" {
		return oauthKey
	}
	return ownerToken
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./cmd/sentrymcp/ -run 'TestResolveEmbedder|TestOAuthSigningKey' -v`
Expected: PASS (all 7).

- [ ] **Step 5: Commit**

```bash
git add cmd/sentrymcp/embed.go cmd/sentrymcp/embed_test.go
git commit -m "feat(sentrymcp): resolveEmbedder (ollama|mistral) + oauthSigningKey helpers (unit-tested)"
```

---

### Task 3: Wire the helpers into `main.go` (provider switch + OAuth decoupling)

**Files:**
- Modify: `cmd/sentrymcp/main.go:76-142`

- [ ] **Step 1: Add the provider + dim resolution and replace the embedder block**

Add flags near the existing embedder flags (after line 79):

```go
	embedProvider := flag.String("embed-provider", envOr("SENTRY_EMBED_PROVIDER", "ollama"), "embedding provider: ollama | mistral")
```

Replace the embedder block (current lines 119-129) with:

```go
	// Resolve the embedder for the configured provider. dim defaults to 768 for
	// ollama; mistral-embed is 1024, so when the operator left -embed-dim at its
	// ollama default we bump it to 1024 for mistral (override with -embed-dim).
	model := *embedModel
	dim := *embedDim
	if *embedProvider == "mistral" {
		if model == "nomic-embed-text" { // the ollama default — pick the mistral default instead
			model = "mistral-embed"
		}
		if dim == 768 { // the ollama default
			dim = 1024
		}
	}
	emb, err := resolveEmbedder(*embedProvider, *ollamaURL, model, dim, os.Getenv("SENTRY_MISTRAL_API_KEY"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "sentrymcp: %v\n", err)
		os.Exit(1)
	}
	if emb != nil {
		mem, err := memory.New(store, emb)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sentrymcp: init semantic memory: %v\n", err)
			os.Exit(1)
		}
		s.mem = mem
		s.mem.DedupThreshold = float32(*dedupTau)
		moko.Info("semantic memory enabled", map[string]string{"provider": *embedProvider, "model": model, "dim": fmt.Sprint(dim)})
	}
```

- [ ] **Step 2: Replace the OAuth-enable block**

Replace current lines 134-142 with:

```go
	if *oauthIssuer != "" {
		secret := oauthSigningKey(os.Getenv("SENTRY_OAUTH_KEY"), os.Getenv("SENTRY_MCP_TOKEN"))
		if secret == "" {
			fmt.Fprintln(os.Stderr, "sentrymcp: -oauth-issuer requires SENTRY_OAUTH_KEY (or SENTRY_MCP_TOKEN) as the JWT signing key")
			os.Exit(1)
		}
		s.oauth = newOAuth(*oauthIssuer, secret, s.tokens.Tenant)
		moko.Info("native OAuth enabled", map[string]string{"issuer": *oauthIssuer})
	}
```

- [ ] **Step 3: Build and run the full module test suite**

Run: `go build ./... && go test ./... -race`
Expected: PASS — all packages green, including the existing `cmd/sentrymcp` OAuth/memory/multi-tenant tests (back-compat: default provider `ollama` + `SENTRY_MCP_TOKEN` fallback ⇒ unchanged).

- [ ] **Step 4: Verify 8808 back-compat explicitly**

Run: `go test ./cmd/sentrymcp/ -run 'OAuth|Tenant|Memory|Embedder' -v`
Expected: PASS — the standalone/owner-token paths behave exactly as before.

- [ ] **Step 5: Commit**

```bash
git add cmd/sentrymcp/main.go
git commit -m "feat(sentrymcp): provider switch (mistral 1024) + OAuth signing key from SENTRY_OAUTH_KEY (teams-only)"
```

---

### Task 4: Deploy to server2 (8809, `matrix.blaze.net.do/mcp`) — leaves 8808 untouched

**Files:** none (operational). Run from the Mac; the owner sets the Mistral key + Cloudflare route.

- [ ] **Step 1: Build for linux/amd64 and ship**

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/sentrymcp ./cmd/sentrymcp
scp -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes /tmp/sentrymcp root@10.10.10.175:/root/sentrymcp
```

- [ ] **Step 2: Generate `tokens.json` ON server2 (secrets never in chat) and print once**

```bash
ssh -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes root@10.10.10.175 '
B=$(openssl rand -hex 16); K=$(openssl rand -hex 16); R=$(openssl rand -hex 16)
printf "[{\"secret\":\"%s\",\"tenant\":2,\"label\":\"BlazeSphere\"},{\"secret\":\"%s\",\"tenant\":3,\"label\":\"Kuadre\"},{\"secret\":\"%s\",\"tenant\":4,\"label\":\"Round PlayGames\"}]" "$B" "$K" "$R" > /root/sentry-tokens.json
chmod 600 /root/sentry-tokens.json
echo "BlazeSphere=$B"; echo "Kuadre=$K"; echo "Round PlayGames=$R"'
```
Retrieve later with: `ssh matrix-sentry2 'cat /root/sentry-tokens.json'`.

- [ ] **Step 3: Write the env file (owner sets the Mistral key) and an OAuth signing key**

```bash
ssh -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes root@10.10.10.175 '
OK=$(openssl rand -hex 32)
cat > /root/sentrymcp-mt.env <<EOF
SENTRY_TOKENS_FILE=/root/sentry-tokens.json
SENTRY_EMBED_PROVIDER=mistral
SENTRY_EMBED_MODEL=mistral-embed
SENTRY_MISTRAL_API_KEY=REPLACE_WITH_MISTRAL_KEY
SENTRY_OAUTH_KEY=$OK
SENTRY_OAUTH_ISSUER=https://matrix.blaze.net.do
SENTRY_DEDUP_TAU=0.45
EOF
chmod 600 /root/sentrymcp-mt.env
echo "wrote /root/sentrymcp-mt.env (set SENTRY_MISTRAL_API_KEY before start)"'
```
Then the owner edits the file in place to set the real `SENTRY_MISTRAL_API_KEY` (out-of-band; never in chat/git).

- [ ] **Step 4: Install the systemd unit and start**

```bash
ssh -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes root@10.10.10.175 '
cat > /etc/systemd/system/sentrymcp-mt.service <<EOF
[Unit]
Description=Matrix Sentry MCP (multi-tenant, teams)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/root/sentrymcp -http 0.0.0.0:8809 -dir /root/sentry-journal-mt
EnvironmentFile=/root/sentrymcp-mt.env
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable --now sentrymcp-mt && sleep 1 && systemctl --no-pager status sentrymcp-mt | head -8'
```

- [ ] **Step 5: Verify locally on server2 (tenant isolation + tool list)**

```bash
ssh -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes root@10.10.10.175 '
B=$(python3 -c "import json;print(json.load(open(\"/root/sentry-tokens.json\"))[0][\"secret\"])")
K=$(python3 -c "import json;print(json.load(open(\"/root/sentry-tokens.json\"))[1][\"secret\"])")
echo "--- tools/list (BlazeSphere) ---"
curl -s -H "Authorization: Bearer $B" -H "Content-Type: application/json" -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}" http://localhost:8809/mcp | head -c 400; echo
echo "--- remember as BlazeSphere ---"
curl -s -H "Authorization: Bearer $B" -H "Content-Type: application/json" -d "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"remember\",\"arguments\":{\"text\":\"ISOLATION PROBE blazesphere only\"}}}" http://localhost:8809/mcp; echo
echo "--- recall as Kuadre (must NOT see it) ---"
curl -s -H "Authorization: Bearer $K" -H "Content-Type: application/json" -d "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"recall\",\"arguments\":{\"query\":\"ISOLATION PROBE\"}}}" http://localhost:8809/mcp; echo'
```
Expected: tools/list shows 9 tools; the Kuadre recall does NOT return the BlazeSphere probe (isolation holds). This requires the real Mistral key to be set (embeds run live); if the key is a placeholder, expect an embed error — set the key first.

- [ ] **Step 6: Cloudflare tunnel + zone (owner action)**

Route `matrix.blaze.net.do` → `http://localhost:8809` on the server2 tunnel (serves `/mcp` and the OAuth `/.well-known/*`, `/authorize`, `/token`, `/register` at the host root). Disable Bot Fight Mode for the zone (same gotcha as the 8808 zone). Then connect claude.ai with a team passphrase and confirm the consent mints a tenant-scoped token.

- [ ] **Step 7: Update HANDOFF.md and commit**

Add a "MISTRAL EMBEDDER + OAUTH TEAMS-ONLY — server2 LIVE" section (provider switch, OAuth key decoupling, the three tenants, the `cat /root/sentry-tokens.json` retrieval note, the Mistral 1024-dim journal). Per the security rule, infra IPs/topology stay ONLY in HANDOFF.md (local), never in Matrix memories.

```bash
git add HANDOFF.md && git commit -m "docs: server2 LIVE — Mistral embedder + OAuth teams-only (BlazeSphere/Kuadre/Round PlayGames)"
```
