# GP Adaptive Per-Query Policy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Evolve a per-query policy (GP arithmetic-expression trees over coarse-distance margins) that picks `(nprobe, rerankK)` per query to Pareto-dominate the static `ivf.Recommended` config, gated by a feasibility measurement.

**Architecture:** A generic GP engine (`internal/gp`) evolves two expression trees per individual. `ivf` exposes per-query margin features + a `SearchPolicy` that runs the existing residual-ADC + exact-rerank at a policy-chosen depth. `cmd/gpevolve` measures feasibility (per-query min-depth oracle vs margin features) as a hard gate, then evolves on a fixed query set with a latency-budget fitness (proxy latency), and validates the champion on a pristine holdout across kmeans seeds — reusing the GA tuner's anti-self-deception protocol.

**Tech Stack:** Pure Go, zero deps. Builds on `ivf` (real index API) and mirrors `cmd/evolve` (dataset/GT/proxy patterns). Spec: `docs/superpowers/specs/2026-06-13-gp-adaptive-policy-design.md`.

---

## File Structure

- **Create** `internal/gp/gp.go` + `gp_test.go` — generic GP engine: `Node`/`Op`, `Eval` (protected ops), ramped init, depth-limited crossover/mutation, `Run`. Knows nothing about IVF. One responsibility: evolve+evaluate trees.
- **Create** `ivf/policy.go` + `policy_test.go` — `Features`, `coarseSorted`, `Policy` interface, `SearchPolicy`. The per-query adaptive search entry point.
- **Create** `cmd/gpevolve/main.go` + `proxy.go` + `proxy_test.go` — runner: load data, build index, exact GT, static baseline, per-query proxy cost, min-depth oracle + feasibility report (Phase 0), then GP fitness + evolve + holdout validation (Phase 1).
- **Outputs** `results/gp-adaptive-feasibility.md`, `results/gp-adaptive-*.json` + `.log`.

Tasks 1, 2, 3, 5 are code (subagent-implemented, TDD). Tasks 4 and 6 are long-running experiment RUNS on tesla (controller-executed); **Task 4 is the feasibility GATE** — if it fails, Tasks 5–6 do not run.

---

## Task 1: `internal/gp` — the GP engine

**Files:** Create `internal/gp/gp.go`, `internal/gp/gp_test.go`

- [ ] **Step 1: Write the failing tests** → `internal/gp/gp_test.go`

```go
package gp

import (
	"math"
	"math/rand"
	"testing"
)

func TestEvalKnownValue(t *testing.T) {
	// (f0 + 2) * f1  on feats {3, 4} = 20
	tree := &Node{Op: Mul,
		L: &Node{Op: Add, L: &Node{Op: Var, Idx: 0}, R: &Node{Op: Const, Val: 2}},
		R: &Node{Op: Var, Idx: 1}}
	if got := Eval(tree, []float64{3, 4}); got != 20 {
		t.Fatalf("Eval = %v, want 20", got)
	}
}

func TestProtectedDivision(t *testing.T) {
	tree := &Node{Op: Div, L: &Node{Op: Const, Val: 5}, R: &Node{Op: Const, Val: 0}}
	if got := Eval(tree, nil); got != 1 || math.IsNaN(got) || math.IsInf(got, 0) {
		t.Fatalf("protected div by zero = %v, want 1", got)
	}
}

func TestEvalDeterministic(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	tr := randTree(rng, 0, 4, 4, false)
	feats := []float64{1.5, 2.0, 0.3, 1.1}
	if Eval(tr, feats) != Eval(tr, feats) {
		t.Fatal("Eval not deterministic")
	}
}

func TestVariationRespectsDepthLimit(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	const maxD = 5
	a := randTree(rng, 0, 4, 4, true)
	b := randTree(rng, 0, 4, 4, true)
	for i := 0; i < 200; i++ {
		if c := crossover(rng, a, b, maxD); c.depth() > maxD {
			t.Fatalf("crossover produced depth %d > %d", c.depth(), maxD)
		}
		if m := mutate(rng, a, maxD, 4); m.depth() > maxD {
			t.Fatalf("mutate produced depth %d > %d", m.depth(), maxD)
		}
	}
}

func TestRunImprovesFitness(t *testing.T) {
	// Target: maximize f0 (so the engine should evolve toward returning a big
	// value driven by Var0). Fitness = value at feats {1} minus size penalty.
	rng := rand.New(rand.NewSource(42))
	cfg := Config{NVars: 1, PopSize: 40, Generations: 20, MaxDepth: 5, InitDepth: 3, Tournament: 4, Elitism: 2, CxProb: 0.8, Immigrants: 2}
	fit := func(in Individual) float64 {
		v := Eval(in.Nprobe, []float64{1})
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return -1e9
		}
		return v - 0.01*float64(in.size())
	}
	res := Run(cfg, fit, rng)
	// A trivial baseline (a single constant 0) scores 0 - 0.02 ≈ -0.02; evolution
	// should comfortably beat that by composing Var0/constants.
	if res.BestFitness <= 0.5 {
		t.Fatalf("Run did not improve: best fitness %.3f", res.BestFitness)
	}
	// Determinism: same seed -> same champion string.
	rng2 := rand.New(rand.NewSource(42))
	if Run(cfg, fit, rng2).Best.String() != res.Best.String() {
		t.Fatal("Run not deterministic for a fixed seed")
	}
}
```

