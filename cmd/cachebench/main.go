// Command cachebench measures the OPERATIONAL value of an access-driven EXACT
// TIER wired into the search path, on REAL embeddings. The tier mirrors a cache
// policy (LRU or Markov-prefetch — the live-validated predictor) with
// UNCOMPRESSED vectors; tiered search scores residents with EXACT distances and
// merges them into the ADC shortlist, so a cache hit is recall-perfect by
// construction and immune to coarse-routing misses.
//
// Protocol per step (strictly causal): search with the tier built from
// accesses < t, THEN record access t. Reported per (eta, seed, policy, cap):
//   - tier hit-rate and fetch bandwidth (misses + prefetch loads)
//   - stream recall@10: base vs tiered, plus recall conditioned on tier hit/miss
//   - frozen-tier recall@10 over ALL queries (unweighted) — the adversarial
//     mixed-scale check: exact and ADC distances share one ranking, so a tier
//     of hot items must NOT degrade cold queries
//   - single-thread latency: full ADC search vs tiered search vs resident-id
//     lookup
//
// HONEST SCOPING (adversarial review, 2026-06-09):
//   - Default stream is OPEN-LOOP (every access = the ground-truth NN of the
//     query, even when the search missed it). -closedloop feeds the tier what
//     the engine actually returned — the deployable protocol; report both.
//   - The "resident-id ns" column is the ID-ADDRESSED re-access hit path (the
//     journal/cachesim workload). A QUERY cannot take it without searching:
//     for query workloads the tier is a recall lever at +0–5% search overhead,
//     not a latency win. Do not pair resident-id ns with miss µs/q.
//   - tierHit magnitudes are protocol-specific (deterministic successor =
//     exactly the Markov hypothesis class); they do not transfer as measured.
//
//	go build -o cachebench ./cmd/cachebench
//	./cachebench -dir /tmp/sembed-big -prefix big -etas 0,0.3,0.6 -seeds 1,2
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

	"matrixsentry/internal/cache"
	"matrixsentry/internal/lab"
	"matrixsentry/internal/refine"
	"matrixsentry/pq"
)

