// Command evolve is a genetic-algorithm auto-tuner for the IVFADC engine: it
// evolves (nlist, M, K, nprobe-fraction, rerankK) configurations against REAL
// embeddings, maximizing recall@10 under a deterministic latency-proxy budget
// (Deb feasibility-first), and validates finalists adversarially.
//
// Protocol (panel-reconciled design, 2026-06-12):
//   - Fitness: recall@10 (gt[qi][0] ∈ top-10, the repo's hitIn semantics) on a
//     FIXED 1000-query evolution set E — same queries for every genome, so
//     comparisons are paired and the fitness memo is sound. The remaining 4000
//     queries are the pristine holdout H, untouched until final validation.
//   - The GA never sees wall-clock time: the latency constraint is the
//     deterministic CostProxy, anchored at lmaxMult× the baseline config's
//     proxy. The run ABORTS if the budget admits brute-force exact search
//     (degenerate run that would just converge to exact scan).
//   - Final validation: top-3 distinct feasible phenotypes are retrained with
//     3 kmeans seeds and scored on H; the champion is the best WORST-SEED
//     holdout recall — the number to believe. The E−H gap is the
//     subsample-overfit audit. Wall-clock µs/q (single-thread, warmup,
//     sink anti-DCE) is measured for finalists and the baseline.
//
//	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o evolve-tuner ./cmd/evolve
//	./evolve-tuner -dir /tmp/sembed-big -prefix big -out /tmp/evolve-out
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"matrixsentry/internal/evolve"
	"matrixsentry/internal/lab"
)

type popEntry struct {
	Genome  []int
	Pheno   Phenotype
	Fitness float64
}

type checkpoint struct {
	Gen           int
	ElapsedSec    float64
	Evals         int
	Trainings     int
	BestPheno     Phenotype
	BestFitness   float64
	Mean          float64
	Lmax          float64
	BaselineProxy float64
	Seed          int64
	Population    []popEntry
}

type finalEntry struct {
	Pheno       Phenotype
	RecallE     float64   // evolution-set recall (the GA's view)
	RecallH     []float64 // holdout recall per kmeans seed
	WorstSeed   float64   // min over RecallH — THE number
	USPerQuery  float64   // measured single-thread wall-clock (seed-1 geometry)
	Proxy       float64
	ProxyOfLmax float64 // proxy / Lmax
}

type finalReport struct {
	Champion        finalEntry
	Finalists       []finalEntry
	Baseline        finalEntry
	BaselineRerank  finalEntry // baseline geometry + rerankK=200 (known next lever)
	EHGapChampion   float64    // RecallE − worst-seed RecallH: subsample-overfit audit
	Note            string
}