- [ ] **Step 2: Run, verify FAIL**

Run: `go test ./internal/gp/`
Expected: FAIL — `undefined: Node`, `Eval`, `randTree`, `crossover`, `mutate`, `Config`, `Run`, `Individual`.

- [ ] **Step 3: Implement** → `internal/gp/gp.go`

```go
// Package gp is a small, deterministic genetic-programming engine over
// arithmetic expression trees. It evolves Individuals (a pair of trees) to
// maximize a caller-supplied fitness. It is domain-agnostic: the caller maps
// tree outputs to whatever it needs. All randomness flows through a seeded
// *rand.Rand, so a fixed seed reproduces a run exactly.
package gp

import (
	"fmt"
	"math"
	"math/rand"
	"strconv"
)

type Op uint8

const (
	Add Op = iota
	Sub
	Mul
	Div // protected
	Min
	Max
	Var
	Const
)

var binOps = []Op{Add, Sub, Mul, Div, Min, Max}

// Node is an expression-tree node. Terminals are Var (Idx) or Const (Val);
// internal nodes hold an operator and two children.
type Node struct {
	Op   Op
	Idx  int
	Val  float64
	L, R *Node
}

func (n *Node) isTerminal() bool { return n.Op == Var || n.Op == Const }

// Eval evaluates n on feats. Division is protected (|denom|<1e-9 -> 1), so the
// result is always finite for finite inputs.
func Eval(n *Node, feats []float64) float64 {
	switch n.Op {
	case Const:
		return n.Val
	case Var:
		if n.Idx < 0 || n.Idx >= len(feats) {
			return 0
		}
		return feats[n.Idx]
	case Add:
		return Eval(n.L, feats) + Eval(n.R, feats)
	case Sub:
		return Eval(n.L, feats) - Eval(n.R, feats)
	case Mul:
		return Eval(n.L, feats) * Eval(n.R, feats)
	case Div:
		d := Eval(n.R, feats)
		if math.Abs(d) < 1e-9 {
			return 1
		}
		return Eval(n.L, feats) / d
	case Min:
		return math.Min(Eval(n.L, feats), Eval(n.R, feats))
	case Max:
		return math.Max(Eval(n.L, feats), Eval(n.R, feats))
	}
	return 0
}

func (n *Node) size() int {
	if n == nil {
		return 0
	}
	if n.isTerminal() {
		return 1
	}
	return 1 + n.L.size() + n.R.size()
}

func (n *Node) depth() int {
	if n == nil || n.isTerminal() {
		return 1
	}
	dl, dr := n.L.depth(), n.R.depth()
	if dl > dr {
		return dl + 1
	}
	return dr + 1
}

func (n *Node) clone() *Node {
	if n == nil {
		return nil
	}
	c := *n
	c.L = n.L.clone()
	c.R = n.R.clone()
	return &c
}

func str(n *Node) string {
	if n == nil {
		return "()"
	}
	switch n.Op {
	case Const:
		return strconv.FormatFloat(n.Val, 'g', 4, 64)
	case Var:
		return "f" + strconv.Itoa(n.Idx)
	}
	sym := map[Op]string{Add: "+", Sub: "-", Mul: "*", Div: "/", Min: "min", Max: "max"}[n.Op]
	return "(" + sym + " " + str(n.L) + " " + str(n.R) + ")"
}

// randTerminal returns a Var (70%) or an ephemeral Const in [-1,3) (30%).
func randTerminal(rng *rand.Rand, nVars int) *Node {
	if rng.Float64() < 0.7 {
		return &Node{Op: Var, Idx: rng.Intn(nVars)}
	}
	return &Node{Op: Const, Val: rng.Float64()*4 - 1}
}

// randTree builds a random tree. full forces growth to maxDepth; grow may stop
// early at internal depths.
func randTree(rng *rand.Rand, depth, maxDepth, nVars int, full bool) *Node {
	if depth >= maxDepth || (!full && depth > 0 && rng.Float64() < 0.3) {
		return randTerminal(rng, nVars)
	}
	return &Node{
		Op: binOps[rng.Intn(len(binOps))],
		L:  randTree(rng, depth+1, maxDepth, nVars, full),
		R:  randTree(rng, depth+1, maxDepth, nVars, full),
	}
}

func collect(n *Node, acc *[]*Node) {
	*acc = append(*acc, n)
	if !n.isTerminal() {
		collect(n.L, acc)
		collect(n.R, acc)
	}
}

// crossover grafts a random subtree of b into a clone of a; if the result
// exceeds maxDepth it is rejected and a clone of a is returned (bloat control).
func crossover(rng *rand.Rand, a, b *Node, maxDepth int) *Node {
	child := a.clone()
	var na, nb []*Node
	collect(child, &na)
	collect(b, &nb)
	*na[rng.Intn(len(na))] = *nb[rng.Intn(len(nb))].clone()
	if child.depth() > maxDepth {
		return a.clone()
	}
	return child
}

// mutate replaces a random subtree of a clone of a with a fresh small subtree;
// rejected (returns clone of a) if it exceeds maxDepth.
func mutate(rng *rand.Rand, a *Node, maxDepth, nVars int) *Node {
	child := a.clone()
	var nodes []*Node
	collect(child, &nodes)
	*nodes[rng.Intn(len(nodes))] = *randTree(rng, 0, 3, nVars, false)
	if child.depth() > maxDepth {
		return a.clone()
	}
	return child
}

// Individual is a pair of trees (one per output the caller needs).
type Individual struct{ Nprobe, Rerank *Node }

func (in Individual) size() int          { return in.Nprobe.size() + in.Rerank.size() }
func (in Individual) clone() Individual  { return Individual{in.Nprobe.clone(), in.Rerank.clone()} }
func (in Individual) String() string     { return "np=" + str(in.Nprobe) + " rk=" + str(in.Rerank) }

type Config struct {
	NVars, PopSize, Generations, MaxDepth, InitDepth, Tournament, Elitism, Immigrants int
	CxProb                                                                            float64
}

type GenStats struct {
	Gen        int
	Best, Mean float64
}

type Result struct {
	Best        Individual
	BestFitness float64
	Gens        []GenStats
}

// Run evolves a population to maximize fitness. Deterministic for a fixed rng.
func Run(cfg Config, fitness func(Individual) float64, rng *rand.Rand) Result {
	newIndiv := func() Individual {
		full := rng.Float64() < 0.5
		d := 1 + rng.Intn(cfg.InitDepth) // ramped
		return Individual{
			Nprobe: randTree(rng, 0, d, cfg.NVars, full),
			Rerank: randTree(rng, 0, d, cfg.NVars, full),
		}
	}
	pop := make([]Individual, cfg.PopSize)
	for i := range pop {
		pop[i] = newIndiv()
	}
	cache := map[string]float64{}
	fit := func(in Individual) float64 {
		k := in.String()
		if v, ok := cache[k]; ok {
			return v
		}
		v := fitness(in)
		cache[k] = v
		return v
	}
	scored := make([]float64, cfg.PopSize)
	var res Result
	res.BestFitness = math.Inf(-1)
	for g := 0; g < cfg.Generations; g++ {
		sum := 0.0
		for i, in := range pop {
			scored[i] = fit(in)
			sum += scored[i]
			if scored[i] > res.BestFitness {
				res.BestFitness, res.Best = scored[i], in.clone()
			}
		}
		res.Gens = append(res.Gens, GenStats{Gen: g, Best: res.BestFitness, Mean: sum / float64(cfg.PopSize)})
		if g == cfg.Generations-1 {
			break
		}
		// elitism (best Elitism by fitness)
		order := make([]int, cfg.PopSize)
		for i := range order {
			order[i] = i
		}
		sortByFitnessDesc(order, scored)
		next := make([]Individual, 0, cfg.PopSize)
		for i := 0; i < cfg.Elitism && i < cfg.PopSize; i++ {
			next = append(next, pop[order[i]].clone())
		}
		for i := 0; i < cfg.Immigrants; i++ {
			next = append(next, newIndiv())
		}
		tour := func() Individual {
			best := rng.Intn(cfg.PopSize)
			for j := 1; j < cfg.Tournament; j++ {
				c := rng.Intn(cfg.PopSize)
				if scored[c] > scored[best] {
					best = c
				}
			}
			return pop[best]
		}
		for len(next) < cfg.PopSize {
			p1 := tour()
			child := p1.clone()
			if rng.Float64() < cfg.CxProb {
				p2 := tour()
				child = Individual{
					Nprobe: crossover(rng, p1.Nprobe, p2.Nprobe, cfg.MaxDepth),
					Rerank: crossover(rng, p1.Rerank, p2.Rerank, cfg.MaxDepth),
				}
			} else {
				child = Individual{
					Nprobe: mutate(rng, p1.Nprobe, cfg.MaxDepth, cfg.NVars),
					Rerank: mutate(rng, p1.Rerank, cfg.MaxDepth, cfg.NVars),
				}
			}
			next = append(next, child)
		}
		pop = next
	}
	return res
}

// sortByFitnessDesc sorts idx so scored[idx[0]] is the largest. Insertion sort
// keeps it dependency-free and deterministic (stable for equal fitness).
func sortByFitnessDesc(idx []int, scored []float64) {
	for i := 1; i < len(idx); i++ {
		for j := i; j > 0 && scored[idx[j]] > scored[idx[j-1]]; j-- {
			idx[j], idx[j-1] = idx[j-1], idx[j]
		}
	}
}

var _ = fmt.Sprint // reserved for future debug; remove if unused
```

