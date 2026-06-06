// Package pq implements Product Quantization from scratch — no ML libraries.
//
// Idea: a D-dimensional vector is split into M contiguous subvectors of length
// D/M. Each subspace gets its own codebook of K centroids learned by k-means.
// To encode a vector we replace each subvector by the index of its nearest
// centroid. With K<=256 that index is one byte, so the whole vector collapses
// from D*4 bytes (float32) to M bytes. At D=768, M=96, K=256 that's a 32x cut.
//
// Search uses ADC (Asymmetric Distance Computation): we keep the query in full
// float precision and precompute, once per query, the distance from each of its
// subvectors to all K centroids (an M x K table). Then every database vector is
// scored by M table lookups + M adds — no multiplications in the inner loop.
package pq

import (
	"encoding/gob"
	"errors"
	"io"
	"math"
	"math/rand"
	"runtime"
	"sort"
	"sync"
)

// PQ holds the learned codebooks and the geometry of the quantizer.
type PQ struct {
	D    int // full vector dimension
	M    int // number of subspaces
	K    int // centroids per subspace (must be <= 256 to fit a byte)
	Dsub int // D / M, dimension of each subvector

	// Codebooks[m][c] is centroid c of subspace m, length Dsub.
	Codebooks [][][]float32
}

// New validates geometry and returns an untrained quantizer.
func New(d, m, k int) (*PQ, error) {
	if m <= 0 || d%m != 0 {
		return nil, errors.New("pq: D must be a positive multiple of M")
	}
	if k <= 0 || k > 256 {
		return nil, errors.New("pq: K must be in 1..256 to fit one byte per code")
	}
	return &PQ{D: d, M: m, K: k, Dsub: d / m}, nil
}

// Train learns every subspace codebook from the training set (n x D).
// vecs should already be L2-normalized if you intend cosine-style recall.
//
// The M subspaces are independent, so training runs them across all CPUs. Each
// subspace gets its own deterministic RNG (seed+m), so the learned codebooks are
// identical regardless of how many workers run — parallelism never changes the
// result, it only changes the wall-clock time.
func (q *PQ) Train(vecs [][]float32, maxIter int, seed int64) error {
	if len(vecs) == 0 {
		return errors.New("pq: empty training set")
	}
	if len(vecs[0]) != q.D {
		return errors.New("pq: training dimension mismatch")
	}
	q.Codebooks = make([][][]float32, q.M)

	workers := runtime.GOMAXPROCS(0)
	if workers > q.M {
		workers = q.M
	}
	jobs := make(chan int, q.M)
	for m := 0; m < q.M; m++ {
		jobs <- m
	}
	close(jobs)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for m := range jobs {
				start := m * q.Dsub
				sub := make([][]float32, len(vecs))
				for i, v := range vecs {
					sub[i] = v[start : start+q.Dsub] // view, no copy
				}
				rng := rand.New(rand.NewSource(seed + int64(m) + 1))
				q.Codebooks[m] = kmeans(sub, q.K, maxIter, rng) // distinct index per goroutine
			}
		}()
	}
	wg.Wait()
	return nil
}

// Encode quantizes one vector into M bytes (one centroid index per subspace).
func (q *PQ) Encode(v []float32) []uint8 {
	code := make([]uint8, q.M)
	for m := 0; m < q.M; m++ {
		start := m * q.Dsub
		sv := v[start : start+q.Dsub]
		best, bestD := 0, math.MaxFloat64
		cb := q.Codebooks[m]
		for c := 0; c < q.K; c++ {
			if d := sqL2(sv, cb[c]); d < bestD {
				bestD, best = d, c
			}
		}
		code[m] = uint8(best)
	}
	return code
}