func main() {
	dir := flag.String("dir", "/tmp/sembed-big", "dataset directory")
	prefix := flag.String("prefix", "big", "dataset file prefix")
	seed := flag.Int64("seed", 1, "master seed (GA + query split)")
	pop := flag.Int("pop", 24, "population size")
	gens := flag.Int("gens", 30, "max generations")
	patience := flag.Int("patience", 8, "early-stop after this many stale generations")
	lmaxMult := flag.Float64("lmax-mult", 2.0, "latency budget = mult x baseline cost proxy")
	evalQ := flag.Int("evalq", 1000, "evolution query subsample size (rest = holdout)")
	out := flag.String("out", "/tmp/evolve-out", "output directory for checkpoints + final report")
	flag.Parse()

	must(os.MkdirAll(*out, 0o755))
	base := lab.ReadFvecs(fmt.Sprintf("%s/%s_base.fvecs", *dir, *prefix))
	learn := lab.ReadFvecs(fmt.Sprintf("%s/%s_learn.fvecs", *dir, *prefix))
	query := lab.ReadFvecs(fmt.Sprintf("%s/%s_query.fvecs", *dir, *prefix))
	gt := lab.ReadIvecs(fmt.Sprintf("%s/%s_groundtruth.ivecs", *dir, *prefix))
	dim := len(base[0])
	fmt.Printf("== evolve · N=%d dim=%d learn=%d queries=%d ==\n", len(base), dim, len(learn), len(query))

	// total partition of the queries: E = evolution set, H = pristine holdout.
	splitRng := rand.New(rand.NewSource(*seed + 101))
	perm := splitRng.Perm(len(query))
	E, H := perm[:*evalQ], perm[*evalQ:]

	grid := TunerGrid()
	basePheno := DecodePheno(grid, BaselineGenome())
	baselineProxy := CostProxy(basePheno, dim, len(base))
	lmax := *lmaxMult * baselineProxy
	if exact := ExactScanProxy(dim, len(base)); lmax >= exact {
		fmt.Fprintf(os.Stderr, "ABORT: Lmax %.0f >= exact-scan proxy %.0f — budget admits brute force, run is degenerate\n", lmax, exact)
		os.Exit(2)
	}
	fmt.Printf("baseline %s proxy=%.2fM ops · Lmax=%.2fM (x%.1f) · |E|=%d |H|=%d\n",
		basePheno.Key(), baselineProxy/1e6, lmax/1e6, *lmaxMult, len(E), len(H))

	tn := newTuner(grid, learn, base, query, gt, E, dim, lmax)
	cfg := evolve.Config{
		Pop: *pop, Gens: *gens, TournamentK: 3,
		CrossRate: 0.9, MutRate: 0.2, CreepProb: 0.8,
		Elite: 2, Immigrants: 2, Patience: *patience, Seed: *seed,
		SeedPop: [][]int{
			BaselineGenome(),    // hand-picked Scenario-B config
			{2, 5, 2, 4, 2},     // baseline + rerankK=100
			{2, 5, 2, 4, 3},     // baseline + rerankK=200 (known next lever)
		},
	}

	start := time.Now()
	onGen := func(st evolve.GenStats) {
		ck := checkpoint{
			Gen: st.Gen, ElapsedSec: time.Since(start).Seconds(),
			Evals: st.Evals, Trainings: tn.trainings,
			BestPheno: DecodePheno(grid, st.Best), BestFitness: st.BestFitness,
			Mean: st.Mean, Lmax: lmax, BaselineProxy: baselineProxy, Seed: *seed,
		}
		for i, g := range st.Population {
			ck.Population = append(ck.Population, popEntry{Genome: g, Pheno: DecodePheno(grid, g), Fitness: st.Fitness[i]})
		}
		writeJSON(filepath.Join(*out, "checkpoint.json"), ck)
		writeJSON(filepath.Join(*out, fmt.Sprintf("gen-%03d.json", st.Gen)), ck)
		fmt.Printf("gen %2d · best %s recall=%.4f · mean %.4f · evals %d · trainings %d · %.0fs\n",
			st.Gen, ck.BestPheno.Key(), st.BestFitness, st.Mean, st.Evals, tn.trainings, ck.ElapsedSec)
	}

	res := evolve.Run(grid, cfg, tn.fitness, onGen)
	fmt.Printf("\nGA done: best %s recall(E)=%.4f after %d evals, %d trainings, %.0fs\n",
		DecodePheno(grid, res.Best).Key(), res.BestFitness, len(tn.memo), tn.trainings, time.Since(start).Seconds())

	// ---- final validation: top-3 distinct feasible phenotypes ----
	finalists := topFeasible(tn, 3)
	report := finalReport{Note: "champion = best WORST-SEED holdout recall; E-H gap is the subsample-overfit audit; differences under ~1.5pp on H are noise"}
	valSeeds := []int64{1, 2, 3}
	timeIdx := H[:200]
	for _, key := range finalists {
		p := tn.phenos[key]
		fe := validate(tn, p, valSeeds, H, timeIdx, lmax)
		report.Finalists = append(report.Finalists, fe)
		fmt.Printf("finalist %s: E=%.4f H=%v worst=%.4f %.0fµs/q\n", p.Key(), fe.RecallE, fe.RecallH, fe.WorstSeed, fe.USPerQuery)
	}
	report.Baseline = validate(tn, basePheno, valSeeds, H, timeIdx, lmax)
	rr := basePheno
	rr.RerankK = 200
	report.BaselineRerank = validate(tn, rr, valSeeds, H, timeIdx, lmax)

	for _, fe := range report.Finalists {
		if fe.WorstSeed > report.Champion.WorstSeed {
			report.Champion = fe
		}
	}
	report.EHGapChampion = report.Champion.RecallE - report.Champion.WorstSeed
	writeJSON(filepath.Join(*out, "final.json"), report)

	fmt.Printf("\n== FINAL (holdout H, worst-seed) ==\n")
	fmt.Printf("baseline        %s: worst=%.4f  %.0fµs/q\n", report.Baseline.Pheno.Key(), report.Baseline.WorstSeed, report.Baseline.USPerQuery)
	fmt.Printf("baseline+rr200  %s: worst=%.4f  %.0fµs/q\n", report.BaselineRerank.Pheno.Key(), report.BaselineRerank.WorstSeed, report.BaselineRerank.USPerQuery)
	fmt.Printf("CHAMPION        %s: worst=%.4f  %.0fµs/q  (E-H gap %+.4f)\n",
		report.Champion.Pheno.Key(), report.Champion.WorstSeed, report.Champion.USPerQuery, report.EHGapChampion)
}

