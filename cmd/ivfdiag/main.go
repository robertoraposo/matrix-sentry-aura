// Command ivfdiag decomposes WHERE recall is lost in our IVFADC index on SIFT1M,
// turning the lever roadmap from hypothesis into measurement. It rebuilds the same
// geometry the ivf package produces (coarse k-means + residual PQ, same seed/opts),
// then reports six error-budget metrics with built-in kill-switches per lever:
//
//	1 oracle-cell recall   PQ-distortion ceiling (routing made perfect)
//	2 coarse-miss vs nprobe routing loss upper bound
//	3 parallel/orth error  anisotropic (ScaNN) headroom + Lever-4 kill-switch (rho-bar)
//	4 per-subspace variance water-filling bit-allocation gain
//	5 residual-variance red. why IVF>flat + LOPQ headroom (nlist vs 4*nlist)
//	6 distortion->rank lift adjudicates PQ-side vs routing-side using real misses
//
//	go build -o ivfdiag ./cmd/ivfdiag
//	./ivfdiag -dir /data/sift -prefix sift -nlist 1024 -m 8 -k 256 -nprobe 64
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"time"

	"matrixsentry/pq"
)

func main() {
	dir := flag.String("dir", "/data/sift", "dir with the .fvecs/.ivecs files")
	prefix := flag.String("prefix", "sift", "file prefix")
	nlist := flag.Int("nlist", 1024, "coarse cells")
	M := flag.Int("m", 8, "PQ subspaces")
	K := flag.Int("k", 256, "centroids per subspace")
	iter := flag.Int("iter", 25, "PQ iterations")
	coarseIter := flag.Int("coarseiter", 10, "coarse iterations")
	coarseSample := flag.Int("coarsesample", 39936, "coarse training subsample")
	pqSample := flag.Int("pqsample", 65536, "PQ training subsample")
	nprobe := flag.Int("nprobe", 64, "operating-point nprobe for metrics 2/6")
	fineNlist := flag.Int("finenlist", 4096, "finer partition for the LOPQ headroom probe")
	varSample := flag.Int("varsample", 100000, "base subsample for the fine-grid variance estimate")
	seed := int64(1)
	flag.Parse()

	fmt.Printf("== ivfdiag · %s (%d cores) ==\n", *prefix, runtime.GOMAXPROCS(0))
	base := readFvecs(fmt.Sprintf("%s/%s_base.fvecs", *dir, *prefix))
	learn := readFvecs(fmt.Sprintf("%s/%s_learn.fvecs", *dir, *prefix))
	query := readFvecs(fmt.Sprintf("%s/%s_query.fvecs", *dir, *prefix))
	gt := readIvecs(fmt.Sprintf("%s/%s_groundtruth.ivecs", *dir, *prefix))
	D := len(base[0])
	fmt.Printf("base=%d learn=%d query=%d dim=%d nlist=%d M=%d K=%d nprobe=%d\n\n",
		len(base), len(learn), len(query), D, *nlist, *M, *K, *nprobe)

	// --- geometry: identical recipe to ivf.trainResidual (seed=1) ---
	t0 := time.Now()
	coarse, q := buildGeometry(learn, *nlist, *M, *K, *iter, *coarseIter, *coarseSample, *pqSample, seed)
	fmt.Printf("geometry built (coarse+residual PQ): %v\n", time.Since(t0).Round(time.Millisecond))

	// --- precompute per-base assignment + code, invert to members ---
	t1 := time.Now()
	cell := make([]int32, len(base))
	codes := make([][]uint8, len(base))
	parallelFor(len(base), func(i int) {
		c := nearest(coarse, base[i])
		cell[i] = int32(c)
		codes[i] = q.Encode(sub(base[i], coarse[c]))
	})
	members := make([][]int32, len(coarse))
	for i := range base {
		c := cell[i]
		members[c] = append(members[c], int32(i))
	}
	fmt.Printf("assigned+encoded 1M base: %v\n\n", time.Since(t1).Round(time.Millisecond))

	metric1Oracle(base, query, gt, coarse, q, cell, codes, members, *nprobe)
	metric2CoarseMiss(query, gt, coarse, cell)
	meanErr := metric3ParOrth(base, coarse, q, cell, codes)
	totDist := metric4Subspace(base, coarse, q, cell, codes)
	fmt.Printf("-- CROSS-CHECK: metric3 mean||e||^2 = %.6f vs metric4 totDist = %.6f  (diff %.2e; must be ~0)\n\n",
		meanErr, totDist, math.Abs(meanErr-totDist))
	metric5ResidualVar(base, learn, coarse, cell, *nlist, *fineNlist, *coarseIter, *coarseSample, *varSample, seed)
	metric6DistortionRank(base, query, gt, coarse, q, cell, codes, members, *nprobe)
}