// EncodeBatch quantizes a whole set; output[i] corresponds to vecs[i].
// Vectors are independent, so encoding fans out across all CPUs.
func (q *PQ) EncodeBatch(vecs [][]float32) [][]uint8 {
	out := make([][]uint8, len(vecs))
	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	chunk := (len(vecs) + workers - 1) / workers
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo := w * chunk
		if lo >= len(vecs) {
			break
		}
		hi := lo + chunk
		if hi > len(vecs) {
			hi = len(vecs)
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			for i := lo; i < hi; i++ {
				out[i] = q.Encode(vecs[i])
			}
		}(lo, hi)
	}
	wg.Wait()
	return out
}

// distanceTable builds the per-query lookup table used by ADC.
// table[m*K + c] = squared distance from query subvector m to centroid c.
func (q *PQ) distanceTable(query []float32) []float32 {
	table := make([]float32, q.M*q.K)
	for m := 0; m < q.M; m++ {
		start := m * q.Dsub
		sq := query[start : start+q.Dsub]
		cb := q.Codebooks[m]
		base := m * q.K
		for c := 0; c < q.K; c++ {
			table[base+c] = float32(sqL2(sq, cb[c]))
		}
	}
	return table
}

// Result is a single search hit. Dist is approximate squared-L2 distance.
type Result struct {
	ID   int
	Dist float32
}

// Search returns the topK nearest codes to query under ADC. The inner loop over
// the whole database is pure additions — that's where the speed comes from.
func (q *PQ) Search(query []float32, codes [][]uint8, topK int) []Result {
	table := q.distanceTable(query)

	// Bounded selection with a max-heap of size topK: O(n log k), not O(n log n).
	h := &maxHeap{}
	for i, code := range codes {
		var d float32
		for m := 0; m < q.M; m++ {
			d += table[m*q.K+int(code[m])]
		}
		if h.Len() < topK {
			h.push(Result{ID: i, Dist: d})
		} else if d < (*h)[0].Dist {
			h.replaceTop(Result{ID: i, Dist: d})
		}
	}
	out := h.drainAscending()
	return out
}

// SearchBatch runs many queries across all CPUs. Search is read-only on the
// codebooks and codes and allocates its own scratch, so it is safe to run
// concurrently. out[i] holds the topK results for queries[i].
func (q *PQ) SearchBatch(queries [][]float32, codes [][]uint8, topK int) [][]Result {
	out := make([][]Result, len(queries))
	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan int, len(queries))
	for i := range queries {
		jobs <- i
	}
	close(jobs)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				out[i] = q.Search(queries[i], codes, topK)
			}
		}()
	}
	wg.Wait()
	return out
}

// Save serializes the quantizer (geometry + codebooks) via gob.
func (q *PQ) Save(w io.Writer) error { return gob.NewEncoder(w).Encode(q) }

// Load reconstructs a quantizer written by Save.
func Load(r io.Reader) (*PQ, error) {
	var q PQ
	if err := gob.NewDecoder(r).Decode(&q); err != nil {
		return nil, err
	}
	return &q, nil
}

// --- minimal max-heap over Result, keyed by Dist (largest at root) ---

type maxHeap []Result

func (h maxHeap) Len() int { return len(h) }

func (h *maxHeap) push(r Result) {
	*h = append(*h, r)
	i := len(*h) - 1
	for i > 0 {
		p := (i - 1) / 2
		if (*h)[p].Dist >= (*h)[i].Dist {
			break
		}
		(*h)[p], (*h)[i] = (*h)[i], (*h)[p]
		i = p
	}
}

func (h *maxHeap) replaceTop(r Result) {
	(*h)[0] = r
	n, i := len(*h), 0
	for {
		l, ri, largest := 2*i+1, 2*i+2, i
		if l < n && (*h)[l].Dist > (*h)[largest].Dist {
			largest = l
		}
		if ri < n && (*h)[ri].Dist > (*h)[largest].Dist {
			largest = ri
		}
		if largest == i {
			break
		}
		(*h)[i], (*h)[largest] = (*h)[largest], (*h)[i]
		i = largest
	}
}

func (h *maxHeap) drainAscending() []Result {
	out := make([]Result, len(*h))
	copy(out, *h)
	sort.Slice(out, func(a, b int) bool { return out[a].Dist < out[b].Dist })
	return out
}