NOTE: if `fmt` ends up unused, delete the import and the `var _ = fmt.Sprint` line.

- [ ] **Step 4: Run, verify PASS**

Run: `go test ./internal/gp/ && go vet ./internal/gp/`
Expected: 5 tests PASS, vet clean.

- [ ] **Step 5: Commit**

```bash
git add internal/gp/
git commit -m "feat(gp): deterministic GP engine over arithmetic expression trees (protected ops, depth-limited variation)"
```
End the commit body with:
Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>

---

## Task 2: `ivf` — per-query features + `SearchPolicy`

**Files:** Create `ivf/policy.go`, `ivf/policy_test.go`

- [ ] **Step 1: Write the failing tests** → `ivf/policy_test.go`

```go
package ivf

import "testing"

func TestFeatures(t *testing.T) {
	// sorted coarse distances ascending
	d := []float32{2, 4, 6, 8, 10, 12, 14, 16}
	f := Features(d)
	if f[0] != 2 {
		t.Fatalf("f0 = %v, want 2 (nearest)", f[0])
	}
	if f[2] != 2 { // d1-d0 = 4-2
		t.Fatalf("f2 = %v, want 2 (gap)", f[2])
	}
	// f1 = d1/(d0+eps) ≈ 2 ; f3 = mean(top8)/(d0+eps) = 9/2 ≈ 4.5
	if f[1] < 1.99 || f[1] > 2.01 {
		t.Fatalf("f1 = %v, want ~2", f[1])
	}
	if f[3] < 4.4 || f[3] > 4.6 {
		t.Fatalf("f3 = %v, want ~4.5", f[3])
	}
}

// constPolicy returns a fixed depth for every query.
type constPolicy struct{ np, rk int }

func (c constPolicy) Plan(_ [4]float32) (int, int) { return c.np, c.rk }

func TestSearchPolicyEqualsSearchRerankAtConstantDepth(t *testing.T) {
	ix := smallTestIndex(t) // builds a tiny trained+populated index (see helper note)
	q := ix.entries[0:1]    // placeholder; the helper provides a real query vector
	_ = q
	query := testQuery(t)
	fetch := func(uint64) []float32 { return nil } // ADC-only rerank path
	want := ix.SearchRerank(query, 8, 5, 50, fetch)
	got := ix.SearchPolicy(query, constPolicy{np: 8, rk: 50}, 5, fetch)
	if len(got) != len(want) {
		t.Fatalf("len %d != %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Handle.Hash != want[i].Handle.Hash || got[i].Dist != want[i].Dist {
			t.Fatalf("hit %d differs: %+v vs %+v", i, got[i], want[i])
		}
	}
}
```