func main() {
	dir := flag.String("dir", "/tmp/sembed-big", "dir with .fvecs/.ivecs")
	prefix := flag.String("prefix", "big", "file prefix")
	nlist := flag.Int("nlist", 64, "coarse cells (Scenario B engine config)")
	M := flag.Int("m", 96, "PQ subspaces (Scenario B: 32x compression)")
	K := flag.Int("k", 256, "PQ centroids/subspace")
	iter := flag.Int("iter", 25, "PQ iterations")
	coarseIter := flag.Int("coarseiter", 10, "coarse iterations")
	coarseSample := flag.Int("coarsesample", 0, "coarse subsample (0=all)")
	pqSample := flag.Int("pqsample", 65536, "PQ subsample")
	nprobe := flag.Int("nprobe", 16, "cells probed")
	rerankK := flag.Int("rerankk", 200, "ADC shortlist per query")
	T := flag.Int("t", 20000, "access-stream length")
	zipf := flag.Float64("zipf", 0.5, "Zipf exponent for the i.i.d. part")
	etas := flag.String("etas", "0,0.3,0.6", "branching ratios (0.3 ~ live agent)")
	seeds := flag.String("seeds", "1,2", "stream seeds")
	capsCSV := flag.String("caps", "16,64,256", "tier capacities")
	latQ := flag.Int("latq", 1000, "queries timed in the latency bench")
	diag := flag.Bool("diag", false, "diagnose hit-failures (why is rec|hit < 1?)")
	closedLoop := flag.Bool("closedloop", false, "feed the tier what the search returned (deployable protocol), not the oracle NN")
	flag.Parse()

	fmt.Printf("== cachebench · %s (%d cores) · nlist=%d M=%d nprobe=%d ==\n",
		*prefix, runtime.GOMAXPROCS(0), *nlist, *M, *nprobe)
	base := lab.ReadFvecs(fmt.Sprintf("%s/%s_base.fvecs", *dir, *prefix))
	learn := lab.ReadFvecs(fmt.Sprintf("%s/%s_learn.fvecs", *dir, *prefix))
	query := lab.ReadFvecs(fmt.Sprintf("%s/%s_query.fvecs", *dir, *prefix))
	gt := lab.ReadIvecs(fmt.Sprintf("%s/%s_groundtruth.ivecs", *dir, *prefix))
	N, Nq := len(base), len(query)
	fmt.Printf("base=%d query=%d dim=%d\n", N, Nq, len(base[0]))

	// --- geometry + base codes ---
	t0 := time.Now()
	seed := int64(1)
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
	fmt.Printf("geometry+encode: %v\n", time.Since(t0).Round(time.Millisecond))

	// --- per-query ADC shortlists (top-rerankK), once. Merging the tier into
	// this shortlist is EXACTLY full-search+merge: a non-resident item past
	// rank rerankK can never reach the merged top-10, and residents enter as
	// candidates regardless of routing. ---
	t1 := time.Now()
	cands := make([][]pq.Result, Nq)
	baseHit10 := make([]bool, Nq)
	lab.ParallelFor(Nq, func(qi int) {
		cands[qi] = adcSearch(query[qi], coarse, basePQ, members, baseCode, *nprobe, *rerankK)
		baseHit10[qi] = hitIn(cands[qi], int(gt[qi][0]), 10)
	})
	fmt.Printf("shortlists precomputed: %v\n\n", time.Since(t1).Round(time.Millisecond))

	caps := parseInts(*capsCSV)
	for _, cp := range caps {
		if cp > *rerankK-10 {
			fmt.Printf("NOTE: cap=%d > rerankK-10=%d — the shortlist-merge ≡ full-search+merge equivalence is no longer structural for that row\n", cp, *rerankK-10)
		}
	}
	if *closedLoop {
		fmt.Println("MODE: closed-loop (tier consumes what the engine returns)")
	}
	fetch := func(item int) []float32 { return base[item] }

	for _, sd := range parseInts(*seeds) {
		for _, eta := range parseFloats(*etas) {
			s := int64(sd)
			weights := refine.ZipfWeights(Nq, *zipf, s+9)
			bag := buildBag(weights, 200000, s+13)
			successor := rand.New(rand.NewSource(s + 17)).Perm(Nq)
			stream := refine.SelfExcitingStream(*T, eta, bag, successor, s+21) // query indices
			aSeq := make([]int, *T)
			for t := 0; t < *T; t++ {
				aSeq[t] = int(gt[stream[t]][0])
			}
			sl := refine.StreamLift(aSeq)
			var baseStream float64
			for t := 0; t < *T; t++ {
				if baseHit10[stream[t]] {
					baseStream++
				}
			}
			baseStream /= float64(*T)

			fmt.Printf("-- seed=%d eta=%.2f · lift=%.1f%% (markov %.1f%% / marginal %.1f%%, cov %.1f%%) · base stream@10=%.4f --\n",
				sd, eta, sl.Lift*100, sl.MarkovHit*100, sl.MarginalHit*100, sl.Coverage*100, baseStream)
			fmt.Printf("%-7s %-5s | %-7s %-9s | %-9s %-8s | %-8s %-8s | %-9s %-9s\n",
				"policy", "cap", "tierHit", "fetch/acc", "stream@10", "Δstream", "rec|hit", "rec|miss", "all@10", "Δall")

			for _, policy := range []string{"LRU", "MK"} {
				for _, cp := range caps {
					runCell(policy, cp, stream, aSeq, query, cands, gt, fetch, baseStream, baseHit10, *T, *diag, base, *closedLoop)
				}
			}
			fmt.Println()
		}
	}

	// --- latency bench (single-thread, real searches) ---
	latencyBench(base, query, coarse, basePQ, members, baseCode, gt,
		*nprobe, caps, *latQ, *T, *zipf, parseFloats(*etas), fetch)
}

