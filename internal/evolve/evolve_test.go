package evolve

import (
	"reflect"
	"testing"
)

// toy grid: 4 genes, 8 values each (indices 0..7). OneMax fitness = sum of
// indices; the unique optimum is genome [7,7,7,7] with fitness 28.
func toyGrid() []Gene {
	g := make([]Gene, 4)
	for i := range g {
		g[i] = Gene{Name: string(rune('a' + i)), Values: []int{0, 1, 2, 3, 4, 5, 6, 7}}
	}
	return g
}

func oneMax(genome []int) float64 {
	s := 0.0
	for _, v := range genome {
		s += float64(v)
	}
	return s
}

func toyConfig() Config {
	return Config{Pop: 16, Gens: 30, TournamentK: 3, CrossRate: 0.9, MutRate: 0.15, Elite: 2, Seed: 42}
}

func TestRunDeterministic(t *testing.T) {
	r1 := Run(toyGrid(), toyConfig(), oneMax, nil)
	r2 := Run(toyGrid(), toyConfig(), oneMax, nil)
	if !reflect.DeepEqual(r1.Best, r2.Best) || r1.BestFitness != r2.BestFitness {
		t.Fatalf("same seed diverged: %v (%f) vs %v (%f)", r1.Best, r1.BestFitness, r2.Best, r2.BestFitness)
	}
	if len(r1.History) != len(r2.History) {
		t.Fatalf("history lengths differ: %d vs %d", len(r1.History), len(r2.History))
	}
	for i := range r1.History {
		if r1.History[i].BestFitness != r2.History[i].BestFitness {
			t.Fatalf("gen %d best differs: %f vs %f", i, r1.History[i].BestFitness, r2.History[i].BestFitness)
		}
	}
}

func TestConvergesOneMax(t *testing.T) {
	r := Run(toyGrid(), toyConfig(), oneMax, nil)
	if r.BestFitness != 28 {
		t.Fatalf("did not reach optimum: best %v fitness %f, want 28", r.Best, r.BestFitness)
	}
	if !reflect.DeepEqual(r.Best, []int{7, 7, 7, 7}) {
		t.Fatalf("best genome %v, want [7 7 7 7]", r.Best)
	}
}

func TestElitismBestNeverDecreases(t *testing.T) {
	r := Run(toyGrid(), toyConfig(), oneMax, nil)
	for i := 1; i < len(r.History); i++ {
		if r.History[i].BestFitness < r.History[i-1].BestFitness {
			t.Fatalf("best fitness decreased at gen %d: %f -> %f",
				i, r.History[i-1].BestFitness, r.History[i].BestFitness)
		}
	}
}

func TestFitnessCachedPerUniqueGenome(t *testing.T) {
	calls := 0
	seen := map[string]bool{}
	fit := func(g []int) float64 {
		calls++
		k := Key(g)
		if seen[k] {
			t.Fatalf("fitness called twice for genome %v", g)
		}
		seen[k] = true
		return oneMax(g)
	}
	Run(toyGrid(), toyConfig(), fit, nil)
	if calls == 0 {
		t.Fatal("fitness never called")
	}
	cfg := toyConfig()
	if max := cfg.Pop * (cfg.Gens + 1); calls > max {
		t.Fatalf("more calls (%d) than genomes ever constructed (%d)", calls, max)
	}
}

func TestValidConstraintRespected(t *testing.T) {
	cfg := toyConfig()
	// forbid any genome where gene0 == gene1 (arbitrary cross-gene constraint,
	// stand-in for Dim%M-style coupling).
	cfg.Valid = func(g []int) bool { return g[0] != g[1] }
	fit := func(g []int) float64 {
		if g[0] == g[1] {
			t.Fatalf("invalid genome evaluated: %v", g)
		}
		return oneMax(g)
	}
	r := Run(toyGrid(), cfg, fit, nil)
	if r.Best[0] == r.Best[1] {
		t.Fatalf("best genome violates constraint: %v", r.Best)
	}
	// optimum under the constraint is fitness 27 (e.g. [6,7,7,7] or [7,6,7,7])
	if r.BestFitness != 27 {
		t.Fatalf("constrained optimum not reached: %v fitness %f, want 27", r.Best, r.BestFitness)
	}
}

