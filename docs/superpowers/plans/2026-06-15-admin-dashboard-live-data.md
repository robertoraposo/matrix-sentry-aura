# Admin Dashboard Live Data (v2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Show the owner's REAL memories in the Vector Galaxy dashboard by adding a `/admin/corpus` read endpoint to sentrymcp and a projection/proxy layer + frontend shim to sentryadmin.

**Architecture:** `memory.Store.List(tenant)` → `sentrymcp GET /admin/corpus` (auth) → `sentryadmin` fetches it server-side, projects vectors to 3D (PCA) + clusters (k-means), serves `GET /api/galaxy` in the exact shape `corpus.js` emits → a `live.js` shim swaps the mock for live data with fallback.

**Tech Stack:** Go (stdlib net/http, encoding/json, httptest, math), the existing `memory.Store`/`resolveTenant`, the embedded dashboard assets.

---

## File Structure

- Modify: `memory/memory.go` (add `List`), `memory/memory_test.go`.
- Modify: `cmd/sentrymcp/main.go` (register `/admin/corpus` + `handleAdminCorpus`), `cmd/sentrymcp/main_test.go`.
- Create: `cmd/sentryadmin/galaxy.go` (pca3, kmeans), `cmd/sentryadmin/galaxy_test.go`.
- Create: `cmd/sentryadmin/api.go` (corpus client + `/api/galaxy`,`/api/comms` handlers + shape builder), `cmd/sentryadmin/api_test.go`.
- Modify: `cmd/sentryadmin/main.go` (wire `/api/*` routes), `cmd/sentryadmin/assets/index.html` (load live.js + await prime), Create: `cmd/sentryadmin/assets/live.js`.

---

### Task 1: `memory.Store.List(tenant)`

**Files:** Modify `memory/memory.go`; Test `memory/memory_test.go`.

- [ ] **Step 1: Write the failing test** (append to `memory/memory_test.go`)

```go
func TestListReturnsTenantEntriesWithVectors(t *testing.T) {
	st := newTestStore(t) // existing helper that builds a memory.Store with a test embedder
	if _, _, _, err := st.Remember(1, "alpha one", RememberOpts{Tags: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.Remember(1, "alpha two", RememberOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.Remember(2, "other tenant", RememberOpts{}); err != nil {
		t.Fatal(err)
	}
	got := st.List(1)
	if len(got) != 2 {
		t.Fatalf("tenant 1 should have 2 memories, got %d", len(got))
	}
	for _, m := range got {
		if len(m.Vector) == 0 {
			t.Fatalf("List must include vectors, id %d had none", m.ID)
		}
		if m.Text == "other tenant" {
			t.Fatal("List leaked another tenant's memory")
		}
	}
}
```

> NOTE: if no `newTestStore` helper exists, check `memory/memory_test.go` / `memory/ollama_test.go` for the existing pattern that constructs a `*Store` with a deterministic test embedder and reuse it. Match the existing test setup exactly.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./memory/ -run TestListReturnsTenant -v`
Expected: FAIL — `st.List undefined`.

- [ ] **Step 3: Implement `List`** (add to `memory/memory.go`, near `Recall`)

```go
// List returns a snapshot copy of all of one tenant's live memories (including
// their vectors), for admin/visualization use. Read-only; other tenants are
// excluded. Order is insertion order; not a recall (no scoring).
func (s *Store) List(tenant sentry.TenantID) []MemoryPayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]MemoryPayload, 0)
	for _, e := range s.entries {
		if e.tenant == tenant {
			out = append(out, e.mem)
		}
	}
	return out
}
```

> Check the mutex field name in `type Store struct` (likely `mu sync.Mutex` or `sync.RWMutex`). If it's an `RWMutex`, use `s.mu.RLock()/RUnlock()`. Match what `Recall` uses.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./memory/ -run TestListReturnsTenant -v` → PASS. Then `go test ./memory/ -race`.

- [ ] **Step 5: Commit**

```bash
git add memory/memory.go memory/memory_test.go
git commit -m "feat(memory): Store.List(tenant) — snapshot a tenant's memories+vectors for admin views"
```

---

### Task 2: `GET /admin/corpus` on sentrymcp

**Files:** Modify `cmd/sentrymcp/main.go`; Test `cmd/sentrymcp/main_test.go`.

- [ ] **Step 1: Write the failing test** (append to `cmd/sentrymcp/main_test.go`)