// ---------------------------------------------------------------------------
// METRIC 1 — oracle-cell recall: PQ-distortion ceiling with routing made perfect.
// ---------------------------------------------------------------------------
func metric1Oracle(base, query [][]float32, gt [][]int32, coarse [][]float32, q *pq.PQ,
	cell []int32, codes [][]uint8, members [][]int32, nprobe int) {
	fmt.Println("-- METRIC 1: oracle-cell recall (PQ ceiling) --")
	Rs := []int{1, 10, 100}
	oracle := make([]float64, len(Rs))
	actual := make([]float64, len(Rs))
	for ri, R := range Rs {
		var oHit, aHit int
		for qi := range query {
			t := int(gt[qi][0])
			cstar := cell[t]
			// oracle: rank of t scanning ONLY its true cell
			table := buildADCTable(q, sub(query[qi], coarse[cstar]))
			dt := adc(table, codes[t], q.M, q.K)
			rank := 0
			for _, id := range members[cstar] {
				if id == int32(t) {
					continue
				}
				d := adc(table, codes[id], q.M, q.K)
				if d < dt || (d == dt && id < int32(t)) {
					rank++
				}
			}
			if rank < R {
				oHit++
			}
			// actual: rank of t across the nprobe probed cells (its cell must be probed)
			if rk := ivfRank(query[qi], t, coarse, q, cell, codes, members, nprobe); rk >= 0 && rk < R {
				aHit++
			}
		}
		oracle[ri] = float64(oHit) / float64(len(query))
		actual[ri] = float64(aHit) / float64(len(query))
	}
	for ri, R := range Rs {
		fmt.Printf("  R=%-3d  oracle %.4f   actual(nprobe=%d) %.4f   gap %.4f\n",
			R, oracle[ri], nprobe, actual[ri], oracle[ri]-actual[ri])
	}
	fmt.Printf("  >> PQ ceiling = oracle@100 = %.4f. If <0.97 -> PQ caps you (build OPQ/LOPQ).\n", oracle[2])
	fmt.Printf("  >> oracle@100 - actual@100 = %.4f. If >0.02 -> routing still leaks (raise nprobe/better coarse).\n\n", oracle[2]-actual[2])
}