**Helper note for the implementer:** `ivf` already has test helpers that build a small trained index (see `ivf/index_test.go` / `ivf/rerank_test.go` — reuse the existing pattern, e.g. a `New(cfg)`+`Train`+`Add` of a handful of deterministic vectors, and a fixed query). Replace `smallTestIndex`/`testQuery` with the existing helper(s) in that package; do NOT invent a new fixture if one exists. The assertion is the contract: `SearchPolicy(q, constPolicy{np,rk}, topK, fetch)` must equal `SearchRerank(q, np, topK, rk, fetch)`.

- [ ] **Step 2: Run, verify FAIL**

Run: `go test ./ivf/ -run 'Features|SearchPolicy'`
Expected: FAIL — `undefined: Features`, `SearchPolicy`.

- [ ] **Step 3: Implement** → `ivf/policy.go`

```go
package ivf

import "sort"

// Features derives per-query policy features from the coarse-distance vector
// (distances to every centroid, ascending). All four are cheap functions of the
// margins the search already computes:
//
//	f0 = d0            nearest-centroid distance (query density)
//	f1 = d1/(d0+eps)   routing-ambiguity ratio (~1 ambiguous, large = clear)
//	f2 = d1 - d0       absolute margin/gap
//	f3 = mean(top8)/(d0+eps)  spread across the top cells
func Features(sorted []float32) [4]float32 {
	if len(sorted) == 0 {
		return [4]float32{}
	}
	const eps = float32(1e-9)
	d0 := sorted[0]
	d1 := d0
	if len(sorted) > 1 {
		d1 = sorted[1]
	}
	n := 8
	if n > len(sorted) {
		n = len(sorted)
	}
	var s float32
	for i := 0; i < n; i++ {
		s += sorted[i]
	}
	mean := s / float32(n)
	return [4]float32{d0, d1 / (d0 + eps), d1 - d0, mean / (d0 + eps)}
}

// coarseSorted returns the centroid distances to query, ascending. (~nlist
// squared-L2 evaluations — the same work probeCells does, exposed for features.)
func (ix *Index) coarseSorted(query []float32) []float32 {
	dists := make([]float32, len(ix.coarse))
	for c := range ix.coarse {
		dists[c] = sqL2(ix.coarse[c], query)
	}
	sort.Slice(dists, func(i, j int) bool { return dists[i] < dists[j] })
	return dists
}

// Policy maps per-query features to a search depth.
type Policy interface {
	Plan(feats [4]float32) (nprobe, rerankK int)
}

// SearchPolicy is SearchRerank at a per-query depth chosen by p from the query's
// margin features. With a constant policy it is exactly SearchRerank.
func (ix *Index) SearchPolicy(query []float32, p Policy, topK int, fetch func(uint64) []float32) []Hit {
	feats := Features(ix.coarseSorted(query))
	np, rk := p.Plan(feats)
	return ix.SearchRerank(query, np, topK, rk, fetch)
}
```

