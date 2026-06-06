// Command ivfsweep maps the efficiency curve of access-gated refinement on
// SIFT1M: it sweeps the refine width M2, the access concentration (Zipf s) and
// the byte budget (hotfrac), reporting access-weighted recall@10 for access-gated
// vs an equal-budget random control. It builds the base geometry once and, the
// key trick, precomputes each query's top-rerankK shortlist once (the expensive
// candidate gather) so every (M2,zipf,hotfrac) cell is a cheap re-rank.
//
//	go build -o ivfsweep ./cmd/ivfsweep
//	./ivfsweep -dir /data/sift -m2s 8,16 -zipfs 0.3,0.5,0.8 -hotfracs 0.05,0.1,0.2
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"matrixsentry/internal/lab"
	"matrixsentry/internal/refine"
	"matrixsentry/pq"
)

type slot struct{ id, cell int32 }

func main() {
	dir := flag.String("dir", "/data/sift", "dir with .fvecs/.ivecs")
	prefix := flag.String("prefix", "sift", "file prefix")
	nlist := flag.Int("nlist", 1024, "coarse cells")
	M := flag.Int("m", 8, "base PQ subspaces")
	K := flag.Int("k", 256, "base centroids/subspace")
	K2 := flag.Int("k2", 256, "refine centroids/subspace")
	iter := flag.Int("iter", 25, "PQ iterations")
	coarseIter := flag.Int("coarseiter", 10, "coarse iterations")
	coarseSample := flag.Int("coarsesample", 39936, "coarse subsample")
	pqSample := flag.Int("pqsample", 65536, "PQ subsample")
	nprobe := flag.Int("nprobe", 16, "cells probed")
	rerankks := flag.String("rerankks", "200,1000", "candidate-shortlist sizes to sweep (raises the exact ceiling)")
	rpop := flag.Int("rpop", 100, "neighbours credited per query for popularity")
	m2s := flag.String("m2s", "8,16,32", "refine widths to sweep")
	zipfs := flag.String("zipfs", "0.5", "Zipf exponents to sweep")
	hotfracs := flag.String("hotfracs", "0.1,0.2", "byte budgets (refined fraction) to sweep")
	seed := int64(1)
	flag.Parse()

	fmt.Printf("== ivfsweep · %s (%d cores) ==\n", *prefix, runtime.GOMAXPROCS(0))
	base := lab.ReadFvecs(fmt.Sprintf("%s/%s_base.fvecs", *dir, *prefix))
	learn := lab.ReadFvecs(fmt.Sprintf("%s/%s_learn.fvecs", *dir, *prefix))
	query := lab.ReadFvecs(fmt.Sprintf("%s/%s_query.fvecs", *dir, *prefix))
	gt := lab.ReadIvecs(fmt.Sprintf("%s/%s_groundtruth.ivecs", *dir, *prefix))
	N, Nq := len(base), len(query)

	// --- base geometry + per-item code (once) ---
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

	// --- precompute each query's top-maxK shortlist by base ADC (the expensive part, once) ---
	rerankKs := parseInts(*rerankks)
	maxK := 0
	for _, k := range rerankKs {
		if k > maxK {
			maxK = k
		}
	}
	t1 := time.Now()
	short := make([][]slot, Nq)
	lab.ParallelFor(Nq, func(qi int) {
		q := query[qi]
		type cd struct {
			s slot
			d float32
		}
		var cands []cd
		for _, c := range lab.ProbeCells(coarse, q, *nprobe) {
			table := lab.BuildADCTable(basePQ, lab.Sub(q, coarse[c]))
			for _, id := range members[c] {
				cands = append(cands, cd{slot{id, int32(c)}, lab.ADC(table, baseCode[id], basePQ.M, basePQ.K)})
			}
		}
		sort.Slice(cands, func(a, b int) bool {
			if cands[a].d != cands[b].d {
				return cands[a].d < cands[b].d
			}
			return cands[a].s.id < cands[b].s.id
		})
		kk := maxK
		if kk > len(cands) {
			kk = len(cands)
		}
		sl := make([]slot, kk)
		for j := 0; j < kk; j++ {
			sl[j] = cands[j].s
		}
		short[qi] = sl
	})
	fmt.Printf("shortlists (top-%d/query) precomputed: %v\n\n", maxK, time.Since(t1).Round(time.Millisecond))

	uniform := make([]float64, Nq)
	for i := range uniform {
		uniform[i] = 1
	}

	fmt.Printf("%-4s %-6s %-7s %-8s %-7s | %-9s %-9s %-9s %-7s | %-9s %-9s\n",
		"M2", "zipf", "ESS", "hotfrac", "rerankK", "base@10w", "acc@10w", "rnd@10w", "acc/rnd", "full@10w", "exact@10w")

	// reconstruct base residual approx once per (M2 build) is cheap; do per query in rerank.
	for _, M2 := range parseInts(*m2s) {
		// refine PQ trained on first-stage errors e=r-rhat; encode all items.
		errOf := func(i int) []float32 {
			r := lab.Sub(base[i], coarse[cell[i]])
			return lab.Sub(r, lab.Reconstruct(basePQ, baseCode[i]))
		}
		ns := *pqSample
		if ns <= 0 || ns > N {
			ns = N
		}
		sidx := rand.New(rand.NewSource(seed + 5)).Perm(N)
		trainErr := make([][]float32, ns)
		for k := 0; k < ns; k++ {
			trainErr[k] = errOf(sidx[k])
		}
		refinePQ, err := pq.New(len(base[0]), M2, *K2)
		must(err)
		must(refinePQ.Train(trainErr, *iter, seed))
		refineCode := make([][]uint8, N)
		lab.ParallelFor(N, func(i int) { refineCode[i] = refinePQ.Encode(errOf(i)) })

		for _, z := range parseFloats(*zipfs) {
			weights := refine.ZipfWeights(Nq, z, seed+9)
			var sumW, sumW2 float64
			for _, w := range weights {
				sumW += w
				sumW2 += w * w
			}
			ess := sumW * sumW / sumW2
			pop := refine.Popularity(gt, weights, *rpop, N)

			for _, hf := range parseFloats(*hotfracs) {
				hot := refine.SelectHot(pop, hf)
				nHot := 0
				for _, h := range hot {
					if h {
						nHot++
					}
				}
				hotRand := make([]bool, N)
				perm := rand.New(rand.NewSource(seed + 11)).Perm(N)
				for i := 0; i < nHot; i++ {
					hotRand[perm[i]] = true
				}

				for _, rk := range rerankKs {
					// rerank the top-rk prefix of each shortlist under each variant
					hAcc := make([]bool, Nq)
					hRnd := make([]bool, Nq)
					hFull := make([]bool, Nq)
					hBase := make([]bool, Nq)
					hExact := make([]bool, Nq)
					lab.ParallelFor(Nq, func(qi int) {
						q := query[qi]
						t := int(gt[qi][0])
						sl := short[qi]
						if rk < len(sl) {
							sl = sl[:rk]
						}
						hBase[qi] = inTopR(sl, 10, t) // baseline order already base-ADC sorted
						hAcc[qi] = rerankTop(q, sl, coarse, basePQ, refinePQ, baseCode, refineCode, base, func(id int32) bool { return hot[id] }, false, 10, t)
						hRnd[qi] = rerankTop(q, sl, coarse, basePQ, refinePQ, baseCode, refineCode, base, func(id int32) bool { return hotRand[id] }, false, 10, t)
						hFull[qi] = rerankTop(q, sl, coarse, basePQ, refinePQ, baseCode, refineCode, base, func(id int32) bool { return true }, false, 10, t)
						hExact[qi] = rerankTop(q, sl, coarse, basePQ, refinePQ, baseCode, refineCode, base, nil, true, 10, t)
					})
					bw := refine.WeightedRecall(hBase, weights)
					aw := refine.WeightedRecall(hAcc, weights)
					rw := refine.WeightedRecall(hRnd, weights)
					fw := refine.WeightedRecall(hFull, weights)
					ew := refine.WeightedRecall(hExact, weights)
					ratio := "n/a"
					if rw-bw > 0.002 {
						ratio = fmt.Sprintf("%.1fx", (aw-bw)/(rw-bw))
					}
					fmt.Printf("%-4d %-6.2f %-7.0f %-8.2f %-7d | %-9.4f %-9.4f %-9.4f %-7s | %-9.4f %-9.4f\n",
						M2, z, ess, hf, rk, bw, aw, rw, ratio, fw, ew)
				}
			}
		}
	}
}

