// Command ivfpredict tests Mechanism D — PREDICTIVE precision allocation — on
// SIFT1M with a synthetic self-exciting access stream. At each step it refines a
// fixed byte budget of items; the control (static) refines the most-frequently-
// accessed items (the marginal — the hardest non-predictive baseline), while D
// reserves part of the budget for items PREDICTED to be accessed next (online
// Markov on the access stream). D wins only if predicting the next access beats
// knowing the marginal — which should happen iff the stream has sequential
// structure (branching ratio eta>0). Pre-registered: eta=0 => D ~ static.
//
//	go build -o ivfpredict ./cmd/ivfpredict
//	./ivfpredict -dir /data/sift -etas 0,0.3,0.6,0.9 -fracs 0.005,0.02 -predfrac 0.5
package main

import (
	"flag"
	"fmt"
	"math"
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
	M2 := flag.Int("m2", 16, "refine PQ subspaces")
	K2 := flag.Int("k2", 256, "refine centroids/subspace")
	iter := flag.Int("iter", 25, "PQ iterations")
	coarseIter := flag.Int("coarseiter", 10, "coarse iterations")
	coarseSample := flag.Int("coarsesample", 39936, "coarse subsample")
	pqSample := flag.Int("pqsample", 65536, "PQ subsample")
	nprobe := flag.Int("nprobe", 16, "cells probed")
	rerankK := flag.Int("rerankk", 200, "candidates re-ranked per query")
	rpop := flag.Int("rpop", 100, "neighbours credited per access for item popularity")
	T := flag.Int("t", 20000, "access-stream length")
	zipf := flag.Float64("zipf", 0.5, "Zipf exponent for the i.i.d. part of the stream")
	etas := flag.String("etas", "0,0.3,0.6,0.9", "self-exciting branching ratios to sweep")
	fracs := flag.String("fracs", "0.005,0.02", "byte budgets (refined fraction) to sweep (tight=where prediction matters)")
	predfrac := flag.Float64("predfrac", 0.5, "fraction of the budget D reserves for predictions")
	seed := int64(1)
	flag.Parse()

	fmt.Printf("== ivfpredict · %s (%d cores) ==\n", *prefix, runtime.GOMAXPROCS(0))
	base := lab.ReadFvecs(fmt.Sprintf("%s/%s_base.fvecs", *dir, *prefix))
	learn := lab.ReadFvecs(fmt.Sprintf("%s/%s_learn.fvecs", *dir, *prefix))
	query := lab.ReadFvecs(fmt.Sprintf("%s/%s_query.fvecs", *dir, *prefix))
	gt := lab.ReadIvecs(fmt.Sprintf("%s/%s_groundtruth.ivecs", *dir, *prefix))
	N, Nq := len(base), len(query)

	// --- geometry + base codes + refine codes (all items refinable) ---
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
	errOf := func(i int) []float32 {
		return lab.Sub(lab.Sub(base[i], coarse[cell[i]]), lab.Reconstruct(basePQ, baseCode[i]))
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
	refinePQ, err := pq.New(len(base[0]), *M2, *K2)
	must(err)
	must(refinePQ.Train(trainErr, *iter, seed))
	refineCode := make([][]uint8, N)
	lab.ParallelFor(N, func(i int) { refineCode[i] = refinePQ.Encode(errOf(i)) })
	fmt.Printf("geometry+base+refine encode: %v\n", time.Since(t0).Round(time.Millisecond))

	// --- shortlists (top-rerankK by base ADC), once ---
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
		kk := *rerankK
		if kk > len(cands) {
			kk = len(cands)
		}
		sl := make([]slot, kk)
		for j := 0; j < kk; j++ {
			sl[j] = cands[j].s
		}
		short[qi] = sl
	})
	fmt.Printf("shortlists precomputed: %v\n\n", time.Since(t1).Round(time.Millisecond))

	// query popularity bag (i.i.d. part ~ Zipf) and a successor permutation (the
	// learnable sequential structure of the access stream).
	weights := refine.ZipfWeights(Nq, *zipf, seed+9)
	bag := buildBag(weights, 200000, seed+13)
	successor := rand.New(rand.NewSource(seed + 17)).Perm(Nq)

	fmt.Printf("%-5s %-8s %-7s | %-9s %-9s %-9s %-9s | %-9s\n",
		"eta", "frac", "m", "base@10", "static@10", "D@10", "D-static", "oracle@10")

	for _, eta := range parseFloats(*etas) {
		stream := refine.SelfExcitingStream(*T, eta, bag, successor, seed+21) // query indices
		aSeq := make([]int, *T)                                              // accessed item per step = true NN
		for t := 0; t < *T; t++ {
			aSeq[t] = int(gt[stream[t]][0])
		}
		// item popularity over the stream (top-rpop of each accessed query) -> marginal.
		itemPop := make([]float64, N)
		for t := 0; t < *T; t++ {
			row := gt[stream[t]]
			lim := *rpop
			if lim > len(row) {
				lim = len(row)
			}
			for r := 0; r < lim; r++ {
				itemPop[row[r]]++
			}
		}
		popPos := rankDesc(itemPop) // popPos[id] = 0-based popularity rank

		for _, frac := range parseFloats(*fracs) {
			m := int(math.Round(frac * float64(N)))
			reserve := int(math.Round(*predfrac * float64(m)))

			mk := refine.NewMarkov()
			var sumBase, sumStatic, sumD, sumOracle float64
			for t := 0; t < *T; t++ {
				qi := stream[t]
				trueNN := aSeq[t]
				sl := short[qi]
				q := query[qi]

				// predictions from the previous access
				var pred []int
				if t > 0 {
					pred = mk.Predict(aSeq[t-1], reserve)
				}
				predSet := make(map[int]bool, len(pred))
				for _, p := range pred {
					predSet[p] = true
				}
				used := len(pred)

				// active-refine predicates (equal budget m for static and D)
				staticActive := func(id int32) bool { return popPos[id] < m }
				dActive := func(id int32) bool { return predSet[int(id)] || popPos[id] < m-used }
				oracleActive := func(id int32) bool { return int(id) == trueNN || popPos[id] < m-1 }

				if rerankHit(q, sl, coarse, basePQ, refinePQ, baseCode, refineCode, nil, false, 10, trueNN) {
					sumBase++ // baseline = no refinement (useEhat always false)
				}
				if rerankHit(q, sl, coarse, basePQ, refinePQ, baseCode, refineCode, staticActive, true, 10, trueNN) {
					sumStatic++
				}
				if rerankHit(q, sl, coarse, basePQ, refinePQ, baseCode, refineCode, dActive, true, 10, trueNN) {
					sumD++
				}
				if rerankHit(q, sl, coarse, basePQ, refinePQ, baseCode, refineCode, oracleActive, true, 10, trueNN) {
					sumOracle++
				}

				if t > 0 {
					mk.Observe(aSeq[t-1], trueNN)
				}
			}
			Tf := float64(*T)
			fmt.Printf("%-5.2f %-8.3f %-7d | %-9.4f %-9.4f %-9.4f %+-9.4f | %-9.4f\n",
				eta, frac, m, sumBase/Tf, sumStatic/Tf, sumD/Tf, (sumD-sumStatic)/Tf, sumOracle/Tf)
		}
	}
}