// ---------------------------------------------------------------------------
// METRIC 2 — coarse-miss rate vs nprobe (routing loss upper bound).
// ---------------------------------------------------------------------------
func metric2CoarseMiss(query [][]float32, gt [][]int32, coarse [][]float32, cell []int32) {
	fmt.Println("-- METRIC 2: coarse-miss vs nprobe (routing upper bound) --")
	probes := []int{1, 8, 16, 32, 64, 128, 256, 512, 1024}
	ranks := make([]int, len(query)) // rank of the true-NN cell in each query's cell ordering
	var sumRank int
	allRanks := make([]int, 0, len(query))
	for qi := range query {
		cstar := int(cell[int(gt[qi][0])])
		type cd struct {
			c int
			d float64
		}
		cds := make([]cd, len(coarse))
		for c := range coarse {
			cds[c] = cd{c, sqL2(coarse[c], query[qi])}
		}
		sort.Slice(cds, func(a, b int) bool {
			if cds[a].d != cds[b].d {
				return cds[a].d < cds[b].d
			}
			return cds[a].c < cds[b].c
		})
		r := 0
		for r < len(cds) && cds[r].c != cstar {
			r++
		}
		ranks[qi] = r
		sumRank += r
		allRanks = append(allRanks, r)
	}
	for _, p := range probes {
		miss := 0
		for _, r := range ranks {
			if r >= p {
				miss++
			}
		}
		fmt.Printf("  nprobe=%-5d coarse-miss %.4f   (recall ceiling %.4f)\n",
			p, float64(miss)/float64(len(query)), 1-float64(miss)/float64(len(query)))
	}
	sort.Ints(allRanks)
	fmt.Printf("  >> mean true-cell rank %.1f, p95 %d. recall1@R <= 1 - coarse-miss(nprobe).\n\n",
		float64(sumRank)/float64(len(query)), allRanks[len(allRanks)*95/100])
}

// ---------------------------------------------------------------------------
// METRIC 3 — parallel/orthogonal reconstruction-error split (ScaNN + Lever-4 kill).
// ---------------------------------------------------------------------------
func metric3ParOrth(base [][]float32, coarse [][]float32, q *pq.PQ, cell []int32, codes [][]uint8) float64 {
	fmt.Println("-- METRIC 3: parallel/orthogonal error split (anisotropic headroom) --")
	D := len(base[0])
	var sumParR, sumParX, sumTot, sumCosX2 float64
	var n int
	for i := range base {
		r := sub(base[i], coarse[cell[i]])
		rhat := reconstruct(q, codes[i], D)
		e := sub(r, rhat)
		tot := norm2(e)
		if tot == 0 {
			continue
		}
		// direction (a): residual r
		if nr := norm2(r); nr > 0 {
			p := dot(e, r)
			sumParR += (p * p) / nr
		}
		// direction (b): datapoint x (ScaNN definition)
		if nx := norm2(base[i]); nx > 0 {
			p := dot(e, base[i])
			sumParX += (p * p) / nx
			sumCosX2 += (p * p) / (nx * tot) // per-vector cos^2(e,x)
		}
		sumTot += tot
		n++
	}
	ratioParR := sumParR / sumTot
	ratioParX := sumParX / sumTot
	rhoBar := sumCosX2 / float64(n)
	iso := 1.0 / float64(D)
	fmt.Printf("  ratioPar(residual-dir) %.4f   ratioPar(datapoint-dir) %.4f   isotropic baseline 1/D=%.4f\n",
		ratioParR, ratioParX, iso)
	fmt.Printf("  rho-bar (mean cos^2(e,x)) %.4f   rho-bar*D %.3f   (isotropic => rho-bar*D ~ 1)\n", rhoBar, rhoBar*float64(D))
	fmt.Printf("  >> Lever-4/anisotropic: if ratioPar(datapoint) >> 3/D=%.4f -> ScaNN worth it.\n", 3*iso)
	fmt.Printf("  >> if rho-bar*D <~ 1 (isotropic) -> anisotropic gain ~0 on SIFT; reserve for Scenario B.\n\n")
	return sumTot / float64(len(base))
}