```go
func TestAdminCorpusReturnsTenantMemories(t *testing.T) {
	s := newMemServer(t) // existing helper: server with a test embedder + memory store
	// seed two memories on the default tenant
	s.mem.Remember(s.tenant, "deploy on fridays", memory.RememberOpts{Tags: []string{"deploy"}})
	s.mem.Remember(s.tenant, "indentation style", memory.RememberOpts{})

	req := httptest.NewRequest(http.MethodGet, "/admin/corpus", nil)
	rec := httptest.NewRecorder()
	s.handleAdminCorpus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Tenant   int `json:"tenant"`
		Count    int `json:"count"`
		Memories []struct {
			ID  uint64    `json:"id"`
			Vec []float32 `json:"vec"`
		} `json:"memories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 2 || len(out.Memories) != 2 {
		t.Fatalf("want 2 memories, got %d", out.Count)
	}
	if len(out.Memories[0].Vec) == 0 {
		t.Fatal("memories must include vectors")
	}
}

func TestAdminCorpusRequiresAuthWhenConfigured(t *testing.T) {
	s := newMemServer(t)
	s.token = "owner-secret" // simulate a configured bearer
	s.tokens, _ = loadTokenRegistry("", "owner-secret", s.tenant)
	req := httptest.NewRequest(http.MethodGet, "/admin/corpus", nil) // no Authorization
	rec := httptest.NewRecorder()
	s.handleAdminCorpus(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer must be 401, got %d", rec.Code)
	}
}
```

> Confirm `newMemServer` exists in main_test.go (it does — it builds `s.mem` with `testEmbedder`). Confirm the `memory` import is present in the test file; add it if needed. Confirm `resolveTenant`'s open-mode rule: with no token/oauth/registry, `newMemServer`'s server is open → the first test (no auth configured) returns the default tenant and 200. The second test configures a token so auth is required.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/sentrymcp/ -run TestAdminCorpus -v`
Expected: FAIL — `s.handleAdminCorpus undefined`.

- [ ] **Step 3: Implement the handler + route**

Add the handler (new code in `main.go`, near `handleHTTP`):

```go
// handleAdminCorpus serves a tenant's full memory corpus (with vectors) for the
// admin dashboard's galaxy view. Auth + tenant come from resolveTenant (same as
// the MCP). Server-to-server only (the admin proxy calls it); no CORS.
func (s *server) handleAdminCorpus(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.resolveTenant(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.mem == nil {
		http.Error(w, "semantic memory disabled (no embedder)", http.StatusServiceUnavailable)
		return
	}
	mems := s.mem.List(tenant)
	type item struct {
		ID   uint64    `json:"id"`
		Text string    `json:"text"`
		Tags []string  `json:"tags,omitempty"`
		Src  string    `json:"src,omitempty"`
		Vec  []float32 `json:"vec"`
	}
	out := struct {
		Tenant   int    `json:"tenant"`
		Dim      int    `json:"dim"`
		Count    int    `json:"count"`
		Memories []item `json:"memories"`
	}{Tenant: int(tenant), Count: len(mems), Memories: make([]item, 0, len(mems))}
	for _, m := range mems {
		out.Dim = len(m.Vector)
		out.Memories = append(out.Memories, item{ID: m.ID, Text: m.Text, Tags: m.Tags, Src: m.Source, Vec: m.Vector})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
```

Register the route in `serveHTTP`, BEFORE `mux.HandleFunc("/", s.handleHTTP)`:

```go
	mux.HandleFunc("/admin/corpus", s.handleAdminCorpus)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/sentrymcp/ -run TestAdminCorpus -v` → PASS. Then `go build ./... && go test ./... -race`.

- [ ] **Step 5: Commit**

```bash
git add cmd/sentrymcp/main.go cmd/sentrymcp/main_test.go
git commit -m "feat(sentrymcp): GET /admin/corpus — tenant-scoped memory dump (vectors) for the dashboard"
```

---

### Task 3: PCA + k-means projection (`cmd/sentryadmin/galaxy.go`)

**Files:** Create `cmd/sentryadmin/galaxy.go`, `cmd/sentryadmin/galaxy_test.go`.

- [ ] **Step 1: Write the failing tests**

