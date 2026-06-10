package ivf

import (
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"

	"matrixsentry/pq"
)

// Config fully specifies an Index. Train carries the FAISS-style subsampling
// knobs (build-time fix); its zero value trains on the full learn set.
type Config struct {
	Dim   int
	Nlist int
	M     int
	K     int
	Iter  int
	Seed  int64
	Train TrainOpts
}

// Handle is a vector's intrinsic identity: the exact content hash (primary key),
// its coarse cell, and its PQ code (quantized fingerprint). It is what Search and
// Recall return; the caller maps Handle.Hash to its own decision record.
type Handle struct {
	Hash uint64
	Cell uint32
	Code []uint8
}

// Index is a content-addressed IVFADC index. entries[pos] is the stored vector's
// Handle; lists[c] is the inverted file of positions in cell c. byHash/byCode are
// derived dedup maps, rebuilt on Load (never serialized).
type Index struct {
	cfg     Config
	coarse  [][]float32
	pq      *pq.PQ
	entries []Handle
	lists   [][]int32
	byHash  map[uint64]int32
	byCode  map[string]int32
}

// New validates cfg and returns an untrained Index.
func New(cfg Config) (*Index, error) {
	switch {
	case cfg.Dim <= 0:
		return nil, errors.New("ivf: Dim must be > 0")
	case cfg.M <= 0:
		return nil, errors.New("ivf: M must be > 0")
	case cfg.Dim%cfg.M != 0:
		return nil, fmt.Errorf("ivf: Dim (%d) must be divisible by M (%d)", cfg.Dim, cfg.M)
	case cfg.K <= 0 || cfg.K > 256:
		return nil, fmt.Errorf("ivf: K must be in 1..256, got %d", cfg.K)
	case cfg.Nlist <= 0:
		return nil, errors.New("ivf: Nlist must be > 0")
	case cfg.Iter <= 0:
		return nil, errors.New("ivf: Iter must be > 0")
	}
	return &Index{
		cfg:    cfg,
		byHash: make(map[uint64]int32),
		byCode: make(map[string]int32),
	}, nil
}

// Ntotal is the number of distinct vectors stored.
func (ix *Index) Ntotal() int { return len(ix.entries) }

// Train learns the coarse quantizer and the residual PQ from a learn set, using
// the FAISS-style subsampling in cfg.Train. Safe to call once before Add.
func (ix *Index) Train(learn [][]float32) error {
	if len(learn) == 0 {
		return errors.New("ivf: empty learn set")
	}
	coarse, q, err := trainResidual(learn, ix.cfg.Nlist, ix.cfg.M, ix.cfg.K, ix.cfg.Iter, ix.cfg.Seed, ix.cfg.Train)
	if err != nil {
		return err
	}
	ix.coarse = coarse
	ix.pq = q
	ix.lists = make([][]int32, len(coarse))
	return nil
}

// Status reports what Add did with a vector.
type Status int

const (
	Inserted       Status = iota // newly stored
	ExactDuplicate               // identical content hash already present; not stored
)

func (s Status) String() string {
	if s == ExactDuplicate {
		return "ExactDuplicate"
	}
	return "Inserted"
}

// AddResult is the per-vector outcome of Add.
type AddResult struct {
	Handle Handle
	Status Status
	Prior  uint64 // when ExactDuplicate, the Hash of the already-stored vector
}

// Add encodes each vector to its cell + residual PQ code + content hash, then
// stores it unless its exact hash is already present. Encoding fans out across
// cores (read-only); appending and dedup happen serially in input order, so the
// resulting index — and every AddResult — is identical regardless of core count.
func (ix *Index) Add(vecs [][]float32) []AddResult {
	n := len(vecs)
	hs := make([]Handle, n)

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
				c := nearest(ix.coarse, vecs[i])
				hs[i] = Handle{
					Hash: hashVec(vecs[i]),
					Cell: uint32(c),
					Code: ix.pq.Encode(sub(vecs[i], ix.coarse[c])),
				}
			}
		}(lo, hi)
	}
	wg.Wait()

	out := make([]AddResult, n)
	for i := 0; i < n; i++ {
		h := hs[i]
		if pos, ok := ix.byHash[h.Hash]; ok {
			out[i] = AddResult{Handle: ix.entries[pos], Status: ExactDuplicate, Prior: h.Hash}
			continue
		}
		pos := int32(len(ix.entries))
		ix.entries = append(ix.entries, h)
		ix.lists[h.Cell] = append(ix.lists[h.Cell], pos)
		ix.byHash[h.Hash] = pos
		ix.byCode[codeKey(h.Cell, h.Code)] = pos
		out[i] = AddResult{Handle: h, Status: Inserted}
	}
	return out
}