// runCell replays one (policy, cap) cell causally and prints its row.
func runCell(policy string, cp int, stream, aSeq []int, query [][]float32,
	cands [][]pq.Result, gt [][]int32, fetch func(int) []float32,
	baseStream float64, baseHit10 []bool, T int, diag bool, base [][]float32,
	closedLoop bool) {

	var pol cache.Cache
	if policy == "LRU" {
		pol = cache.NewLRU(cp)
	} else {
		pol = cache.NewMarkovPrefetch(cp)
	}
	tier := cache.NewExactTier(pol, fetch)

	var hits, tierHits float64
	var hitRec, missRec, missN float64
	var failN, failNearEq, failUnder, failCrowded float64
	for t := 0; t < T; t++ {
		qi := stream[t]
		trueNN := aSeq[t]
		resident := tier.Vec(trueNN) != nil // BEFORE this access (causal)

		td := tierDists(query[qi], tier)
		merged := mergeTiered(cands[qi], td, 10)
		ok := hitIn(merged, trueNN, 10)
		if ok {
			hits++
		}
		if resident {
			tierHits++
			if ok {
				hitRec++
			} else if diag {
				// why did a RESIDENT true NN miss top-10? Either the top-10 is
				// near-equal true distances (near-duplicate crowding — metric
				// pedantry, the answers are equivalent) or ADC underestimates
				// displaced it (real ranking damage).
				q := query[qi]
				dNN := lab.SqL2(q, base[trueNN])
				nearEq, under := 0, 0
				for _, r := range merged {
					trueD := lab.SqL2(q, base[r.ID])
					if trueD <= dNN*1.01 {
						nearEq++
					}
					if _, inTier := td[r.ID]; !inTier && float64(r.Dist) < trueD {
						under++
					}
				}
				failN++
				failNearEq += float64(nearEq)
				failUnder += float64(under)
				if nearEq >= 10 {
					failCrowded++
				}
			}
		} else {
			missN++
			if ok {
				missRec++
			}
		}
		accessed := trueNN
		if closedLoop && !ok && len(merged) > 0 {
			accessed = merged[0].ID // the agent consumes what the engine returned
		}
		tier.Access(accessed)
	}
	if diag && failN > 0 {
		fmt.Printf("   diag %s/%d: %d hit-failures · mean near-equal in top10 = %.1f/10 · mean ADC-underestimates = %.1f/10 · fully-crowded (≥10 near-eq) = %.0f%%\n",
			policy, cp, int(failN), failNearEq/failN, failUnder/failN, failCrowded/failN*100)
	}
	Tf := float64(T)
	streamRec := hits / Tf
	condHit := 0.0
	if tierHits > 0 {
		condHit = hitRec / tierHits
	}
	condMiss := 0.0
	if missN > 0 {
		condMiss = missRec / missN
	}

	// frozen-tier unweighted check over ALL queries (mixed-scale bias test)
	var allBase, allTier float64
	for qi := range cands {
		if baseHit10[qi] {
			allBase++
		}
		if hitIn(mergeTiered(cands[qi], tierDists(query[qi], tier), 10), int(gt[qi][0]), 10) {
			allTier++
		}
	}
	nq := float64(len(cands))

	fmt.Printf("%-7s %-5d | %-7.3f %-9.3f | %-9.4f %+-8.4f | %-8.4f %-8.4f | %-9.4f %+-9.4f\n",
		policy, cp, tierHits/Tf, float64(tier.Fetches())/Tf,
		streamRec, streamRec-baseStream, condHit, condMiss,
		allTier/nq, (allTier-allBase)/nq)
}

// tierDists scores every resident against the query with EXACT distances.
func tierDists(q []float32, tier *cache.ExactTier) map[int]float32 {
	out := map[int]float32{}
	tier.Range(func(item int, vec []float32) {
		out[item] = float32(lab.SqL2(q, vec))
	})
	return out
}

// adcSearch is the real engine path: probe cells, build tables, scan codes.
func adcSearch(q []float32, coarse [][]float32, basePQ *pq.PQ,
	members [][]int32, baseCode [][]uint8, nprobe, topK int) []pq.Result {
	var out []pq.Result
	for _, c := range lab.ProbeCells(coarse, q, nprobe) {
		table := lab.BuildADCTable(basePQ, lab.Sub(q, coarse[c]))
		for _, id := range members[c] {
			out = append(out, pq.Result{ID: int(id), Dist: lab.ADC(table, baseCode[id], basePQ.M, basePQ.K)})
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Dist != out[b].Dist {
			return out[a].Dist < out[b].Dist
		}
		return out[a].ID < out[b].ID
	})
	if len(out) > topK {
		out = out[:topK]
	}
	return out
}

// adcSearchTopK is adcSearch with a bounded top-K accumulator instead of
// append-everything + full sort — cost-faithful to the production heap path
// (ivf.Index.Search), so the timed miss path is not inflated by lab
// bookkeeping (the adversarial review measured the full-sort variant ~1.5×
// slower than the production heap at this operating point).
func adcSearchTopK(q []float32, coarse [][]float32, basePQ *pq.PQ,
	members [][]int32, baseCode [][]uint8, nprobe, topK int) []pq.Result {
	top := make([]pq.Result, 0, topK)
	for _, c := range lab.ProbeCells(coarse, q, nprobe) {
		table := lab.BuildADCTable(basePQ, lab.Sub(q, coarse[c]))
		for _, id := range members[c] {
			d := lab.ADC(table, baseCode[id], basePQ.M, basePQ.K)
			if len(top) == topK && d >= top[topK-1].Dist {
				continue
			}
			r := pq.Result{ID: int(id), Dist: d}
			if len(top) < topK {
				top = append(top, r)
			} else {
				top[topK-1] = r
			}
			for i := len(top) - 1; i > 0 && (top[i].Dist < top[i-1].Dist ||
				(top[i].Dist == top[i-1].Dist && top[i].ID < top[i-1].ID)); i-- {
				top[i], top[i-1] = top[i-1], top[i]
			}
		}
	}
	return top
}