- [ ] **Step 4: Run, verify PASS**

Run: `go test ./ivf/ && go vet ./ivf/`
Expected: all `ivf` tests PASS (existing + 2 new), vet clean.

- [ ] **Step 5: Commit**

```bash
git add ivf/policy.go ivf/policy_test.go
git commit -m "feat(ivf): per-query margin Features + SearchPolicy (adaptive depth, == SearchRerank at constant depth)"
```
End the commit body with:
Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>

---

## Task 3: `cmd/gpevolve` — data, GT, baseline, per-query proxy, min-depth oracle + feasibility report

This builds everything Phase 0 (Task 4) needs. Reuse `cmd/evolve` patterns for the dataset loader (read the
`.fvecs`/dataset files the same way `cmd/evolve/main.go` does — read that file and reuse its loader rather than
reinventing it). Build the index through the PRODUCTION API (`ivf.New`→`Train`→`Add` with `ivf.Recommended(dim)`),
exactly like `cmd/ivfverify`. The depth grids and per-query proxy live here.

**Files:** Create `cmd/gpevolve/main.go`, `cmd/gpevolve/proxy.go`, `cmd/gpevolve/proxy_test.go`

- [ ] **Step 1: Write the failing tests** → `cmd/gpevolve/proxy_test.go`

```go
package main

import "testing"

func TestSnapToGrid(t *testing.T) {
	if got := snap(nprobeGrid, 30); got != 32 {
		t.Fatalf("snap nprobe 30 -> %d, want 32 (nearest grid)", got)
	}
	if got := snap(rerankGrid, 7); got != 0 {
		t.Fatalf("snap rerank 7 -> %d, want 0", got)
	}
	if got := snap(nprobeGrid, 1000); got != nprobeGrid[len(nprobeGrid)-1] {
		t.Fatalf("snap above range -> %d, want max %d", got, nprobeGrid[len(nprobeGrid)-1])
	}
	if got := snap(nprobeGrid, -5); got != nprobeGrid[0] {
		t.Fatalf("snap below range -> %d, want min %d", got, nprobeGrid[0])
	}
}

func TestQueryProxyMonotonic(t *testing.T) {
	// Deeper search costs strictly more under the proxy.
	cheap := queryProxy(8, 0, 768, 47000, 64)
	mid := queryProxy(32, 0, 768, 47000, 64)
	deep := queryProxy(32, 200, 768, 47000, 64)
	if !(cheap < mid && mid < deep) {
		t.Fatalf("proxy not monotonic: %.1f %.1f %.1f", cheap, mid, deep)
	}
}

func TestRecallAt10(t *testing.T) {
	// hitIn semantics: the true NN (gt[0]) present in hits -> 1.0, else 0.
	gt := []int32{5, 9, 2}
	hitHashes := map[int32]bool{} // model hits by their ranked ids
	_ = hitHashes
	if recallHit([]int32{1, 5, 7}, 5) != 1.0 {
		t.Fatal("true NN present should score 1.0")
	}
	if recallHit([]int32{1, 7, 3}, 5) != 0.0 {
		t.Fatal("true NN absent should score 0.0")
	}
	_ = gt
}
```

- [ ] **Step 2: Run, verify FAIL**

Run: `go test ./cmd/gpevolve/`
Expected: FAIL — `undefined: snap`, `nprobeGrid`, `rerankGrid`, `queryProxy`, `recallHit`.

- [ ] **Step 3: Implement the proxy/grid/recall helpers** → `cmd/gpevolve/proxy.go`