```go
// cmd/sentryadmin/galaxy_test.go
package main

import (
	"math"
	"testing"
)

func TestPCA3OrdersByVariance(t *testing.T) {
	// Anisotropic data: huge spread on dim 0, tiny on dims 1..4.
	vecs := make([][]float32, 200)
	for i := range vecs {
		v := make([]float32, 5)
		t01 := float32(i) / 200.0
		v[0] = (t01 - 0.5) * 100 // dominant axis
		v[1] = float32(math.Sin(float64(i))) * 0.1
		vecs[i] = v
	}
	pos := pca3(vecs)
	if len(pos) != 200 {
		t.Fatalf("want 200 projected points, got %d", len(pos))
	}
	// Component 0 must carry far more variance than component 1.
	var v0, v1 float64
	for _, p := range pos {
		v0 += float64(p[0]) * float64(p[0])
		v1 += float64(p[1]) * float64(p[1])
	}
	if !(v0 > v1*5) {
		t.Fatalf("PC0 variance (%.3f) should dominate PC1 (%.3f)", v0, v1)
	}
	for _, p := range pos {
		for _, c := range p {
			if math.IsNaN(float64(c)) || math.IsInf(float64(c), 0) {
				t.Fatal("projection produced non-finite coordinate")
			}
		}
	}
}

func TestKMeansSeparatesBlobs(t *testing.T) {
	var pts [][3]float32
	truth := []int{}
	centers := [][3]float32{{0, 0, 0}, {50, 0, 0}, {0, 50, 50}}
	for c, ctr := range centers {
		for j := 0; j < 30; j++ {
			off := float32(j%5) * 0.2
			pts = append(pts, [3]float32{ctr[0] + off, ctr[1] + off, ctr[2] + off})
			truth = append(truth, c)
		}
	}
	assign, cs := kmeans(pts, 3)
	if len(cs) != 3 || len(assign) != len(pts) {
		t.Fatalf("bad shapes: %d centers, %d assigns", len(cs), len(assign))
	}
	// Points sharing a truth label must land in the same cluster.
	for c := 0; c < 3; c++ {
		first := -1
		for i := range pts {
			if truth[i] != c {
				continue
			}
			if first == -1 {
				first = assign[i]
			} else if assign[i] != first {
				t.Fatalf("blob %d split across clusters", c)
			}
		}
	}
	// Determinism.
	assign2, _ := kmeans(pts, 3)
	for i := range assign {
		if assign[i] != assign2[i] {
			t.Fatal("kmeans not deterministic")
		}
	}
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./cmd/sentryadmin/ -run 'TestPCA3|TestKMeans' -v` → FAIL (undefined).

- [ ] **Step 3: Implement `cmd/sentryadmin/galaxy.go`**