// ---------------------------------------------------------------------------
// METRIC 4 — per-subspace variance and distortion (water-filling gain).
// ---------------------------------------------------------------------------
func metric4Subspace(base [][]float32, coarse [][]float32, q *pq.PQ, cell []int32, codes [][]uint8) float64 {
	fmt.Println("-- METRIC 4: per-subspace variance / distortion (water-filling) --")
	M, Dsub := q.M, q.Dsub
	sumSq := make([]float64, M)    // E[||r_block||^2]
	sumVec := make([][]float64, M) // E[r_block]
	dist := make([]float64, M)     // D_m
	for m := 0; m < M; m++ {
		sumVec[m] = make([]float64, Dsub)
	}
	N := float64(len(base))
	for i := range base {
		r := sub(base[i], coarse[cell[i]])
		for m := 0; m < M; m++ {
			blk := r[m*Dsub : (m+1)*Dsub]
			cen := q.Codebooks[m][codes[i][m]]
			for j, v := range blk {
				sumSq[m] += float64(v) * float64(v)
				sumVec[m][j] += float64(v)
				dv := float64(v) - float64(cen[j])
				dist[m] += dv * dv
			}
		}
	}
	var totVar, totDist float64
	fmt.Printf("  %-4s %-12s %-12s %-10s\n", "m", "sigma^2_m", "D_m", "D_m/var")
	for m := 0; m < M; m++ {
		var mnorm float64
		for j := 0; j < Dsub; j++ {
			mu := sumVec[m][j] / N
			mnorm += mu * mu
		}
		varm := sumSq[m]/N - mnorm
		dm := dist[m] / N
		totVar += varm
		totDist += dm
		fmt.Printf("  %-4d %-12.4f %-12.4f %-10.4f\n", m, varm, dm, dm/varm)
	}
	fmt.Printf("  >> total var %.4f  total distortion %.4f (= mean ||e||^2).\n", totVar, totDist)
	fmt.Printf("  >> if sigma^2_m spread is wide -> non-uniform K_m (water-filling) helps; if flat -> uniform is fine.\n\n")
	return totDist
}

// ---------------------------------------------------------------------------
// METRIC 5 — residual variance reduction + LOPQ headroom probe (nlist vs 4*nlist).
// ---------------------------------------------------------------------------
func metric5ResidualVar(base, learn [][]float32, coarse [][]float32, cell []int32,
	nlist, fineNlist, coarseIter, coarseSample, varSample int, seed int64) {
	fmt.Println("-- METRIC 5: residual variance reduction + LOPQ headroom --")
	D := len(base[0])
	mu := make([]float64, D)
	for i := range base {
		for j, v := range base[i] {
			mu[j] += float64(v)
		}
	}
	muf := make([]float32, D)
	for j := range mu {
		muf[j] = float32(mu[j] / float64(len(base)))
	}
	var vRaw, vResid float64
	for i := range base {
		vRaw += sqL2(base[i], muf)
		vResid += sqL2(base[i], coarse[cell[i]])
	}
	vRaw /= float64(len(base))
	vResid /= float64(len(base))
	fmt.Printf("  V_raw %.2f   V_residual(nlist=%d) %.2f   reduction %.1f%%\n",
		vRaw, nlist, vResid, 100*(1-vResid/vRaw))

	// finer partition on a base subsample to estimate LOPQ headroom cheaply.
	// Scale the training subsample with cell count to keep points-per-cell
	// constant — otherwise the fine grid is undertrained and vFine is inflated.
	fineSample := coarseSample * fineNlist / nlist
	if fineSample > len(learn) {
		fineSample = len(learn)
	}
	fineCoarse := buildCoarse(learn, fineNlist, coarseIter, fineSample, seed)
	if varSample > len(base) {
		varSample = len(base)
	}
	var vFine float64
	for i := 0; i < varSample; i++ {
		vFine += sqL2(base[i], fineCoarse[nearest(fineCoarse, base[i])])
	}
	vFine /= float64(varSample)
	// V_residual on the same subsample at base nlist, for an apples-to-apples ratio
	var vResidSub float64
	for i := 0; i < varSample; i++ {
		vResidSub += sqL2(base[i], coarse[cell[i]])
	}
	vResidSub /= float64(varSample)
	ratio := vFine / vResidSub
	fmt.Printf("  V_residual(nlist=%d, %dk sample) %.2f   V_residual(nlist=%d) %.2f   ratio %.3f\n",
		nlist, varSample/1000, vResidSub, fineNlist, vFine, ratio)
	fmt.Printf("  >> reduction%% is WHY IVF beats flat. If 4x-cells ratio <0.85 -> LOPQ/finer cells pay off; >0.95 -> saturated.\n\n")
}