// topFeasible returns the memo keys of the n best feasible phenotypes.
func topFeasible(t *tuner, n int) []string {
	type kv struct {
		k string
		f float64
	}
	var all []kv
	for k, f := range t.memo {
		if f >= 0 {
			all = append(all, kv{k, f})
		}
	}
	// deterministic order: fitness desc, key asc
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j].f > all[i].f || (all[j].f == all[i].f && all[j].k < all[i].k) {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	if len(all) > n {
		all = all[:n]
	}
	out := make([]string, len(all))
	for i, x := range all {
		out[i] = x.k
	}
	return out
}

// validate retrains p with each kmeans seed, scores it on the holdout, and
// wall-clocks the seed-1 geometry single-threaded (warmup + sink anti-DCE).
func validate(t *tuner, p Phenotype, seeds []int64, holdout, timeIdx []int, lmax float64) finalEntry {
	fe := finalEntry{Pheno: p, Proxy: CostProxy(p, t.dim, len(t.base))}
	fe.ProxyOfLmax = fe.Proxy / lmax
	if f, ok := t.memo[p.Key()]; ok {
		fe.RecallE = f
	} else {
		fe.RecallE = t.recall(t.geometry(p), p, t.evalIdx)
	}
	var firstGeom *geometry
	for _, s := range seeds {
		g := buildGeometry(t.learn, t.base, p, s)
		if firstGeom == nil {
			firstGeom = g
		}
		r := t.recall(g, p, holdout)
		fe.RecallH = append(fe.RecallH, r)
		if fe.WorstSeed == 0 || r < fe.WorstSeed {
			fe.WorstSeed = r
		}
	}
	// timing: single-thread, one warmup pass, sink defeats dead-code elim.
	sink := 0
	for _, qi := range timeIdx[:50] {
		rs := searchOne(firstGeom, p, t.queries[qi], t.base)
		if len(rs) > 0 {
			sink += rs[0].ID
		}
	}
	startT := time.Now()
	for _, qi := range timeIdx {
		rs := searchOne(firstGeom, p, t.queries[qi], t.base)
		if len(rs) > 0 {
			sink += rs[0].ID
		}
	}
	fe.USPerQuery = float64(time.Since(startT).Microseconds()) / float64(len(timeIdx))
	fmt.Fprintln(io.Discard, sink)
	return fe
}

func writeJSON(path string, v any) {
	b, err := json.MarshalIndent(v, "", " ")
	must(err)
	tmp := path + ".tmp"
	must(os.WriteFile(tmp, b, 0o644))
	must(os.Rename(tmp, path))
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
