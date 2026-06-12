package main

import (
	"sort"

	"matrixsentry/internal/evolve"
	"matrixsentry/internal/lab"
	"matrixsentry/pq"
)

// frozen training hyperparameters — cachebench defaults, NOT genes: evolving
// training knobs (or the seed) selects lucky kmeans initializations that do
// not generalize, and doubles the geometry cache keyspace for nothing.
const (
	trainIter         = 25
	trainCoarseIter   = 10
	trainCoarseSample = 0
	trainPQSample     = 65536
	trainSeed         = 1
	geomCacheCap      = 24
)

// geometry is one trained (nlist, M, K) index shape over the base set.
type geometry struct {
	coarse   [][]float32
	basePQ   *pq.PQ
	baseCode [][]uint8
	members  [][]int32
}

// tuner evaluates phenotypes: geometry cache + phenotype-keyed fitness memo.
// fitness is a pure function of the phenotype given (data, evalIdx, lmax).
type tuner struct {
	grid           []evolve.Gene
	learn, base    [][]float32
	queries        [][]float32
	gt             [][]int32
	evalIdx        []int
	dim            int
	lmax           float64
	geoms          map[string]*geometry
	geomOrder      []string // LRU, oldest first
	memo           map[string]float64
	phenos         map[string]Phenotype // memo key -> decoded phenotype
	trainings      int
	rerankAccessed int // cumulative vectors touched by the exact re-rank stage
}

func newTuner(grid []evolve.Gene, learn, base, queries [][]float32, gt [][]int32,
	evalIdx []int, dim int, lmax float64) *tuner {
	return &tuner{
		grid: grid, learn: learn, base: base, queries: queries, gt: gt,
		evalIdx: evalIdx, dim: dim, lmax: lmax,
		geoms: map[string]*geometry{}, memo: map[string]float64{},
		phenos: map[string]Phenotype{},
	}
}

// fitness implements Deb's feasibility-first rules as a scalar: feasible
// genomes score recall@10 ∈ [0,1]; infeasible ones score the negative
// normalized violation (< 0), so any feasible beats any infeasible, feasibles
// compare on recall, and infeasibles compare on violation. Infeasible
// phenotypes are scored from the proxy alone — no geometry training.
func (t *tuner) fitness(genome []int) float64 {
	p := DecodePheno(t.grid, genome)
	if f, ok := t.memo[p.Key()]; ok {
		return f
	}
	var f float64
	if proxy := CostProxy(p, t.dim, len(t.base)); proxy > t.lmax {
		f = -(proxy/t.lmax - 1)
	} else {
		f = t.recall(t.geometry(p), p, t.evalIdx)
	}
	t.memo[p.Key()] = f
	t.phenos[p.Key()] = p
	return f
}

// geometry returns the trained shape for p, training at most once per GeomKey.
func (t *tuner) geometry(p Phenotype) *geometry {
	key := p.GeomKey()
	if g, ok := t.geoms[key]; ok {
		return g
	}
	g := buildGeometry(t.learn, t.base, p, trainSeed)
	t.trainings++
	if len(t.geoms) >= geomCacheCap {
		oldest := t.geomOrder[0]
		t.geomOrder = t.geomOrder[1:]
		delete(t.geoms, oldest)
	}
	t.geoms[key] = g
	t.geomOrder = append(t.geomOrder, key)
	return g
}

func buildGeometry(learn, base [][]float32, p Phenotype, seed int64) *geometry {
	coarse, basePQ := lab.BuildGeometry(learn, p.Nlist, p.M, p.K,
		trainIter, trainCoarseIter, trainCoarseSample, trainPQSample, seed)
	n := len(base)
	cellOf := make([]int, n)
	baseCode := make([][]uint8, n)
	lab.ParallelFor(n, func(i int) {
		c := lab.Nearest(coarse, base[i])
		cellOf[i] = c
		baseCode[i] = basePQ.Encode(lab.Sub(base[i], coarse[c]))
	})
	members := make([][]int32, len(coarse))
	for i := 0; i < n; i++ {
		members[cellOf[i]] = append(members[cellOf[i]], int32(i))
	}
	return &geometry{coarse: coarse, basePQ: basePQ, baseCode: baseCode, members: members}
}

// searchOne runs the full tiered search for one query: ADC over the probed
// cells, then (if RerankK > 0) the exact SqL2 re-rank of the top-RerankK
// shortlist against the float32 originals. Returns the final ranked list.
func searchOne(g *geometry, p Phenotype, q []float32, base [][]float32) []pq.Result {
	var cand []pq.Result
	for _, c := range lab.ProbeCells(g.coarse, q, p.Nprobe) {
		table := lab.BuildADCTable(g.basePQ, lab.Sub(q, g.coarse[c]))
		for _, id := range g.members[c] {
			cand = append(cand, pq.Result{ID: int(id), Dist: lab.ADC(table, g.baseCode[id], g.basePQ.M, g.basePQ.K)})
		}
	}
	sortResults(cand)
	if p.RerankK > 0 {
		k := p.RerankK
		if k > len(cand) {
			k = len(cand)
		}
		re := make([]pq.Result, k)
		for j := 0; j < k; j++ {
			re[j] = pq.Result{ID: cand[j].ID, Dist: float32(lab.SqL2(q, base[cand[j].ID]))}
		}
		sortResults(re)
		cand = re
	}
	return cand
}

// recall computes recall@10 with the repo's hitIn semantics — fraction of
// queries in idx whose TRUE nearest neighbor (gt[qi][0], precomputed exact
// GT, never engine-derived) appears in the final top-10. Parallel over
// queries, results merged by index: deterministic regardless of core count.
func (t *tuner) recall(g *geometry, p Phenotype, idx []int) float64 {
	hits := make([]int, len(idx))
	lab.ParallelFor(len(idx), func(i int) {
		qi := idx[i]
		cand := searchOne(g, p, t.queries[qi], t.base)
		topN := 10
		if topN > len(cand) {
			topN = len(cand)
		}
		want := int(t.gt[qi][0])
		for j := 0; j < topN; j++ {
			if cand[j].ID == want {
				hits[i] = 1
				break
			}
		}
	})
	sum := 0
	for _, h := range hits {
		sum += h
	}
	if p.RerankK > 0 {
		t.rerankAccessed += p.RerankK * len(idx)
	}
	return float64(sum) / float64(len(idx))
}

// sortResults orders by (Dist, ID) — the deterministic tie-break used across
// the repo's harnesses.
func sortResults(rs []pq.Result) {
	sort.Slice(rs, func(a, b int) bool {
		if rs[a].Dist != rs[b].Dist {
			return rs[a].Dist < rs[b].Dist
		}
		return rs[a].ID < rs[b].ID
	})
}