// ---------------------------------------------------------------------------
// METRIC 6 — distortion->rank lift: are misses caused by badly-quantized true NNs?
// ---------------------------------------------------------------------------
func metric6DistortionRank(base, query [][]float32, gt [][]int32, coarse [][]float32, q *pq.PQ,
	cell []int32, codes [][]uint8, members [][]int32, nprobe int) {
	fmt.Println("-- METRIC 6: distortion->rank lift (adjudicates PQ-side vs routing-side) --")
	D := len(base[0])
	type qd struct {
		miss bool
		dist float64
	}
	rows := make([]qd, len(query))
	var sumHit, sumMiss float64
	var nHit, nMiss int
	for qi := range query {
		t := int(gt[qi][0])
		rk := ivfRank(query[qi], t, coarse, q, cell, codes, members, nprobe)
		miss := !(rk >= 0 && rk < 100)
		r := sub(base[t], coarse[cell[t]])
		distT := norm2(sub(r, reconstruct(q, codes[t], D)))
		rows[qi] = qd{miss, distT}
		if miss {
			sumMiss += distT
			nMiss++
		} else {
			sumHit += distT
			nHit++
		}
	}
	meanHit := sumHit / float64(max(nHit, 1))
	meanMiss := sumMiss / float64(max(nMiss, 1))
	fmt.Printf("  hits %d  misses %d   meanDist(hit) %.4f   meanDist(miss) %.4f   lift %.3f\n",
		nHit, nMiss, meanHit, meanMiss, meanMiss/meanHit)
	// decile table: sort by distortion, miss-rate per decile
	sort.Slice(rows, func(a, b int) bool { return rows[a].dist < rows[b].dist })
	fmt.Printf("  miss-rate by true-NN-distortion decile (low->high):\n   ")
	per := len(rows) / 10
	for d := 0; d < 10; d++ {
		lo, hi := d*per, (d+1)*per
		if d == 9 {
			hi = len(rows)
		}
		m := 0
		for i := lo; i < hi; i++ {
			if rows[i].miss {
				m++
			}
		}
		fmt.Printf(" %.3f", float64(m)/float64(hi-lo))
	}
	fmt.Printf("\n  >> lift>1.5 + monotone deciles -> misses are PQ-distortion-driven (Levers 1/2 target them; Lever 3 premise holds).\n")
	fmt.Printf("  >> lift~1.0 / flat -> routing misses -> Lever 3 near-dead, pivot to LOPQ/nprobe.\n\n")
}

// ===========================================================================
// geometry + helpers (local copies; ivf/pq internals are unexported)
// ===========================================================================

func buildGeometry(learn [][]float32, nlist, M, K, iter, coarseIter, coarseSample, pqSample int, seed int64) ([][]float32, *pq.PQ) {
	d := len(learn[0])
	coarse := buildCoarse(learn, nlist, coarseIter, coarseSample, seed)
	residuals := make([][]float32, len(learn))
	for i, v := range learn {
		residuals[i] = sub(v, coarse[nearest(coarse, v)])
	}
	pqLearn := residuals
	if pqSample > 0 && pqSample < len(residuals) {
		pqLearn = subsample(residuals, pqSample, seed+2)
	}
	q, err := pq.New(d, M, K)
	must(err)
	must(q.Train(pqLearn, iter, seed))
	return coarse, q
}

func buildCoarse(learn [][]float32, nlist, coarseIter, coarseSample int, seed int64) [][]float32 {
	cl := learn
	if coarseSample > 0 && coarseSample < len(learn) {
		cl = subsample(learn, coarseSample, seed+1)
	}
	return coarseKMeans(cl, nlist, coarseIter, seed)
}