```go
package main

// Depth grids the policy may choose from. nprobe is capped to nlist at use.
var (
	nprobeGrid = []int{1, 2, 4, 8, 16, 32, 64}
	rerankGrid = []int{0, 50, 100, 200, 400}
)

// snap clamps v to [grid[0], grid[last]] and returns the nearest grid value.
func snap(grid []int, v int) int {
	if v <= grid[0] {
		return grid[0]
	}
	if v >= grid[len(grid)-1] {
		return grid[len(grid)-1]
	}
	best := grid[0]
	bestd := 1 << 30
	for _, g := range grid {
		d := g - v
		if d < 0 {
			d = -d
		}
		if d < bestd {
			bestd, best = d, g
		}
	}
	return best
}

// queryProxy is the deterministic per-query cost (in float-op units) of a search
// at (nprobe, rerankK): ADC over the probed lists (balanced-list estimate
// n*nprobe/nlist candidates) plus exact L2 (2*dim ops) over rerankK candidates.
// Mirrors cmd/evolve's CostProxy shape; wall-clock is never used in evolution.
func queryProxy(nprobe, rerankK, dim, n, nlist int) float64 {
	scanned := float64(n) * float64(nprobe) / float64(nlist)
	return scanned + float64(rerankK)*2*float64(dim)
}

// recallHit is the repo's recall@10 hitIn semantics for one query: 1.0 if the
// true nearest neighbor (gt0) is among the returned ids, else 0.0.
func recallHit(rankedIDs []int32, gt0 int32) float64 {
	for _, id := range rankedIDs {
		if id == gt0 {
			return 1.0
		}
	}
	return 0.0
}
```

- [ ] **Step 4: Run, verify PASS**

Run: `go test ./cmd/gpevolve/`
Expected: 3 tests PASS.

- [ ] **Step 5: Implement `main.go`** — data load, index, GT, baseline, oracle, feasibility report.

Write `cmd/gpevolve/main.go` with these pieces (read `cmd/evolve/main.go` first and reuse its `.fvecs`/dataset
loader verbatim; read `cmd/ivfverify` for the production index-build pattern):