// rerankHit re-ranks a shortlist by refined distance (ehat applied where useEhat
// is true, or never if useEhat==nil) and reports whether trueNN is in the top R.
func rerankHit(q []float32, sl []slot, coarse [][]float32, basePQ, refinePQ *pq.PQ,
	baseCode, refineCode [][]uint8, useEhat func(int32) bool, useRefine bool, R, trueNN int) bool {
	type sd struct {
		id int32
		d  float64
	}
	sds := make([]sd, len(sl))
	for j, s := range sl {
		recon := lab.Add(coarse[s.cell], lab.Reconstruct(basePQ, baseCode[s.id]))
		if useRefine && useEhat(s.id) {
			recon = lab.Add(recon, lab.Reconstruct(refinePQ, refineCode[s.id]))
		}
		sds[j] = sd{s.id, lab.SqL2(q, recon)}
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
		if int(sds[j].id) == trueNN {
			return true
		}
	}
	return false
}

// buildBag samples `size` query indices proportional to weights (deterministic),
// forming the i.i.d. popularity bag the stream draws from.
func buildBag(weights []float64, size int, seed int64) []int {
	cum := make([]float64, len(weights))
	var s float64
	for i, w := range weights {
		s += w
		cum[i] = s
	}
	rng := rand.New(rand.NewSource(seed))
	bag := make([]int, size)
	for i := range bag {
		x := rng.Float64() * s
		bag[i] = sort.Search(len(cum), func(j int) bool { return cum[j] >= x })
	}
	return bag
}

// rankDesc returns rank[i] = 0-based position of i when sorted by value desc
// (ties -> lower index first).
func rankDesc(val []float64) []int {
	idx := make([]int, len(val))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		if val[idx[a]] != val[idx[b]] {
			return val[idx[a]] > val[idx[b]]
		}
		return idx[a] < idx[b]
	})
	rank := make([]int, len(val))
	for pos, id := range idx {
		rank[id] = pos
	}
	return rank
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
