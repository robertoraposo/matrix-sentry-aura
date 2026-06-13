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
	feats := []float64{1.5, 2.0, 0.3, 1.1}
	rng1 := rand.New(rand.NewSource(7))
	rng2 := rand.New(rand.NewSource(7))
	tr1 := randTree(rng1, 0, 4, 4, false)
	tr2 := randTree(rng2, 0, 4, 4, false)
	if Eval(tr1, feats) != Eval(tr2, feats) {
		t.Fatal("randTree not deterministic for the same seed")
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
	if res.BestFitness <= 0.5 {
		t.Fatalf("Run did not improve: best fitness %.3f", res.BestFitness)
	}
	rng2 := rand.New(rand.NewSource(42))
	if Run(cfg, fit, rng2).Best.String() != res.Best.String() {
		t.Fatal("Run not deterministic for a fixed seed")
	}
}