```go
package main

import "math"

// pca3 projects d-dimensional vectors to 3D via the top-3 principal components,
// found by power iteration on the implicit covariance action C·v = Xᵀ(X·v),
// orthogonalizing each candidate against the already-found components. Output is
// std-normalized per axis and scaled (~±15) to match the dashboard's visual
// scale. Deterministic: fixed init vectors. Returns one [3]float32 per input.
func pca3(vecs [][]float32) [][3]float32 {
	n := len(vecs)
	out := make([][3]float32, n)
	if n == 0 {
		return out
	}
	d := len(vecs[0])
	if d == 0 {
		return out
	}
	// Mean-center into a float64 matrix.
	mean := make([]float64, d)
	X := make([][]float64, n)
	for i, v := range vecs {
		X[i] = make([]float64, d)
		for j := 0; j < d && j < len(v); j++ {
			X[i][j] = float64(v[j])
			mean[j] += X[i][j]
		}
	}
	for j := range mean {
		mean[j] /= float64(n)
	}
	for i := range X {
		for j := range X[i] {
			X[i][j] -= mean[j]
		}
	}

	comps := make([][]float64, 0, 3)
	for c := 0; c < 3; c++ {
		v := make([]float64, d)
		for j := range v { // deterministic, distinct seed per component
			v[j] = math.Sin(float64((j+1)*(c+1))) + 0.1
		}
		normalize(v)
		for iter := 0; iter < 40; iter++ {
			// w = Xᵀ(X v)
			Xv := make([]float64, n)
			for i := 0; i < n; i++ {
				s := 0.0
				for j := 0; j < d; j++ {
					s += X[i][j] * v[j]
				}
				Xv[i] = s
			}
			w := make([]float64, d)
			for i := 0; i < n; i++ {
				xv := Xv[i]
				for j := 0; j < d; j++ {
					w[j] += X[i][j] * xv
				}
			}
			for _, pc := range comps { // deflate: orthogonalize against found comps
				dot := 0.0
				for j := 0; j < d; j++ {
					dot += w[j] * pc[j]
				}
				for j := 0; j < d; j++ {
					w[j] -= dot * pc[j]
				}
			}
			if normalize(w) == 0 {
				break
			}
			v = w
		}
		comps = append(comps, v)
	}

	// Project, then std-normalize per axis and scale.
	var mom [3]float64
	for i := 0; i < n; i++ {
		for c := 0; c < 3 && c < len(comps); c++ {
			s := 0.0
			for j := 0; j < d; j++ {
				s += X[i][j] * comps[c][j]
			}
			out[i][c] = float32(s)
			mom[c] += s * s
		}
	}
	const scale = 9.0
	for c := 0; c < 3; c++ {
		std := math.Sqrt(mom[c] / float64(n))
		if std < 1e-9 {
			std = 1
		}
		for i := 0; i < n; i++ {
			out[i][c] = float32(float64(out[i][c]) / std * scale)
		}
	}
	return out
}

func normalize(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x * x
	}
	n := math.Sqrt(s)
	if n < 1e-12 {
		return 0
	}
	for i := range v {
		v[i] /= n
	}
	return n
}

// kmeans clusters 3D points into k groups: deterministic farthest-point init +
// Lloyd iterations. Returns each point's cluster index and the cluster centers.
func kmeans(pts [][3]float32, k int) (assign []int, centers [][3]float32) {
	n := len(pts)
	assign = make([]int, n)
	if n == 0 || k <= 0 {
		return assign, nil
	}
	if k > n {
		k = n
	}
	// Farthest-point init: start at point 0, then repeatedly add the point
	// farthest from the current center set. Deterministic.
	centers = make([][3]float32, 0, k)
	centers = append(centers, pts[0])
	for len(centers) < k {
		best, bestD := -1, -1.0
		for i := 0; i < n; i++ {
			dmin := math.MaxFloat64
			for _, c := range centers {
				if dd := dist2(pts[i], c); dd < dmin {
					dmin = dd
				}
			}
			if dmin > bestD {
				bestD, best = dmin, i
			}
		}
		centers = append(centers, pts[best])
	}
	for iter := 0; iter < 25; iter++ {
		changed := false
		for i := 0; i < n; i++ {
			best, bestD := 0, math.MaxFloat64
			for c := range centers {
				if dd := dist2(pts[i], centers[c]); dd < bestD {
					bestD, best = dd, c
				}
			}
			if assign[i] != best {
				assign[i] = best
				changed = true
			}
		}
		sums := make([][3]float64, len(centers))
		cnts := make([]int, len(centers))
		for i := 0; i < n; i++ {
			a := assign[i]
			sums[a][0] += float64(pts[i][0])
			sums[a][1] += float64(pts[i][1])
			sums[a][2] += float64(pts[i][2])
			cnts[a]++
		}
		for c := range centers {
			if cnts[c] > 0 {
				centers[c] = [3]float32{
					float32(sums[c][0] / float64(cnts[c])),
					float32(sums[c][1] / float64(cnts[c])),
					float32(sums[c][2] / float64(cnts[c])),
				}
			}
		}
		if !changed && iter > 0 {
			break
		}
	}
	return assign, centers
}

func dist2(a, b [3]float32) float64 {
	dx := float64(a[0] - b[0])
	dy := float64(a[1] - b[1])
	dz := float64(a[2] - b[2])
	return dx*dx + dy*dy + dz*dz
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/sentryadmin/ -run 'TestPCA3|TestKMeans' -v` → PASS. `go vet ./cmd/sentryadmin/`.

- [ ] **Step 5: Commit**

```bash
git add cmd/sentryadmin/galaxy.go cmd/sentryadmin/galaxy_test.go
git commit -m "feat(sentryadmin): deterministic PCA(→3D) + k-means for the live galaxy projection"
```

---

### Task 4: corpus client + `/api/galaxy` (`cmd/sentryadmin/api.go`)

**Files:** Create `cmd/sentryadmin/api.go`, `cmd/sentryadmin/api_test.go`; Modify `cmd/sentryadmin/main.go`.

- [ ] **Step 1: Write the failing test**

