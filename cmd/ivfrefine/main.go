// Command ivfrefine measures the access-gated residual-refinement bet on SIFT1M:
// does allocating a second residual PQ code ONLY to access-hot items (gated by a
// synthetic Zipf access log) capture most of the full-refinement recall gain on
// the ACCESS-WEIGHTED metric, at a fraction of the extra bytes? It reports, for
// four variants, recall1@{1,10,100} both uniform and access-weighted.
//
//	go build -o ivfrefine ./cmd/ivfrefine
//	./ivfrefine -dir /data/sift -prefix sift -nprobe 16 -m2 8 -hotfrac 0.10 -zipf 1.07
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"runtime"
	"sort"
	"time"

	"matrixsentry/internal/lab"
	"matrixsentry/internal/refine"
	"matrixsentry/pq"
)

func main() {
	dir := flag.String("dir", "/data/sift", "dir with .fvecs/.ivecs")
	prefix := flag.String("prefix", "sift", "file prefix")
	nlist := flag.Int("nlist", 1024, "coarse cells")
	M := flag.Int("m", 8, "base PQ subspaces")
	K := flag.Int("k", 256, "base centroids/subspace")
	M2 := flag.Int("m2", 8, "refine PQ subspaces (extra bytes/item when refined)")
	K2 := flag.Int("k2", 256, "refine centroids/subspace")
	iter := flag.Int("iter", 25, "PQ iterations")
	coarseIter := flag.Int("coarseiter", 10, "coarse iterations")
	coarseSample := flag.Int("coarsesample", 39936, "coarse subsample")
	pqSample := flag.Int("pqsample", 65536, "PQ subsample")
	nprobe := flag.Int("nprobe", 16, "cells probed")
	rerankK := flag.Int("rerankk", 200, "candidates re-ranked per query")
	zipf := flag.Float64("zipf", 0.5, "Zipf exponent for the synthetic access log (lower => higher effective sample size; 1.07 is too concentrated, ESS~35)")
	hotfrac := flag.Float64("hotfrac", 0.10, "fraction of items refined (access budget)")
	rpop := flag.Int("rpop", 100, "neighbours credited per query for popularity")
	seed := int64(1)
	flag.Parse()

	fmt.Printf("== ivfrefine · %s (%d cores) ==\n", *prefix, runtime.GOMAXPROCS(0))
	base := lab.ReadFvecs(fmt.Sprintf("%s/%s_base.fvecs", *dir, *prefix))
	learn := lab.ReadFvecs(fmt.Sprintf("%s/%s_learn.fvecs", *dir, *prefix))
	query := lab.ReadFvecs(fmt.Sprintf("%s/%s_query.fvecs", *dir, *prefix))
	gt := lab.ReadIvecs(fmt.Sprintf("%s/%s_groundtruth.ivecs", *dir, *prefix))
	N := len(base)
	fmt.Printf("base=%d learn=%d query=%d nprobe=%d rerankK=%d zipf=%.2f hotfrac=%.2f m2=%d\n\n",
		N, len(learn), len(query), *nprobe, *rerankK, *zipf, *hotfrac, *M2)

	// --- base geometry + per-item assignment/code ---
	t0 := time.Now()
	coarse, basePQ := lab.BuildGeometry(learn, *nlist, *M, *K, *iter, *coarseIter, *coarseSample, *pqSample, seed)
	cell := make([]int32, N)
	baseCode := make([][]uint8, N)
	lab.ParallelFor(N, func(i int) {
		c := lab.Nearest(coarse, base[i])
		cell[i] = int32(c)
		baseCode[i] = basePQ.Encode(lab.Sub(base[i], coarse[c]))
	})
	members := make([][]int32, len(coarse))
	for i := 0; i < N; i++ {
		members[cell[i]] = append(members[cell[i]], int32(i))
	}
	fmt.Printf("base geometry+encode: %v\n", time.Since(t0).Round(time.Millisecond))

	// --- refine PQ trained on first-stage errors e_i = r_i - rhat_i ---
	t1 := time.Now()
	errOf := func(i int) []float32 {
		r := lab.Sub(base[i], coarse[cell[i]])
		return lab.Sub(r, lab.Reconstruct(basePQ, baseCode[i]))
	}
	sampleIdx := rand.New(rand.NewSource(seed + 5)).Perm(N)
	ns := *pqSample
	if ns <= 0 || ns > N {
		ns = N // 0 means "train on all errors"
	}
	trainErr := make([][]float32, ns)
	for k := 0; k < ns; k++ {
		trainErr[k] = errOf(sampleIdx[k])
	}
	refinePQ, err := pq.New(len(base[0]), *M2, *K2)
	must(err)
	must(refinePQ.Train(trainErr, *iter, seed))
	refineCode := make([][]uint8, N)
	lab.ParallelFor(N, func(i int) { refineCode[i] = refinePQ.Encode(errOf(i)) })
	fmt.Printf("refine PQ train+encode: %v\n", time.Since(t1).Round(time.Millisecond))

	// --- synthetic access model: Zipf over queries -> item popularity -> hot set ---
	weights := refine.ZipfWeights(len(query), *zipf, seed+9)
	pop := refine.Popularity(gt, weights, *rpop, N)
	hot := refine.SelectHot(pop, *hotfrac)
	nHot := 0
	for _, h := range hot {
		if h {
			nHot++
		}
	}
	// Effective sample size of the access-weighted metric: (Σw)²/Σw². If this is
	// small (≪ a few hundred), the weighted recall is decided by a handful of
	// queries and the headline is high-variance noise — flag it.
	var sumW, sumW2 float64
	for _, w := range weights {
		sumW += w
		sumW2 += w * w
	}
	ess := sumW * sumW / sumW2
	fmt.Printf("access model: zipf=%.2f, ESS=%.0f/%d queries, hot items=%d (%.1f%%), refine bytes: gated=+%.2f full=+%d per item\n",
		*zipf, ess, len(query), nHot, 100*float64(nHot)/float64(N), float64(*M2)*float64(nHot)/float64(N), *M2)
	if ess < 300 {
		fmt.Printf("   !! WARNING: ESS=%.0f is too low; weighted headline is unreliable. Lower -zipf.\n", ess)
	}
	// Value-blind control: refine a RANDOM nHot items. The novel claim is that
	// access-frequency gating beats this equal-budget blind allocation.
	hotRand := make([]bool, N)
	{
		perm := rand.New(rand.NewSource(seed + 11)).Perm(N)
		for i := 0; i < nHot; i++ {
			hotRand[perm[i]] = true
		}
	}
	fmt.Println()

	// --- search + re-rank for each variant; collect per-query hit booleans ---
	Rs := []int{1, 10, 100}
	variants := []string{"a baseline (no rerank)", "b access-gated", "f random-gated (control)", "c full-refine", "e exact rerank (ceiling)"}
	// hits[v][ri][qi]
	hits := make([][][]bool, len(variants))
	for v := range hits {
		hits[v] = make([][]bool, len(Rs))
		for ri := range Rs {
			hits[v][ri] = make([]bool, len(query))
		}
	}

	t2 := time.Now()
	lab.ParallelFor(len(query), func(qi int) {
		q := query[qi]
		trueNN := int(gt[qi][0])
		probed := lab.ProbeCells(coarse, q, *nprobe)

		// gather candidates with base ADC distance
		type cand struct {
			id   int32
			cell int32
			base float32
		}
		var cands []cand
		qr := make(map[int32][]float32) // query residual per probed cell
		for _, c := range probed {
			r := lab.Sub(q, coarse[c])
			qr[int32(c)] = r
			table := lab.BuildADCTable(basePQ, r)
			for _, id := range members[c] {
				cands = append(cands, cand{id, int32(c), lab.ADC(table, baseCode[id], basePQ.M, basePQ.K)})
			}
		}
		// shortlist: top rerankK by base distance
		sort.Slice(cands, func(a, b int) bool {
			if cands[a].base != cands[b].base {
				return cands[a].base < cands[b].base
			}
			return cands[a].id < cands[b].id
		})
		kk := *rerankK
		if kk > len(cands) {
			kk = len(cands)
		}
		short := cands[:kk]

		// distance under each variant for a shortlisted candidate
		rerank := func(useEhat func(id int32) bool, exact bool) []int32 {
			type sd struct {
				id int32
				d  float64
			}
			sds := make([]sd, len(short))
			for j, cd := range short {
				var d float64
				if exact {
					d = lab.SqL2(q, base[cd.id])
				} else {
					recon := lab.Add(coarse[cd.cell], lab.Reconstruct(basePQ, baseCode[cd.id]))
					if useEhat(cd.id) {
						recon = lab.Add(recon, lab.Reconstruct(refinePQ, refineCode[cd.id]))
					}
					d = lab.SqL2(q, recon)
				}
				sds[j] = sd{cd.id, d}
			}
			sort.Slice(sds, func(a, b int) bool {
				if sds[a].d != sds[b].d {
					return sds[a].d < sds[b].d
				}
				return sds[a].id < sds[b].id
			})
			out := make([]int32, len(sds))
			for j := range sds {
				out[j] = sds[j].id
			}
			return out
		}

		// (a) baseline: already sorted by base distance
		baseOrder := make([]int32, len(short))
		for j, cd := range short {
			baseOrder[j] = cd.id
		}
		orders := [][]int32{
			baseOrder,
			rerank(func(id int32) bool { return hot[id] }, false),     // b access-gated
			rerank(func(id int32) bool { return hotRand[id] }, false), // f random-gated (control)
			rerank(func(id int32) bool { return true }, false),        // c full
			rerank(nil, true),                                         // e exact
		}
		for v, order := range orders {
			for ri, R := range Rs {
				lim := R
				if lim > len(order) {
					lim = len(order)
				}
				for j := 0; j < lim; j++ {
					if int(order[j]) == trueNN {
						hits[v][ri][qi] = true
						break
					}
				}
			}
		}
	})
	fmt.Printf("search+rerank (%d variants): %v\n\n", len(variants), time.Since(t2).Round(time.Millisecond))

	// --- report: uniform vs access-weighted recall per variant ---
	uniform := make([]float64, len(query))
	for i := range uniform {
		uniform[i] = 1
	}
	fmt.Printf("%-26s %-22s %-22s\n", "variant", "uniform recall1@", "access-weighted recall1@")
	fmt.Printf("%-26s %6s %6s %6s    %6s %6s %6s\n", "", "@1", "@10", "@100", "@1", "@10", "@100")
	for v, name := range variants {
		fmt.Printf("%-26s ", name)
		for ri := range Rs {
			fmt.Printf("%.4f ", refine.WeightedRecall(hits[v][ri], uniform))
		}
		fmt.Printf("  ")
		for ri := range Rs {
			fmt.Printf("%.4f ", refine.WeightedRecall(hits[v][ri], weights))
		}
		fmt.Println()
	}

	// --- headline: the novel claim is access-gated vs RANDOM-gated at equal bytes ---
	// (variants: 0 baseline, 1 access-gated, 2 random-gated, 3 full, 4 exact)
	// Lead on @10: @1 is quantization-floored/noisiest, @100 nearly saturated.
	fmt.Printf("\n>> ACCESS-WEIGHTED recall (ESS=%.0f). access-gated vs random-gated both spend +%.2f B/item:\n",
		ess, float64(*M2)*float64(*hotfrac))
	for ri, R := range Rs {
		awBase := refine.WeightedRecall(hits[0][ri], weights)
		awAcc := refine.WeightedRecall(hits[1][ri], weights)
		awRand := refine.WeightedRecall(hits[2][ri], weights)
		awFull := refine.WeightedRecall(hits[3][ri], weights)
		accGain := awAcc - awBase
		randGain := awRand - awBase
		mult := "n/a"
		if randGain > 0.002 {
			mult = fmt.Sprintf("%.1fx", accGain/randGain)
		}
		mark := "  "
		if R == 10 {
			mark = ">>"
		}
		fmt.Printf("   %s @%-3d base %.4f | access-gated %.4f (+%.4f) | random-gated %.4f (+%.4f) | access/random %s | full %.4f\n",
			mark, R, awBase, awAcc, accGain, awRand, randGain, mult, awFull)
	}
	fmt.Printf(">> NOVEL CLAIM holds if access/random >> 1: allocating refine bytes by access frequency beats blind allocation.\n")
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