// Hit is a search result: the matched vector's Handle and its ADC distance.
type Hit struct {
	Handle Handle
	Dist   float32
}

// Search returns the topK nearest stored vectors to query, scanning only the
// nprobe nearest cells. Each probed cell builds its own ADC table from the
// query's residual against that cell centroid.
func (ix *Index) Search(query []float32, nprobe, topK int) []Hit {
	h := &maxHeap{}
	for _, c := range probeCells(ix.coarse, query, nprobe) {
		table := buildADCTable(ix.pq, sub(query, ix.coarse[c]))
		for _, pos := range ix.lists[c] {
			code := ix.entries[pos].Code
			var d float32
			for m := 0; m < ix.pq.M; m++ {
				d += table[m*ix.pq.K+int(code[m])]
			}
			r := pq.Result{ID: int(pos), Dist: d}
			if h.Len() < topK {
				h.push(r)
			} else if d < (*h)[0].Dist {
				h.replaceTop(r)
			}
		}
	}
	ranked := h.drainAscending()
	out := make([]Hit, len(ranked))
	for i, r := range ranked {
		out[i] = Hit{Handle: ix.entries[r.ID], Dist: r.Dist}
	}
	return out
}

// SearchTiered is Search with an EXACT TIER: tier maps Handle.Hash to the
// item's uncompressed vector (the access-driven cache keeps hot/predicted
// items at full precision). Residents are scored with EXACT distances —
// replacing their ADC estimate — and are candidates regardless of cell
// routing, so a cache hit cannot be lost to a coarse-quantizer miss or PQ
// distortion: hits are recall-perfect. Hashes not present in the index are
// ignored (the tier mirrors indexed items). Results are re-ranked by distance
// (ties to lower Hash) and truncated to topK; with an empty tier the result is
// exactly Search.
func (ix *Index) SearchTiered(query []float32, nprobe, topK int, tier map[uint64][]float32) []Hit {
	if len(tier) == 0 {
		return ix.Search(query, nprobe, topK)
	}
	// Gather topK+len(tier) ADC candidates: in the worst case every resident
	// occupies a top-K slot and is demoted by its exact distance, and each
	// vacated slot must backfill from the next ADC rank.
	adc := ix.Search(query, nprobe, topK+len(tier))
	merged := make([]Hit, 0, len(adc)+len(tier))
	for _, h := range adc {
		if _, ok := tier[h.Handle.Hash]; ok {
			continue // replaced by its exact entry below
		}
		merged = append(merged, h)
	}
	for hash, vec := range tier {
		pos, ok := ix.byHash[hash]
		if !ok {
			continue
		}
		merged = append(merged, Hit{Handle: ix.entries[pos], Dist: float32(sqL2(query, vec))})
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Dist != merged[j].Dist {
			return merged[i].Dist < merged[j].Dist
		}
		return merged[i].Handle.Hash < merged[j].Handle.Hash
	})
	if len(merged) > topK {
		merged = merged[:topK]
	}
	return merged
}

// Recall answers "do I already know something like this?" at resolution tol:
// it finds the stored vector in v's cell whose PQ code is within Hamming tol of
// v's code, returning the closest by ADC distance. tol=0 requires an identical
// code; larger tol widens to a semantic neighborhood. Returns false if none.
func (ix *Index) Recall(v []float32, tol int) (Handle, bool) {
	c := nearest(ix.coarse, v)
	residual := sub(v, ix.coarse[c])
	code := ix.pq.Encode(residual)
	table := buildADCTable(ix.pq, residual)

	bestPos := int32(-1)
	var bestDist float32
	for _, pos := range ix.lists[c] {
		e := ix.entries[pos]
		if hammingBytes(code, e.Code) > tol {
			continue
		}
		var d float32
		for m := 0; m < ix.pq.M; m++ {
			d += table[m*ix.pq.K+int(e.Code[m])]
		}
		if bestPos == -1 || d < bestDist {
			bestPos, bestDist = pos, d
		}
	}
	if bestPos == -1 {
		return Handle{}, false
	}
	return ix.entries[bestPos], true
}

// SearchBatch runs many queries across all cores; read-only, so results are
// identical to a serial run regardless of core count.
func (ix *Index) SearchBatch(queries [][]float32, nprobe, topK int) [][]Hit {
	out := make([][]Hit, len(queries))
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