func hitIn(rs []pq.Result, trueNN, topK int) bool {
	lim := topK
	if lim > len(rs) {
		lim = len(rs)
	}
	for j := 0; j < lim; j++ {
		if rs[j].ID == trueNN {
			return true
		}
	}
	return false
}

// latencyBench times three paths single-threaded with the production-faithful
// bounded top-K search: the query miss path (full ADC search), the tiered
// query path (search + resident rescoring — the ONLY path a query can take),
// and the resident-id lookup (the ID-ADDRESSED re-access hit path; a query
// cannot take it without searching first — do not pair it with miss µs/q).
// Tier state comes from replaying the eta-stream with the Markov policy.
func latencyBench(base, query [][]float32, coarse [][]float32, basePQ *pq.PQ,
	members [][]int32, baseCode [][]uint8, gt [][]int32,
	nprobe int, caps []int, latQ, T int, zipf float64, etas []float64,
	fetch func(int) []float32) {

	s := int64(1)
	Nq := len(query)
	eta := etas[len(etas)-1]
	for _, e := range etas {
		if e == 0.3 {
			eta = 0.3
		}
	}
	weights := refine.ZipfWeights(Nq, zipf, s+9)
	bag := buildBag(weights, 200000, s+13)
	successor := rand.New(rand.NewSource(s + 17)).Perm(Nq)
	stream := refine.SelfExcitingStream(T, eta, bag, successor, s+21)

	if latQ > T {
		latQ = T
	}
	if latQ <= 0 {
		return
	}
	fmt.Printf("-- latency (single-thread, %d queries, eta=%.2f stream, bounded top-K search) --\n", latQ, eta)
	fmt.Printf("%-5s | %-12s %-12s %-10s | %-22s\n", "cap", "miss µs/q", "tiered µs/q", "overhead", "resident-id ns (no-Q)")

	// query miss path, tier-independent
	t0 := time.Now()
	for t := 0; t < latQ; t++ {
		adcSearchTopK(query[stream[t]], coarse, basePQ, members, baseCode, nprobe, 10)
	}
	missUS := float64(time.Since(t0).Microseconds()) / float64(latQ)

	for _, cp := range caps {
		tier := cache.NewExactTier(cache.NewMarkovPrefetch(cp), fetch)
		for t := 0; t < T; t++ {
			tier.Access(int(gt[stream[t]][0]))
		}
		t1 := time.Now()
		for t := 0; t < latQ; t++ {
			q := query[stream[t]]
			rs := adcSearchTopK(q, coarse, basePQ, members, baseCode, nprobe, 10)
			mergeTiered(rs, tierDists(q, tier), 10)
		}
		tierUS := float64(time.Since(t1).Microseconds()) / float64(latQ)

		// ID-addressed re-access hit path: only valid when the caller already
		// knows the item id (journal replay / recall-by-path) — NOT a query path.
		t2 := time.Now()
		hits := 0
		for t := 0; t < latQ; t++ {
			if tier.Vec(int(gt[stream[t]][0])) != nil {
				hits++
			}
		}
		hitNS := float64(time.Since(t2).Nanoseconds()) / float64(latQ)
		sink = hits // keep the loop observable (no dead-code elimination)

		fmt.Printf("%-5d | %-12.0f %-12.0f %-10s | %-22.0f\n",
			cp, missUS, tierUS, fmt.Sprintf("%+.1f%%", (tierUS-missUS)/missUS*100), hitNS)
	}
}

// sink defeats dead-code elimination in timing loops.
var sink int

// buildBag samples `size` query indices proportional to weights (deterministic),
// forming the i.i.d. popularity bag the stream draws from. (Same as ivfpredict.)
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

func parseInts(csv string) []int {
	var out []int
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			v, err := strconv.Atoi(p)
			must(err)
			out = append(out, v)
		}
	}
	return out
}

func parseFloats(csv string) []float64 {
	var out []float64
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			f, err := strconv.ParseFloat(p, 64)
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