// reconstruct rebuilds a residual from its PQ code (inverse of pq.Encode).
func reconstruct(q *pq.PQ, code []uint8, D int) []float32 {
	rhat := make([]float32, D)
	for m := 0; m < q.M; m++ {
		copy(rhat[m*q.Dsub:(m+1)*q.Dsub], q.Codebooks[m][code[m]])
	}
	return rhat
}

func buildADCTable(q *pq.PQ, residual []float32) []float32 {
	table := make([]float32, q.M*q.K)
	for m := 0; m < q.M; m++ {
		start := m * q.Dsub
		sv := residual[start : start+q.Dsub]
		cb := q.Codebooks[m]
		bse := m * q.K
		for c := 0; c < q.K; c++ {
			table[bse+c] = float32(sqL2(sv, cb[c]))
		}
	}
	return table
}

func adc(table []float32, code []uint8, M, K int) float32 {
	var d float32
	for m := 0; m < M; m++ {
		d += table[m*K+int(code[m])]
	}
	return d
}

// ivfRank returns the global rank of base item t among all items in the nprobe
// nearest cells of query (0 = nearest). Returns -1 if t's cell is not probed.
func ivfRank(query []float32, t int, coarse [][]float32, q *pq.PQ,
	cell []int32, codes [][]uint8, members [][]int32, nprobe int) int {
	probed := probeCells(coarse, query, nprobe)
	ctOK := false
	for _, c := range probed {
		if int32(c) == cell[t] {
			ctOK = true
			break
		}
	}
	if !ctOK {
		return -1
	}
	tableT := buildADCTable(q, sub(query, coarse[cell[t]]))
	dt := adc(tableT, codes[t], q.M, q.K)
	rank := 0
	for _, c := range probed {
		table := buildADCTable(q, sub(query, coarse[c]))
		for _, id := range members[c] {
			if id == int32(t) {
				continue
			}
			d := adc(table, codes[id], q.M, q.K)
			if d < dt || (d == dt && id < int32(t)) {
				rank++
			}
		}
	}
	return rank
}

func probeCells(coarse [][]float32, query []float32, nprobe int) []int {
	type cd struct {
		c int
		d float64
	}
	cds := make([]cd, len(coarse))
	for c := range coarse {
		cds[c] = cd{c, sqL2(coarse[c], query)}
	}
	sort.Slice(cds, func(a, b int) bool {
		if cds[a].d != cds[b].d {
			return cds[a].d < cds[b].d
		}
		return cds[a].c < cds[b].c
	})
	if nprobe > len(cds) {
		nprobe = len(cds)
	}
	out := make([]int, nprobe)
	for i := 0; i < nprobe; i++ {
		out[i] = cds[i].c
	}
	return out
}

// --- frozen-engine math, copied verbatim so metrics match the engine exactly ---

func sqL2(a, b []float32) float64 {
	var s float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		s += d * d
	}
	return s
}

func sub(a, b []float32) []float32 {
	out := make([]float32, len(a))
	for i := range a {
		out[i] = a[i] - b[i]
	}
	return out
}

func nearest(centroids [][]float32, v []float32) int {
	best, bestD := 0, math.MaxFloat64
	for c, cen := range centroids {
		if d := sqL2(cen, v); d < bestD {
			bestD, best = d, c
		}
	}
	return best
}

func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

func norm2(a []float32) float64 {
	var s float64
	for _, v := range a {
		s += float64(v) * float64(v)
	}
	return s
}

func cloneF32(v []float32) []float32 {
	c := make([]float32, len(v))
	copy(c, v)
	return c
}

func subsample(points [][]float32, m int, seed int64) [][]float32 {
	n := len(points)
	if m >= n {
		return points
	}
	rng := rand.New(rand.NewSource(seed))
	reservoir := make([][]float32, m)
	copy(reservoir, points[:m])
	for i := m; i < n; i++ {
		if j := rng.Intn(i + 1); j < m {
			reservoir[j] = points[i]
		}
	}
	return reservoir
}

