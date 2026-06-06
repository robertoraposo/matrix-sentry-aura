// Package lab holds shared, verified-bit-identical copies of the frozen engine's
// math (k-means, residual geometry, ADC, PQ reconstruction) plus TEXMEX loaders,
// so experiment commands (ivfdiag, ivfrefine, ...) reuse one source of truth
// instead of each carrying its own copy. It deliberately reimplements the ivf/pq
// internals (which are unexported) rather than widening their production API.
package lab

import (
	"encoding/binary"
	"math"
	"math/rand"
	"os"
	"runtime"
	"sort"

	"matrixsentry/pq"
)

// --- TEXMEX loaders (verbatim from cmd/ivf1m) ---

func ReadFvecs(path string) [][]float32 {
	raw, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
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

func ReadIvecs(path string) [][]int32 {
	raw, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
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

// --- frozen-engine math (verified identical to ivf/pq internals) ---

func SqL2(a, b []float32) float64 {
	var s float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		s += d * d
	}
	return s
}

func Sub(a, b []float32) []float32 {
	out := make([]float32, len(a))
	for i := range a {
		out[i] = a[i] - b[i]
	}
	return out
}

// Add returns a+b as a new slice.
func Add(a, b []float32) []float32 {
	out := make([]float32, len(a))
	for i := range a {
		out[i] = a[i] + b[i]
	}
	return out
}

func Nearest(centroids [][]float32, v []float32) int {
	best, bestD := 0, math.MaxFloat64
	for c, cen := range centroids {
		if d := SqL2(cen, v); d < bestD {
			bestD, best = d, c
		}
	}
	return best
}

func cloneF32(v []float32) []float32 {
	c := make([]float32, len(v))
	copy(c, v)
	return c
}

func Subsample(points [][]float32, m int, seed int64) [][]float32 {
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

func CoarseKMeans(points [][]float32, k, maxIter int, seed int64) [][]float32 {
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
			b := Nearest(centroids, p)
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
		d2[i] = SqL2(p, first)
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
			if dd := SqL2(p, nc); dd < d2[i] {
				d2[i] = dd
			}
			sum += d2[i]
		}
	}
	return centroids
}

// ProbeCells returns the nprobe nearest coarse cells to query, nearest first,
// ties to lower index.
func ProbeCells(coarse [][]float32, query []float32, nprobe int) []int {
	type cd struct {
		c int
		d float64
	}
	cds := make([]cd, len(coarse))
	for c := range coarse {
		cds[c] = cd{c, SqL2(coarse[c], query)}
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

// BuildADCTable builds the per-cell ADC table from a residual.
func BuildADCTable(q *pq.PQ, residual []float32) []float32 {
	table := make([]float32, q.M*q.K)
	for m := 0; m < q.M; m++ {
		start := m * q.Dsub
		sv := residual[start : start+q.Dsub]
		cb := q.Codebooks[m]
		base := m * q.K
		for c := 0; c < q.K; c++ {
			table[base+c] = float32(SqL2(sv, cb[c]))
		}
	}
	return table
}

// ADC sums a code's per-subspace table lookups.
func ADC(table []float32, code []uint8, M, K int) float32 {
	var d float32
	for m := 0; m < M; m++ {
		d += table[m*K+int(code[m])]
	}
	return d
}

// Reconstruct rebuilds a vector from its PQ code (inverse of pq.Encode): the
// concatenation of the chosen sub-centroids.
func Reconstruct(q *pq.PQ, code []uint8) []float32 {
	out := make([]float32, q.D)
	for m := 0; m < q.M; m++ {
		copy(out[m*q.Dsub:(m+1)*q.Dsub], q.Codebooks[m][code[m]])
	}
	return out
}

// BuildGeometry trains the coarse quantizer + residual PQ exactly as
// ivf.trainResidual does (FAISS-subsampled), returning both for inspection.
func BuildGeometry(learn [][]float32, nlist, M, K, iter, coarseIter, coarseSample, pqSample int, seed int64) ([][]float32, *pq.PQ) {
	d := len(learn[0])
	cl := learn
	if coarseSample > 0 && coarseSample < len(learn) {
		cl = Subsample(learn, coarseSample, seed+1)
	}
	coarse := CoarseKMeans(cl, nlist, coarseIter, seed)
	residuals := make([][]float32, len(learn))
	for i, v := range learn {
		residuals[i] = Sub(v, coarse[Nearest(coarse, v)])
	}
	pqLearn := residuals
	if pqSample > 0 && pqSample < len(residuals) {
		pqLearn = Subsample(residuals, pqSample, seed+2)
	}
	q, err := pq.New(d, M, K)
	if err != nil {
		panic(err)
	}
	if err := q.Train(pqLearn, iter, seed); err != nil {
		panic(err)
	}
	return coarse, q
}

// ParallelFor runs fn(i) for i in [0,n) across all cores.
func ParallelFor(n int, fn func(i int)) {
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
