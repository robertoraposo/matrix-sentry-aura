// Package evolve is a small, deterministic genetic algorithm over discrete
// parameter grids. A genome is a slice of indices, one per Gene, so every
// genome is valid-by-construction with respect to per-gene ranges; cross-gene
// constraints go through Config.Valid. Fitness values are cached per unique
// genome, which matters when one evaluation means training an index.
package evolve

import (
	"math/rand"
	"sort"
	"strconv"
	"strings"
)

// Gene is one evolvable parameter: a name and its discrete candidate values.
type Gene struct {
	Name   string
	Values []int
}

// Config holds the GA hyperparameters. Valid, when set, must hold for every
// genome the GA constructs or evaluates (cross-gene constraints).
type Config struct {
	Pop         int
	Gens        int
	TournamentK int
	CrossRate   float64
	MutRate     float64
	CreepProb   float64 // P(a firing mutation is a ±1 grid step instead of a uniform reset)
	Elite       int
	Immigrants  int // random genomes injected per generation (replace worst non-elites)
	Patience    int // stop after this many generations without best improvement (0 = off)
	Seed        int64
	SeedPop     [][]int // genomes guaranteed present in the initial population
	Valid       func(genome []int) bool
}

// GenStats summarizes one generation; Gen 0 is the random initial population.
type GenStats struct {
	Gen         int
	Best        []int
	BestFitness float64
	Mean        float64
	Evals       int       // cumulative fitness evaluations (cache misses)
	Population  [][]int   // this generation's genomes, sorted by fitness desc
	Fitness     []float64 // fitness aligned with Population
}

// Result is the outcome of Run.
type Result struct {
	Best        []int
	BestFitness float64
	History     []GenStats
}

// Key is the canonical string form of a genome, usable as a cache key.
func Key(genome []int) string {
	parts := make([]string, len(genome))
	for i, v := range genome {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ",")
}

// Decode maps a genome's indices back to named parameter values.
func Decode(grid []Gene, genome []int) map[string]int {
	out := make(map[string]int, len(grid))
	for i, g := range grid {
		out[g.Name] = g.Values[genome[i]]
	}
	return out
}

// Run evolves genomes over grid for cfg.Gens generations and returns the best
// genome ever evaluated. Deterministic for a given cfg.Seed: evaluation is
// serial and ties are broken by genome key. onGen, when non-nil, fires once
// per generation (including generation 0) — the checkpoint hook.
func Run(grid []Gene, cfg Config, fitness func([]int) float64, onGen func(GenStats)) Result {
	rng := rand.New(rand.NewSource(cfg.Seed))
	cache := map[string]float64{}
	evals := 0

	evalGenome := func(g []int) float64 {
		k := Key(g)
		if f, ok := cache[k]; ok {
			return f
		}
		f := fitness(g)
		cache[k] = f
		evals++
		return f
	}

	valid := func(g []int) bool { return cfg.Valid == nil || cfg.Valid(g) }

	randomGenome := func() []int {
		for {
			g := make([]int, len(grid))
			for i := range grid {
				g[i] = rng.Intn(len(grid[i].Values))
			}
			if valid(g) {
				return g
			}
		}
	}

	pop := make([][]int, 0, cfg.Pop)
	for _, g := range cfg.SeedPop {
		if len(pop) < cfg.Pop && valid(g) {
			pop = append(pop, append([]int(nil), g...))
		}
	}
	for len(pop) < cfg.Pop {
		pop = append(pop, randomGenome())
	}

	type scored struct {
		genome  []int
		fitness float64
	}
	score := func(pop [][]int) []scored {
		s := make([]scored, len(pop))
		for i, g := range pop {
			s[i] = scored{g, evalGenome(g)}
		}
		sort.Slice(s, func(i, j int) bool {
			if s[i].fitness != s[j].fitness {
				return s[i].fitness > s[j].fitness
			}
			return Key(s[i].genome) < Key(s[j].genome)
		})
		return s
	}

	tournament := func(s []scored) []int {
		best := -1
		for k := 0; k < cfg.TournamentK; k++ {
			c := rng.Intn(len(s))
			if best == -1 || c < best { // s is sorted: lower index = fitter
				best = c
			}
		}
		return s[best].genome
	}

	breed := func(s []scored) [][]int {
		next := make([][]int, 0, cfg.Pop)
		for i := 0; i < cfg.Elite && i < len(s); i++ {
			next = append(next, append([]int(nil), s[i].genome...))
		}
		for len(next) < cfg.Pop {
			p1, p2 := tournament(s), tournament(s)
			child := make([]int, len(grid))
			crossing := rng.Float64() < cfg.CrossRate
			for i := range child {
				if crossing && rng.Intn(2) == 1 {
					child[i] = p2[i]
				} else {
					child[i] = p1[i]
				}
			}
			for i := range child {
				if rng.Float64() < cfg.MutRate {
					if rng.Float64() < cfg.CreepProb {
						// ±1 ordinal step, clamped to the grid ends
						step := 1
						if rng.Intn(2) == 0 {
							step = -1
						}
						v := child[i] + step
						if v < 0 {
							v = 0
						}
						if v >= len(grid[i].Values) {
							v = len(grid[i].Values) - 1
						}
						child[i] = v
					} else {
						child[i] = rng.Intn(len(grid[i].Values))
					}
				}
			}
			if !valid(child) {
				child = randomGenome()
			}
			next = append(next, child)
		}
		for i := 0; i < cfg.Immigrants && cfg.Elite+i < len(next); i++ {
			next[len(next)-1-i] = randomGenome()
		}
		return next
	}

	var history []GenStats
	var best []int
	bestFit := 0.0
	stale := 0
	for gen := 0; gen <= cfg.Gens; gen++ {
		s := score(pop)
		if best == nil || s[0].fitness > bestFit {
			best = append([]int(nil), s[0].genome...)
			bestFit = s[0].fitness
			stale = 0
		} else {
			stale++
		}
		mean := 0.0
		for _, x := range s {
			mean += x.fitness
		}
		mean /= float64(len(s))
		popCopy := make([][]int, len(s))
		fitCopy := make([]float64, len(s))
		for i, x := range s {
			popCopy[i] = append([]int(nil), x.genome...)
			fitCopy[i] = x.fitness
		}
		st := GenStats{
			Gen:         gen,
			Best:        append([]int(nil), best...),
			BestFitness: bestFit,
			Mean:        mean,
			Evals:       evals,
			Population:  popCopy,
			Fitness:     fitCopy,
		}
		history = append(history, st)
		if onGen != nil {
			onGen(st)
		}
		if gen == cfg.Gens || (cfg.Patience > 0 && stale >= cfg.Patience) {
			break
		}
		pop = breed(s)
	}
	return Result{Best: best, BestFitness: bestFit, History: history}
}