func coarseKMeans(points [][]float32, k, maxIter int, seed int64) [][]float32 {
	n := len(points)
	if n == 0 {
		return nil
	}
	dim := len(points[0])
	if k > n {
		k = n
	}
	rng := rand.New(rand.NewSource(seed))
	centroids := kmeansppInit(points, k, rng)
	assign := make([]int, n)
	for iter := 0; iter < maxIter; iter++ {
		changed := false
		for i, p := range points {
			b := nearest(centroids, p)
			if assign[i] != b {
				assign[i], changed = b, true
			}
		}
		sums := make([][]float64, k)
		counts := make([]int, k)
		for c := range sums {
			sums[c] = make([]float64, dim)
		}
		for i, p := range points {
			c := assign[i]
			counts[c]++
			for j := 0; j < dim; j++ {
				sums[c][j] += float64(p[j])
			}
		}
		for c := 0; c < k; c++ {
			if counts[c] == 0 {
				centroids[c] = cloneF32(points[rng.Intn(n)])
				continue
			}
			inv := 1.0 / float64(counts[c])
			for j := 0; j < dim; j++ {
				centroids[c][j] = float32(sums[c][j] * inv)
			}
		}
		if !changed && iter > 0 {
			break
		}
	}
	return centroids
}

func kmeansppInit(points [][]float32, k int, rng *rand.Rand) [][]float32 {
	n := len(points)
	centroids := make([][]float32, 0, k)
	first := cloneF32(points[rng.Intn(n)])
	centroids = append(centroids, first)
	d2 := make([]float64, n)
	var sum float64
	for i, p := range points {
		d2[i] = sqL2(p, first)
		sum += d2[i]
	}
	for len(centroids) < k {
		if sum == 0 {
			centroids = append(centroids, cloneF32(points[rng.Intn(n)]))
			continue
		}
		target := rng.Float64() * sum
		idx, acc := n-1, 0.0
		for i := 0; i < n; i++ {
			if acc += d2[i]; acc >= target {
				idx = i
				break
			}
		}
		nc := cloneF32(points[idx])
		centroids = append(centroids, nc)
		sum = 0
		for i, p := range points {
			if dd := sqL2(p, nc); dd < d2[i] {
				d2[i] = dd
			}
			sum += d2[i]
		}
	}
	return centroids
}

// --- small utilities ---

func parallelFor(n int, fn func(i int)) {
	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	chunk := (n + workers - 1) / workers
	done := make(chan struct{}, workers)
	started := 0
	for w := 0; w < workers; w++ {
		lo := w * chunk
		if lo >= n {
			break
		}
		hi := lo + chunk
		if hi > n {
			hi = n
		}
		started++
		go func(lo, hi int) {
			for i := lo; i < hi; i++ {
				fn(i)
			}
			done <- struct{}{}
		}(lo, hi)
	}
	for i := 0; i < started; i++ {
		<-done
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func readFvecs(path string) [][]float32 {
	raw, err := os.ReadFile(path)
	must(err)
	var out [][]float32
	for off := 0; off < len(raw); {
		dim := int(binary.LittleEndian.Uint32(raw[off:]))
		off += 4
		v := make([]float32, dim)
		for j := 0; j < dim; j++ {
			v[j] = math.Float32frombits(binary.LittleEndian.Uint32(raw[off:]))
			off += 4
		}
		out = append(out, v)
	}
	return out
}

func readIvecs(path string) [][]int32 {
	raw, err := os.ReadFile(path)
	must(err)
	var out [][]int32
	for off := 0; off < len(raw); {
		dim := int(binary.LittleEndian.Uint32(raw[off:]))
		off += 4
		v := make([]int32, dim)
		for j := 0; j < dim; j++ {
			v[j] = int32(binary.LittleEndian.Uint32(raw[off:]))
			off += 4
		}
		out = append(out, v)
	}
	return out
}
