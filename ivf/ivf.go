package ivf

import (
	"errors"
	"math/rand"
	"runtime"
	"sync"

	"matrixsentry/pq"
)

// IVFADC is an inverted-file index with residual Product Quantization. The coarse
// quantizer partitions the space into len(coarse) cells; each stored vector is
// encoded as the PQ code of its residual against its cell centroid. Search probes
// only the nprobe nearest cells instead of scanning every code.
type IVFADC struct {
	coarse [][]float32  // nlist centroids, dim D
	pq     *pq.PQ       // trained on residuals
	lists  [][]int32    // lists[c] = stored IDs assigned to cell c
	codes  [][][]uint8  // codes[c][j] = PQ code of residual of lists[c][j]
	ntotal int          // running count of added vectors (assigns stable IDs)
}

// TrainOpts tunes training-set subsampling and coarse iterations, FAISS-style.
// A zero value means "no subsampling / use the iter argument", so the zero
// TrainOpts reproduces plain Train exactly. Subsampling the *training* set is
// safe for recall because k-means centroids converge with far fewer points than
// the full corpus; only the index build (Add) must see every vector.
type TrainOpts struct {
	CoarseIter   int // k-means iterations for the coarse quantizer (0 → iter). FAISS uses 10 for IVF.
	CoarseSample int // learn vectors used to train the coarse quantizer (0 → all). FAISS rule: ~39*nlist.
	PQSample     int // residual vectors used to train the PQ (0 → all). FAISS caps at K*256.
}

// Train learns the coarse quantizer and the residual PQ from a learn set. The
// coarse cells come from full-vector k-means; the PQ is trained on the residuals
// of the learn vectors against their assigned cells (pq stays untouched — it just
// receives residuals instead of raw vectors).
func Train(learn [][]float32, nlist, M, K, iter int, seed int64) (*IVFADC, error) {
	return TrainWithOpts(learn, nlist, M, K, iter, seed, TrainOpts{})
}

// TrainWithOpts is Train with optional subsampling. The coarse quantizer trains
// on CoarseSample vectors for CoarseIter iterations; the residual PQ trains on
// PQSample residuals. Each subsample is drawn with its own RNG (seed+1, seed+2)
// so the coarse k-means centroid stream is left exactly as plain Train sees it,
// keeping the cross-core determinism guarantee intact.
func TrainWithOpts(learn [][]float32, nlist, M, K, iter int, seed int64, opts TrainOpts) (*IVFADC, error) {
	if len(learn) == 0 {
		return nil, errors.New("ivf: empty learn set")
	}
	coarse, q, err := trainResidual(learn, nlist, M, K, iter, seed, opts)
	if err != nil {
		return nil, err
	}
	nl := len(coarse)
	return &IVFADC{
		coarse: coarse,
		pq:     q,
		lists:  make([][]int32, nl),
		codes:  make([][][]uint8, nl),
	}, nil
}

// subsample returns m vectors drawn from points using reservoir sampling (one
// O(n) pass) with a fixed seed, so the draw is identical run-to-run and across
// core counts. It keeps the original vector references (no copies) and returns
// the input untouched when m >= len(points).
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

// Add assigns each base vector to its nearest cell and stores the PQ code of its
// residual. IDs are assigned sequentially across calls, so the first vector ever
// added is ID 0. Encoding fans out across cores; the assignment of an ID to a cell
// is independent of scheduling, so the index is identical regardless of core count.
func (ix *IVFADC) Add(base [][]float32) {
	n := len(base)
	cells := make([]int, n)
	codes := make([][]uint8, n)

	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	chunk := (n + workers - 1) / workers
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo := w * chunk
		if lo >= n {
			break
		}
		hi := lo + chunk
		if hi > n {
			hi = n
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			for i := lo; i < hi; i++ {
				c := nearest(ix.coarse, base[i])
				cells[i] = c
				codes[i] = ix.pq.Encode(sub(base[i], ix.coarse[c]))
			}
		}(lo, hi)
	}
	wg.Wait()

	// Append in input order so list contents are deterministic.
	for i := 0; i < n; i++ {
		c := cells[i]
		ix.lists[c] = append(ix.lists[c], int32(ix.ntotal+i))
		ix.codes[c] = append(ix.codes[c], codes[i])
	}
	ix.ntotal += n
}

// Search returns the topK nearest stored IDs to query, scanning only the nprobe
// nearest cells. Each probed cell gets its own ADC table built from the query's
// residual against that cell's centroid.
func (ix *IVFADC) Search(query []float32, nprobe, topK int) []pq.Result {
	cells := ix.probe(query, nprobe)

	h := &maxHeap{}
	for _, c := range cells {
		table := ix.adcTable(sub(query, ix.coarse[c]))
		ids := ix.lists[c]
		for j, code := range ix.codes[c] {
			var d float32
			for m := 0; m < ix.pq.M; m++ {
				d += table[m*ix.pq.K+int(code[m])]
			}
			r := pq.Result{ID: int(ids[j]), Dist: d}
			if h.Len() < topK {
				h.push(r)
			} else if d < (*h)[0].Dist {
				h.replaceTop(r)
			}
		}
	}
	return h.drainAscending()
}

// SearchBatch runs many queries across all cores. It is read-only on the index
// and allocates its own scratch per query, so results are identical to a serial
// run regardless of core count.
func (ix *IVFADC) SearchBatch(queries [][]float32, nprobe, topK int) [][]pq.Result {
	out := make([][]pq.Result, len(queries))
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
				out[i] = ix.Search(queries[i], nprobe, topK)
			}
		}()
	}
	wg.Wait()
	return out
}

// Scanned reports how many stored vectors live in the nprobe nearest cells of
// query — the honest cost of an IVF search, used to report scan fraction.
func (ix *IVFADC) Scanned(query []float32, nprobe int) int {
	total := 0
	for _, c := range ix.probe(query, nprobe) {
		total += len(ix.lists[c])
	}
	return total
}

// Ntotal is the number of vectors stored in the index.
func (ix *IVFADC) Ntotal() int { return ix.ntotal }

// probe returns the indices of the nprobe nearest cells to query, nearest first.
// Ties resolve to the lower cell index to keep search deterministic.
func (ix *IVFADC) probe(query []float32, nprobe int) []int {
	return probeCells(ix.coarse, query, nprobe)
}

// adcTable builds the per-cell ADC lookup table from a residual.
func (ix *IVFADC) adcTable(residual []float32) []float32 {
	return buildADCTable(ix.pq, residual)
}

// sub returns a - b as a new slice.
func sub(a, b []float32) []float32 {
	out := make([]float32, len(a))
	for i := range a {
		out[i] = a[i] - b[i]
	}
	return out
}