1. **Flags:** `-data` (glob/prefix for the 47k base + query files, same as cmd/evolve), `-e` (E size, default 1000), `-h` (H size, default 4000), `-seed`, `-out` (results path), `-phase` (`feasibility` | `evolve`, default `feasibility`).
2. **Load** base vectors `[][]float32` and query vectors; split queries into E and H (disjoint slices).
3. **Build** the index: `cfg, sp, _ := ivf.Recommended(dim)`; `ix := ivf.New(cfg)`; `ix.Train(base, ...)`; `ix.Add(base)` (follow the exact ivf API/argument order from `cmd/ivfverify`). Keep `sp` (Nprobe=32, RerankK=200) as the static baseline depth. Build a `fetch` closure returning the original base vector by `Handle.Hash` (use `ivf.HashVec` to key a `map[uint64][]float32` over base, as ivfverify does for SearchRerank).
4. **Exact GT** for E and H: brute-force top-1 (the true NN id) per query over base (`gt0[q]`). Abort the run if `ExactScanProxy(dim, n)`-equivalent is ever treated as within budget (guard like cmd/evolve).
5. **Static baseline** on E: for each q, `SearchRerank(q, sp.Nprobe, 10, sp.RerankK, fetch)` → recallHit vs gt0; meanRecall and mean `queryProxy(sp.Nprobe, sp.RerankK, …)`. This `(recallStatic, latStatic)` is the point to dominate; `latStatic` is the evolution budget.
6. **Per-query min-depth oracle** (Phase 0): for each q in E, scan the grid (nprobe×rerankK) in ascending `queryProxy` order; the first depth whose `SearchRerank` retrieves gt0 is that query's min-depth; record its proxy + the query's `ivf.Features(ix-coarse-sorted)`. (Expose coarse-sorted for the runner via a tiny exported shim OR recompute features by calling a new `ivf` helper — simplest: add nothing; compute features by re-deriving from `SearchPolicy`'s path is internal, so instead expose `ivf.QueryFeatures(ix, q) [4]float32` — add a 3-line exported wrapper in `ivf/policy.go` calling `Features(ix.coarseSorted(q))`, and a test that it is non-panicking on a built index. Add that wrapper as part of THIS task.)
7. **Feasibility analysis:** (a) oracle-Pareto ceiling = mean recall (≈1.0 by construction for gt0, so instead report: at the oracle's mean latency, the recall is 1.0 — compare the oracle mean latency to `latStatic`; the headroom is `latStatic - latOracle` at equal-or-better recall); (b) predictability = correlation between each feature and the oracle min-depth proxy (Spearman or bucketed mean depth per feature quartile — a simple Pearson on ranks is fine). Write `results/gp-adaptive-feasibility.md` with: `(recallStatic, latStatic)`, oracle mean latency + recall, the per-feature correlation table, and a **VERDICT line**: `GATE PASS` if oracle latency is materially below static at ≥ static recall AND at least one feature correlates with needed depth; else `GATE FAIL` with the reason.
8. The `-phase evolve` branch is added in Task 5; for now it may print "evolve phase not yet implemented" and exit.

Because main.go orchestrates real data, its unit-testable logic (snap/proxy/recall) is already covered in Step 1–4; main itself is exercised by the Task 4 run. Keep main.go focused; put any pure helper you want to test in `proxy.go` with a test.

- [ ] **Step 6: Build + vet + commit**

Run: `go build ./cmd/gpevolve/ && go vet ./cmd/gpevolve/ && go test ./cmd/gpevolve/ && go build ./... && go test ./...`
Expected: builds, vet clean, module green.

```bash
git add cmd/gpevolve/ ivf/policy.go ivf/policy_test.go
git commit -m "feat(gpevolve): data/GT/baseline + per-query proxy + min-depth oracle + feasibility report (Phase 0)"
```
End the commit body with:
Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>

---

## Task 4: RUN the feasibility gate on tesla (controller-executed — GATE)

No new code. Cross-compile, ship to tesla (which holds `/tmp/sembed-big/big_*`, 47k×768), run Phase 0.

- [ ] **Step 1:** `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/gpevolve ./cmd/gpevolve` and `scp /tmp/gpevolve tesla:/tmp/gpevolve`.
- [ ] **Step 2:** `ssh tesla '/tmp/gpevolve -data /tmp/sembed-big/big_ -phase feasibility -out /tmp/gp-feasibility'` (match the actual data prefix/flags the loader expects).
- [ ] **Step 3:** Read the feasibility output. Copy it into `results/gp-adaptive-feasibility.md`. Evaluate the VERDICT:
  - **GATE FAIL** → STOP. Commit the feasibility finding, report to the user that the coarse-margin signal does not yield Pareto headroom (with the numbers), and do NOT proceed to Tasks 5–6. This is a legitimate, valuable outcome (a cheap check that saved building a doomed engine).
  - **GATE PASS** → proceed to Task 5, noting the oracle ceiling (the GP's target) and which features correlate.
- [ ] **Step 4:** Commit: `git add results/gp-adaptive-feasibility.md && git commit -m "docs(gpevolve): Phase-0 feasibility result — <PASS/FAIL>, oracle ceiling vs static"` (end body with the Co-Authored-By trailer).

---

## Task 5: `cmd/gpevolve` evolve phase — GP fitness, evolution, holdout validation (only if Task 4 PASSED)

**Files:** Modify `cmd/gpevolve/main.go`; create `cmd/gpevolve/policy.go` + `policy_test.go` (the GP→ivf.Policy adapter).

- [ ] **Step 1: Write the failing test** → `cmd/gpevolve/policy_test.go`

```go
package main

import (
	"testing"

	"matrixsentry/internal/gp"
)

func TestGPPolicySnapsToGrid(t *testing.T) {
	// nprobe tree returns 30 -> snaps to 32; rerank tree returns 7 -> snaps to 0.
	p := gpPolicy{ind: gp.Individual{
		Nprobe: &gp.Node{Op: gp.Const, Val: 30},
		Rerank: &gp.Node{Op: gp.Const, Val: 7},
	}, nlist: 64}
	np, rk := p.Plan([4]float32{1, 2, 3, 4})
	if np != 32 || rk != 0 {
		t.Fatalf("Plan = (%d,%d), want (32,0)", np, rk)
	}
}

func TestGPPolicyCapsNprobeToNlist(t *testing.T) {
	p := gpPolicy{ind: gp.Individual{
		Nprobe: &gp.Node{Op: gp.Const, Val: 1000},
		Rerank: &gp.Node{Op: gp.Const, Val: 0},
	}, nlist: 16}
	np, _ := p.Plan([4]float32{1, 1, 1, 1})
	if np > 16 {
		t.Fatalf("nprobe %d exceeds nlist 16", np)
	}
}
```

- [ ] **Step 2: Run, verify FAIL** — `go test ./cmd/gpevolve/ -run GPPolicy` → `undefined: gpPolicy`.

- [ ] **Step 3: Implement the adapter** → `cmd/gpevolve/policy.go`

```go
package main

import "matrixsentry/internal/gp"

// gpPolicy adapts a GP individual to ivf.Policy: evaluate each tree on the
// query features, then clamp+snap to the legal grids (nprobe additionally
// capped to nlist). A non-finite output snaps to the grid extreme via snap.
type gpPolicy struct {
	ind   gp.Individual
	nlist int
}

func (p gpPolicy) Plan(feats [4]float32) (int, int) {
	f := []float64{float64(feats[0]), float64(feats[1]), float64(feats[2]), float64(feats[3])}
	np := snap(nprobeGrid, int(gp.Eval(p.ind.Nprobe, f)))
	if np > p.nlist {
		np = p.nlist
	}
	rk := snap(rerankGrid, int(gp.Eval(p.ind.Rerank, f)))
	return np, rk
}
```

- [ ] **Step 4: Run, verify PASS** — `go test ./cmd/gpevolve/ -run GPPolicy` (2 tests PASS).

- [ ] **Step 5: Wire the evolve phase in `main.go`** (the `-phase evolve` branch):

1. **Fitness** over E: given a `gp.Individual`, build `gpPolicy{ind, nlist:cfg.Nlist}`; for each q in E compute features ONCE and reuse them for both the depth and the search (avoids a redundant coarse scan): `feats := ivf.QueryFeatures(ix, q); np, rk := policy.Plan(feats); hits := ix.SearchRerank(q, np, 10, rk, fetch)` → `recallHit(idsOf(hits), gt0[q])` and `queryProxy(np, rk, dim, len(base), cfg.Nlist)`; accumulate meanRecall, meanLat. (`idsOf` maps `Hit.Handle` to its base id via the same hash map used for `fetch`.) `fitness = meanRecall − LAMBDA_LAT*max(0, meanLat-budget)/budget − PARSIMONY*float64(ind.size())` where `budget = latStatic`, `LAMBDA_LAT = 1.0`, `PARSIMONY = 1e-4` (constants at top of main.go, documented). Guard non-finite → large negative.
2. **Evolve:** `gp.Run(gp.Config{NVars:4, PopSize:60, Generations:40, MaxDepth:6, InitDepth:4, Tournament:5, Elitism:2, Immigrants:3, CxProb:0.8}, fitness, rand.New(rand.NewSource(seed)))`. Log per-gen best/mean.
3. **Holdout validation:** rebuild geometry for ≥3 kmeans seeds (re-`Train` with different `cfg.Seed`), and for each, measure the champion's `(recall, lat)` on H AND the static baseline on H. Champion is reported with its worst-seed H point. Confirm single-thread wall-clock for the champion vs static on H (anti-DCE: sum a checksum of results).
4. **Verdict:** the champion DOMINATES iff on H (worst seed) `recallChamp ≥ recallStatic − noise AND latChamp ≤ latStatic` with at least one strict beyond noise (~0.3pp recall). Emit `results/gp-adaptive-<seed>.json` (champion policy `.String()`, E and H points, static points per seed, E–H gap, gens) + `.log`.

- [ ] **Step 6: Build + module green + commit**

Run: `go build ./... && go vet ./... && go test ./...` (all green).

```bash
git add cmd/gpevolve/
git commit -m "feat(gpevolve): evolve phase — GP fitness (recall at latency budget), evolution, multi-seed holdout validation"
```
End the commit body with:
Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>

---

## Task 6: RUN evolution + holdout validation on tesla (controller-executed; only if Task 4 PASSED)

- [ ] **Step 1:** Cross-compile + ship `gpevolve` to tesla (as Task 4 Step 1).
- [ ] **Step 2:** `ssh tesla '/tmp/gpevolve -data /tmp/sembed-big/big_ -phase evolve -seed 1 -out /tmp/gp-evolve'` (long-running, like the GA's ~2.5h; run detached/backgrounded and poll).
- [ ] **Step 3:** Retrieve the results JSON/log. Record the champion's learned policy, the E and H Pareto points vs static, and the E–H gap. Copy artifacts to `results/`.
- [ ] **Step 4: Adversarial read of the verdict** before believing it:
  - Does the champion DOMINATE on the **holdout H** (not just E)? An E-only win is overfit.
  - Is the win beyond the ~0.3pp / seed-noise band across all ≥3 kmeans seeds (worst-seed)?
  - Did wall-clock confirm the proxy's latency ordering on the champion?
  - Sanity: does the learned policy *make sense* (e.g., low nprobe when the ratio feature is large/clear, high when ambiguous)? A nonsensical policy that "wins" is a red flag for a metric leak.
  If any check fails, the honest verdict is "no robust Pareto gain" — record it (like the closed-loop reversal) rather than shipping.
- [ ] **Step 5:** Update `HANDOFF.md` + memory with the outcome (PASS → champion policy + numbers + a note that production wiring is a separate spec; FAIL → the negative result and why). Commit.

---

## Notes for the implementer

- **Determinism:** the GP uses a seeded `*rand.Rand`; the kmeans seed is NOT a gene (it varies only in holdout validation). Never use global `rand` or time.
- **Proxy, never wall-clock, during evolution** — wall-clock only confirms the champion (anti-DCE: accumulate a result checksum so the compiler can't elide the search).
- **The gate is real:** Task 4 FAIL means STOP. A negative feasibility result is a successful outcome of this plan, not a failure to fix.
- **Reuse, don't reinvent:** the `.fvecs`/dataset loader and the production index-build pattern already exist in `cmd/evolve` and `cmd/ivfverify` — read and reuse them.
- **No production deploy here.** Winning only earns a follow-up deployment spec; `ivf.SearchPolicy` exists but is not wired into `memory`/`sentrymcp` by this plan.
```