```go
// cmd/sentryadmin/api_test.go
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func stubMCP(t *testing.T, count int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/corpus" {
			http.NotFound(w, r)
			return
		}
		type item struct {
			ID   uint64    `json:"id"`
			Text string    `json:"text"`
			Tags []string  `json:"tags"`
			Vec  []float32 `json:"vec"`
		}
		out := struct {
			Tenant, Dim, Count int
			Memories           []item
		}{Tenant: 1, Dim: 4, Count: count}
		for i := 0; i < count; i++ {
			out.Memories = append(out.Memories, item{
				ID: uint64(i + 1), Text: "memory " + strings.Repeat("x", i%3),
				Tags: []string{[]string{"deploy", "auth", "infra"}[i%3]},
				Vec:  []float32{float32(i), float32(i % 4), float32(i % 7), 1},
			})
		}
		json.NewEncoder(w).Encode(out)
	}))
}

func TestAPIGalaxyShape(t *testing.T) {
	mcp := stubMCP(t, 40)
	defer mcp.Close()
	srv := &apiServer{mcpURL: mcp.URL, token: "x", client: http.DefaultClient}

	rec := httptest.NewRecorder()
	srv.handleGalaxy(rec, httptest.NewRequest(http.MethodGet, "/api/galaxy?tenant=personal", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var d struct {
		Clusters []struct {
			Label  string     `json:"label"`
			Center [3]float32 `json:"center"`
			Count  int        `json:"count"`
		} `json:"clusters"`
		Points []struct {
			Pos          [3]float32 `json:"pos"`
			Text         string     `json:"text"`
			ClusterLabel string     `json:"clusterLabel"`
			Color        string     `json:"color"`
		} `json:"points"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Points) != 40 {
		t.Fatalf("want 40 points, got %d", len(d.Points))
	}
	if len(d.Clusters) == 0 {
		t.Fatal("want clusters")
	}
	if d.Points[0].ClusterLabel == "" || d.Points[0].Color == "" {
		t.Fatal("points must carry clusterLabel + color")
	}
}