// rerankTop re-ranks a shortlist by the refined distance ||q - (mu_c + rhat + ehat?)||^2
// (or, if exact, by the true ||q - base[id]||^2 — the re-rank ceiling) and returns
// whether item t is in the top R.
func rerankTop(q []float32, sl []slot, coarse [][]float32, basePQ, refinePQ *pq.PQ,
	baseCode, refineCode [][]uint8, base [][]float32, useEhat func(int32) bool, exact bool, R, t int) bool {
	type sd struct {
		id int32
		d  float64
	}
	sds := make([]sd, len(sl))
	for j, s := range sl {
		var d float64
		if exact {
			d = lab.SqL2(q, base[s.id])
		} else {
			recon := lab.Add(coarse[s.cell], lab.Reconstruct(basePQ, baseCode[s.id]))
			if useEhat(s.id) {
				recon = lab.Add(recon, lab.Reconstruct(refinePQ, refineCode[s.id]))
			}
			d = lab.SqL2(q, recon)
		}
		sds[j] = sd{s.id, d}
	}
	sort.Slice(sds, func(a, b int) bool {
		if sds[a].d != sds[b].d {
			return sds[a].d < sds[b].d
		}
		return sds[a].id < sds[b].id
	})
	lim := R
	if lim > len(sds) {
		lim = len(sds)
	}
	for j := 0; j < lim; j++ {
		if int(sds[j].id) == t {
			return true
		}
	}
	return false
}

func inTopR(sl []slot, R, t int) bool {
	lim := R
	if lim > len(sl) {
		lim = len(sl)
	}
	for j := 0; j < lim; j++ {
		if int(sl[j].id) == t {
			return true
		}
	}
	return false
}

func parseInts(csv string) []int {
	var out []int
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			n, err := strconv.Atoi(s)
			must(err)
			out = append(out, n)
		}
	}
	return out
}

func parseFloats(csv string) []float64 {
	var out []float64
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			f, err := strconv.ParseFloat(s, 64)
			must(err)
			out = append(out, f)
		}
	}
	return out
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
