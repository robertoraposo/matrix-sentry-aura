// Package gp is a small, deterministic genetic-programming engine over
// arithmetic expression trees. It evolves Individuals (a pair of trees) to
// maximize a caller-supplied fitness. It is domain-agnostic: the caller maps
// tree outputs to whatever it needs. All randomness flows through a seeded
// *rand.Rand, so a fixed seed reproduces a run exactly.
package gp

import (
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

func (in Individual) size() int         { return in.Nprobe.size() + in.Rerank.size() }
func (in Individual) clone() Individual { return Individual{in.Nprobe.clone(), in.Rerank.clone()} }
func (in Individual) String() string    { return "np=" + str(in.Nprobe) + " rk=" + str(in.Rerank) }

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
		d := 1 + rng.Intn(cfg.InitDepth)
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
			var child Individual
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

// sortByFitnessDesc sorts idx so scored[idx[0]] is largest. Insertion sort:
// dependency-free, deterministic, stable for equal fitness.
func sortByFitnessDesc(idx []int, scored []float64) {
	for i := 1; i < len(idx); i++ {
		for j := i; j > 0 && scored[idx[j]] > scored[idx[j-1]]; j-- {
			idx[j], idx[j-1] = idx[j-1], idx[j]
		}
	}
}