func TestAPIGalaxyUpstreamErrorIs502(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	srv := &apiServer{mcpURL: bad.URL, token: "x", client: http.DefaultClient}
	rec := httptest.NewRecorder()
	srv.handleGalaxy(rec, httptest.NewRequest(http.MethodGet, "/api/galaxy?tenant=personal", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("upstream 500 → 502, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./cmd/sentryadmin/ -run TestAPIGalaxy -v` → FAIL (undefined `apiServer`).

- [ ] **Step 3: Implement `cmd/sentryadmin/api.go`**

```go
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"
)

// apiServer fetches a tenant's corpus from the MCP (server-side, with the
// bearer) and serves it projected for the dashboard's galaxy.
type apiServer struct {
	mcpURL string
	token  string
	client *http.Client
}

func newAPIServer(mcpURL, token string) *apiServer {
	return &apiServer{mcpURL: mcpURL, token: token, client: &http.Client{Timeout: 30 * time.Second}}
}

type corpusItem struct {
	ID   uint64    `json:"id"`
	Text string    `json:"text"`
	Tags []string  `json:"tags"`
	Src  string    `json:"src"`
	Vec  []float32 `json:"vec"`
}
type corpusResp struct {
	Tenant   int          `json:"tenant"`
	Dim      int          `json:"dim"`
	Count    int          `json:"count"`
	Memories []corpusItem `json:"memories"`
}

// palette mirrors the dashboard's cluster hues (cyan/magenta/amber/blue/violet/green/rose).
var palette = []string{"#35E6FF", "#FF3DCB", "#FFB23E", "#5B8CFF", "#9D7BFF", "#9DEE4E", "#FF6B8B"}

var tenantMeta = map[string]struct{ Key, Name, Glyph, Accent string }{
	"personal":    {"personal", "Personal", "✦", "#FFB23E"},
	"blazesphere": {"blazesphere", "BlazeSphere", "◆", "#35E6FF"},
	"kuadre":      {"kuadre", "Kuadre", "▲", "#FF3DCB"},
	"roundplay":   {"roundplay", "Round PlayGames", "●", "#9DEE4E"},
}

func (a *apiServer) fetchCorpus() (*corpusResp, error) {
	req, _ := http.NewRequest(http.MethodGet, a.mcpURL+"/admin/corpus", nil)
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mcp /admin/corpus: status %d", resp.StatusCode)
	}
	var cr corpusResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, err
	}
	return &cr, nil
}

func (a *apiServer) handleGalaxy(w http.ResponseWriter, r *http.Request) {
	tk := r.URL.Query().Get("tenant")
	if tk == "" {
		tk = "personal"
	}
	cr, err := a.fetchCorpus()
	if err != nil {
		http.Error(w, `{"error":"upstream"}`, http.StatusBadGateway)
		return
	}
	out := buildGalaxy(tk, cr)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// handleComms returns empty live comms for now (real wiring is a follow-up), so
// the frontend's comms view degrades to empty instead of erroring.
func (a *apiServer) handleComms(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"areas":[],"messages":[]}`))
}

type galaxyPoint struct {
	ID           string     `json:"id"`
	Tenant       string     `json:"tenant"`
	Cluster      int        `json:"cluster"`
	ClusterKey   string     `json:"clusterKey"`
	ClusterLabel string     `json:"clusterLabel"`
	Color        string     `json:"color"`
	Pos          [3]float32 `json:"pos"`
	Text         string     `json:"text"`
	Tags         []string   `json:"tags"`
	Source       string     `json:"source"`
	Access       int        `json:"access"`
	Heat         float64    `json:"heat"`
	CreatedAt    int64      `json:"createdAt"`
	Dim          int        `json:"dim"`
}
type galaxyCluster struct {
	Key    string     `json:"key"`
	Label  string     `json:"label"`
	Color  string     `json:"color"`
	Center [3]float32 `json:"center"`
	Count  int        `json:"count"`
}
type galaxyData struct {
	Tenant   interface{}     `json:"tenant"`
	Clusters []galaxyCluster `json:"clusters"`
	Points   []galaxyPoint   `json:"points"`
}

func buildGalaxy(tenantKey string, cr *corpusResp) galaxyData {
	n := len(cr.Memories)
	vecs := make([][]float32, n)
	for i, m := range cr.Memories {
		vecs[i] = m.Vec
	}
	pos := pca3(vecs)
	k := n / 12
	if k < 2 {
		k = 2
	}
	if k > 6 {
		k = 6
	}
	if k > n && n > 0 {
		k = n
	}
	assign, centers := kmeans(pos, k)

	// Cluster labels = most-common first-tag in the cluster.
	labelOf := make([]string, len(centers))
	for c := range centers {
		freq := map[string]int{}
		for i := range cr.Memories {
			if assign[i] == c && len(cr.Memories[i].Tags) > 0 {
				freq[cr.Memories[i].Tags[0]]++
			}
		}
		best, bestN := "", 0
		for tag, f := range freq {
			if f > bestN || (f == bestN && tag < best) {
				best, bestN = tag, f
			}
		}
		if best == "" {
			best = fmt.Sprintf("grupo %d", c+1)
		}
		labelOf[c] = best
	}

	tm, ok := tenantMeta[tenantKey]
	if !ok {
		tm = struct{ Key, Name, Glyph, Accent string }{tenantKey, tenantKey, "✦", "#35E6FF"}
	}

	clusters := make([]galaxyCluster, len(centers))
	for c := range centers {
		clusters[c] = galaxyCluster{
			Key: fmt.Sprintf("c%d", c), Label: labelOf[c],
			Color: palette[c%len(palette)], Center: centers[c],
		}
	}
	now := time.Now().UnixMilli()
	pts := make([]galaxyPoint, n)
	for i, m := range cr.Memories {
		c := 0
		if i < len(assign) {
			c = assign[i]
		}
		heat := 0.0
		if n > 1 {
			heat = float64(i) / float64(n-1) // id-rank proxy: newer (later) = hotter
		}
		pts[i] = galaxyPoint{
			ID: fmt.Sprintf("m%d", m.ID), Tenant: tm.Key, Cluster: c,
			ClusterKey: clusters[c].Key, ClusterLabel: labelOf[c], Color: palette[c%len(palette)],
			Pos: pos[i], Text: m.Text, Tags: m.Tags, Source: m.Src,
			Access: int(heat*1000) + 1, Heat: heat, CreatedAt: now, Dim: cr.Dim,
		}
		clusters[c].Count++
	}
	// Stable cluster order for display.
	sort.SliceStable(clusters, func(i, j int) bool { return clusters[i].Key < clusters[j].Key })
	return galaxyData{
		Tenant:   map[string]string{"key": tm.Key, "name": tm.Name, "glyph": tm.Glyph, "accent": tm.Accent},
		Clusters: clusters, Points: pts,
	}
}

var _ = url.Values{} // (keep net/url import if unused after edits; remove if vet complains)
```

> If `go vet` flags the `net/url` import as unused, delete both the import and the trailing `var _ = url.Values{}` line.

- [ ] **Step 4: Wire routes in `cmd/sentryadmin/main.go`**

After the existing `mux` is created and before `mux.Handle("/", ...)`, add:

```go
	mcpURL := os.Getenv("SENTRY_ADMIN_MCP_URL")
	mcpToken := os.Getenv("SENTRY_ADMIN_MCP_TOKEN")
	if mcpURL != "" {
		api := newAPIServer(mcpURL, mcpToken)
		mux.HandleFunc("/api/galaxy", api.handleGalaxy)
		mux.HandleFunc("/api/comms", api.handleComms)
		fmt.Fprintf(os.Stderr, "sentryadmin: live data ON (mcp %s)\n", mcpURL)
	} else {
		fmt.Fprintln(os.Stderr, "sentryadmin: live data OFF (set SENTRY_ADMIN_MCP_URL) — serving mock")
	}
```

- [ ] **Step 5: Run tests + build**

Run: `go test ./cmd/sentryadmin/ -v` → all PASS. `go build ./... && go test ./... -race`.

- [ ] **Step 6: Commit**

```bash
git add cmd/sentryadmin/api.go cmd/sentryadmin/api_test.go cmd/sentryadmin/main.go
git commit -m "feat(sentryadmin): /api/galaxy — fetch corpus, project+cluster, emit the dashboard data shape"
```

---

### Task 5: Frontend shim (`live.js` + index.html edits)

**Files:** Create `cmd/sentryadmin/assets/live.js`; Modify `cmd/sentryadmin/assets/index.html`.

- [ ] **Step 1: Create `cmd/sentryadmin/assets/live.js`**

```javascript
/* Live-data shim: replaces window.MatrixCorpus.generate/comms with backend data
   when /api/galaxy is reachable, falling back to the original mock otherwise. */
(function () {
  const cache = {};            // tenantKey -> galaxy data
  const commsCache = {};
  window.MatrixLive = {
    async prime(tenantKey) {
      try {
        const r = await fetch("/api/galaxy?tenant=" + encodeURIComponent(tenantKey));
        if (!r.ok) throw new Error("galaxy " + r.status);
        cache[tenantKey] = await r.json();
        try {
          const rc = await fetch("/api/comms?tenant=" + encodeURIComponent(tenantKey));
          if (rc.ok) commsCache[tenantKey] = await rc.json();
        } catch (e) { /* comms optional */ }
        if (!this._patched) this._patch();
        this._patched = true;
        console.info("[live] primed", tenantKey, "points:", (cache[tenantKey].points || []).length);
      } catch (e) {
        console.warn("[live] prime failed for", tenantKey, "— using mock:", e.message);
      }
    },
    _patch() {
      const C = window.MatrixCorpus;
      if (!C) return;
      const mockGen = C.generate.bind(C);
      const mockComms = C.comms ? C.comms.bind(C) : null;
      C.generate = (tenantKey, count) => cache[tenantKey] || mockGen(tenantKey, count);
      if (mockComms) {
        C.comms = (tenantKey) => {
          const live = commsCache[tenantKey];
          if (live && Array.isArray(live.messages)) return live; // shape-compatible
          return mockComms(tenantKey);
        };
      }
    },
  };
})();
```

> The `/api/comms` shape here is a stub (`{areas:[],messages:[]}`); the mock `comms()` returns a richer shape. The guard `Array.isArray(live.messages)` means an empty live comms still returns `{areas:[],messages:[]}` (empty kanban), which is acceptable and non-breaking. Real comms is a follow-up.

- [ ] **Step 2: Edit `index.html` — load live.js**

In `_boot`'s `urls` array, add `"live.js"` immediately after `"corpus.js"` (so it loads before galaxy.js; it only defines `window.MatrixLive` and does not touch the DOM):

```javascript
      "corpus.js",
      "live.js",
      "galaxy.js",
```

- [ ] **Step 3: Edit `index.html` — await prime before the first generate**

Find the line in `_boot` (after scripts load):
```javascript
    this.corpusData = window.MatrixCorpus.generate(this.state.tenant, 1800);
```
Insert immediately BEFORE it:
```javascript
    await window.MatrixLive.prime(this.state.tenant);
```
(`_boot` is already `async`.)

- [ ] **Step 4: Edit `index.html` — await prime on tenant switch**

In `_switchTenant(key)` (the method that sets a new tenant), find its `generate(...)` call(s), e.g.:
```javascript
    this.corpusData = window.MatrixCorpus.generate(key, 1800);
```
Make `_switchTenant` async if it isn't, and insert before that call:
```javascript
    await window.MatrixLive.prime(key);
```
Do the same for the split-view branch if it calls `generate` for a second tenant (`generate(key, 1400)` → `await window.MatrixLive.prime(key)` first). If making `_switchTenant` async risks an onClick signature issue, instead wrap: call `window.MatrixLive.prime(key).then(() => { /* existing body */ })`. Choose whichever keeps the existing behavior; verify the dashboard still switches tenants.

- [ ] **Step 5: Build + verify locally with the mock still working (live OFF)**

Run: `go build -o /tmp/sentryadmin ./cmd/sentryadmin` then run it WITHOUT `SENTRY_ADMIN_MCP_URL` and confirm with Playwright (navigate, wait 5s, screenshot) that the galaxy still renders (mock fallback path: prime fails → mock). Console should log `[live] prime failed ... using mock`.

- [ ] **Step 6: Commit**

```bash
git add cmd/sentryadmin/assets/live.js cmd/sentryadmin/assets/index.html
git commit -m "feat(sentryadmin): live.js shim — swap mock corpus for /api/galaxy, mock fallback"
```

---

### Task 6: Deploy + verify real data

**Files:** none (operational).

- [ ] **Step 1: Rebuild + redeploy sentrymcp to 8808 and 8809**

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/sentrymcp ./cmd/sentrymcp
scp -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes /tmp/sentrymcp root@10.10.10.96:/root/sentrymcp.new
ssh matrix-sentry 'mv /root/sentrymcp /root/sentrymcp.old && mv /root/sentrymcp.new /root/sentrymcp && chmod +x /root/sentrymcp && systemctl restart sentrymcp && sleep 1 && systemctl is-active sentrymcp'
# teams server (same binary, on server2):
scp -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes /tmp/sentrymcp root@10.10.10.175:/root/sentrymcp.new
ssh matrix-sentry2 'mv /root/sentrymcp /root/sentrymcp.old && mv /root/sentrymcp.new /root/sentrymcp && chmod +x /root/sentrymcp && systemctl restart sentrymcp-mt && sleep 1 && systemctl is-active sentrymcp-mt'
```

- [ ] **Step 2: Configure sentryadmin live env on server2 (token never in chat)**

```bash
ssh matrix-sentry 'grep SENTRY_MCP_TOKEN /root/sentrymcp.env'   # the owner reads the personal token here
# Owner appends to server2's /root/sentryadmin.env, out-of-band:
#   SENTRY_ADMIN_MCP_URL=http://10.10.10.96:8808
#   SENTRY_ADMIN_MCP_TOKEN=<that personal token>
```
The controller should NOT print the token into the chat; do this as an instruction to the owner, or run a server-to-server copy that does not echo the secret (read it on server1 and write it on server2 within a single piped ssh, masking stdout).

- [ ] **Step 3: Rebuild + redeploy sentryadmin**

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/sentryadmin ./cmd/sentryadmin
scp -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes /tmp/sentryadmin root@10.10.10.175:/root/sentryadmin
ssh matrix-sentry2 'chmod +x /root/sentryadmin && systemctl restart sentryadmin && sleep 1 && systemctl is-active sentryadmin && curl -s -o /dev/null -w "galaxy api: %{http_code}\n" http://localhost:8810/api/galaxy?tenant=personal'
```

- [ ] **Step 4: Verify real data renders**

Over the SSH tunnel (`ssh -L 8810:localhost:8810 matrix-sentry2`), load `http://localhost:8810/` with Playwright (basic-auth creds), wait, screenshot. Confirm: the galaxy shows real cluster labels matching the owner's real tags; the metric memory count equals the personal `stats` count; clicking a node shows real memory text. Spot-check that a known recent memory (e.g. the admin-dashboard status) appears via the recall box.

- [ ] **Step 5: Update HANDOFF.md + commit**

Add an "ADMIN DASHBOARD — LIVE DATA (v2)" section: the `/admin/corpus` endpoint, the sentryadmin projection/proxy, the env (`SENTRY_ADMIN_MCP_URL`/`SENTRY_ADMIN_MCP_TOKEN`, token only on server2), the redeploy of 8808+8809, and the remaining follow-ups (real access heat, journal, comms, multi-tenant selector). Keep infra specifics in HANDOFF only.

```bash
git add HANDOFF.md && git commit -m "docs: admin dashboard live data (v2) deployed — real tenant-1 corpus in the galaxy"
```