func TestOnGenCallbackSeesEveryGeneration(t *testing.T) {
	var gens []int
	cb := func(s GenStats) { gens = append(gens, s.Gen) }
	cfg := toyConfig()
	r := Run(toyGrid(), cfg, oneMax, cb)
	if len(gens) != cfg.Gens+1 { // gen 0 (init) + Gens evolved generations
		t.Fatalf("callback fired %d times, want %d", len(gens), cfg.Gens+1)
	}
	for i, g := range gens {
		if g != i {
			t.Fatalf("gens out of order: %v", gens)
		}
	}
	if len(r.History) != cfg.Gens+1 {
		t.Fatalf("history has %d entries, want %d", len(r.History), cfg.Gens+1)
	}
}

func TestKeyDistinguishesGenomes(t *testing.T) {
	if Key([]int{1, 2}) == Key([]int{12}) {
		t.Fatal("Key must not collide across different genome shapes")
	}
	if Key([]int{1, 2}) == Key([]int{2, 1}) {
		t.Fatal("Key must be order-sensitive")
	}
}

func TestDecodeMapsIndicesToValues(t *testing.T) {
	grid := []Gene{
		{Name: "nlist", Values: []int{32, 64, 128}},
		{Name: "nprobe", Values: []int{4, 8, 16, 32}},
	}
	got := Decode(grid, []int{2, 1})
	want := map[string]int{"nlist": 128, "nprobe": 8}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Decode = %v, want %v", got, want)
	}
}

func TestSeededIndividualsEnterInitialPopulation(t *testing.T) {
	cfg := toyConfig()
	cfg.Gens = 0 // initial population only
	cfg.SeedPop = [][]int{{7, 7, 7, 7}}
	r := Run(toyGrid(), cfg, oneMax, nil)
	if r.BestFitness != 28 {
		t.Fatalf("seeded optimum not in gen 0: best %v (%f)", r.Best, r.BestFitness)
	}
}

func TestPopulationExposedInStats(t *testing.T) {
	cfg := toyConfig()
	cfg.Gens = 2
	r := Run(toyGrid(), cfg, oneMax, nil)
	for _, st := range r.History {
		if len(st.Population) != cfg.Pop || len(st.Fitness) != cfg.Pop {
			t.Fatalf("gen %d: population %d fitness %d, want %d", st.Gen, len(st.Population), len(st.Fitness), cfg.Pop)
		}
		for i := 1; i < len(st.Fitness); i++ {
			if st.Fitness[i] > st.Fitness[i-1] {
				t.Fatalf("gen %d: fitness not sorted desc at %d", st.Gen, i)
			}
		}
	}
}

func TestCreepMutationStaysOnGridAndClimbs(t *testing.T) {
	cfg := toyConfig()
	cfg.CrossRate = 0
	cfg.MutRate = 1.0
	cfg.CreepProb = 1.0 // every mutation is a ±1 grid step
	cfg.Gens = 60
	r := Run(toyGrid(), cfg, oneMax, nil)
	for _, st := range r.History {
		for _, g := range st.Population {
			for i, v := range g {
				if v < 0 || v >= len(toyGrid()[i].Values) {
					t.Fatalf("gen %d: gene %d out of grid: %d", st.Gen, i, v)
				}
			}
		}
	}
	if r.BestFitness != 28 {
		t.Fatalf("creep-only hill climb failed: %f", r.BestFitness)
	}
}

func TestImmigrantsInjectDiversity(t *testing.T) {
	cfg := toyConfig()
	cfg.CrossRate = 0
	cfg.MutRate = 0
	cfg.TournamentK = cfg.Pop * 4 // selection ~always picks the best
	cfg.Elite = 1
	cfg.Immigrants = 2
	cfg.Gens = 3
	r := Run(toyGrid(), cfg, oneMax, nil)
	last := r.History[len(r.History)-1]
	bestK := Key(last.Population[0])
	distinct := 0
	for _, g := range last.Population {
		if Key(g) != bestK {
			distinct++
		}
	}
	if distinct < 2 {
		t.Fatalf("immigrants absent: only %d non-best genomes in final population", distinct)
	}
}

func TestPatienceStopsStaleRun(t *testing.T) {
	cfg := toyConfig()
	cfg.Patience = 3
	flat := func(g []int) float64 { return 1.0 } // never improves after gen 0
	r := Run(toyGrid(), cfg, flat, nil)
	if len(r.History) != cfg.Patience+1 {
		t.Fatalf("flat run produced %d generations, want %d (gen0 + patience)", len(r.History), cfg.Patience+1)
	}
}
