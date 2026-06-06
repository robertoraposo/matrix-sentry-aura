// Command pqdemo exercises the hand-written Product Quantizer on synthetic
// embeddings shaped like real ones (D=768, clustered, L2-normalized) and reports
// the only numbers that matter: recall@10 vs exact search, query latency, and
// the compression ratio. No external data, no ML libraries — just the math.
package main

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"

	"matrixsentry/pq"
)

const (
	N      = 5000 // database vectors
	NQ     = 60   // queries
	D      = 768  // embedding dimension (nomic-embed-text)
	M      = 96   // subspaces -> 96 bytes per code
	K      = 64   // centroids per subspace -> fits in a uint8
	TopK   = 10
	NTrain = 5000
	Iter   = 12
	Rank   = 16 // intrinsic latent dimension of the synthetic manifold
)

func main() {
	rng := rand.New(rand.NewSource(42))

	fmt.Println("== Matrix Sentry · Product Quantization (hand-built, Go puro) ==")
	fmt.Printf("N=%d  D=%d  M=%d  K=%d  topK=%d\n\n", N, D, M, K, TopK)

	// One shared set of latent centers. Both the corpus and the queries are
	// sampled from it, so a query genuinely has near neighbors in the corpus —
	// otherwise "recall" would just measure noise.
	centers := makeCenters(Rank, D, rng)

	data := sampleFrom(centers, N, 0.10, rng)
	for _, v := range data {
		normalize(v)
	}

	// Train on a random subset (PQ never needs the full corpus to learn codebooks).
	train := make([][]float32, NTrain)
	for i := range train {
		train[i] = data[rng.Intn(N)]
	}

	q, err := pq.New(D, M, K)
	if err != nil {
		panic(err)
	}

	t0 := time.Now()
	if err := q.Train(train, Iter, 1); err != nil {
		panic(err)
	}
	fmt.Printf("train  : %d vecs, %d iters/subspace        -> %v\n", NTrain, Iter, time.Since(t0).Round(time.Millisecond))

	t1 := time.Now()
	codes := q.EncodeBatch(data)
	fmt.Printf("encode : %d vecs -> %d-byte codes          -> %v\n\n", N, M, time.Since(t1).Round(time.Millisecond))

	// Queries from the SAME centers (real neighbors exist in the corpus).
	queries := sampleFrom(centers, NQ, 0.10, rng)
	for _, v := range queries {
		normalize(v)
	}

	// Recall: how many of the exact top-10 does PQ recover?
	var hit, total int
	var pqTime, exTime time.Duration
	for _, qq := range queries {
		te := time.Now()
		exact := exactTopK(data, qq, TopK)
		exTime += time.Since(te)

		tp := time.Now()
		approx := q.Search(qq, codes, TopK)
		pqTime += time.Since(tp)

		set := make(map[int]bool, TopK)
		for _, id := range exact {
			set[id] = true
		}
		for _, r := range approx {
			if set[r.ID] {
				hit++
			}
		}
		total += TopK
	}

	recall := float64(hit) / float64(total)
	rawMB := float64(N*D*4) / 1e6
	pqMB := float64(N*M) / 1e6

	fmt.Printf("recall@%d            : %.4f\n", TopK, recall)
	fmt.Printf("exact search/query  : %v\n", (exTime / NQ).Round(time.Microsecond))
	fmt.Printf("PQ search/query     : %v\n", (pqTime / NQ).Round(time.Microsecond))
	fmt.Printf("speedup             : %.1fx\n\n", float64(exTime)/float64(pqTime))

	fmt.Printf("raw float32         : %.1f MB\n", rawMB)
	fmt.Printf("PQ codes            : %.1f MB\n", pqMB)
	fmt.Printf("compression         : %.1fx\n", rawMB/pqMB)

	// Persisted codebook size (what you ship alongside the codes).
	var buf bytes.Buffer
	if err := q.Save(&buf); err != nil {
		panic(err)
	}
	fmt.Printf("codebook on disk    : %.1f KB\n", float64(buf.Len())/1e3)

	// Sanity: reload and confirm a query still returns the same neighbor.
	q2, err := pq.Load(&buf)
	if err != nil {
		panic(err)
	}
	a := q.Search(queries[0], codes, 1)
	b := q2.Search(queries[0], codes, 1)
	fmt.Printf("save/load roundtrip : top-1 match = %v\n", a[0].ID == b[0].ID)
}

// makeCenters returns a random projection basis W (d x r). Real embeddings have
// low intrinsic rank, so we model that: latent z in R^r, observed x = W z (+noise).
// This yields graded neighborhoods like real embedding spaces, unlike tight blobs.
func makeCenters(r, d int, rng *rand.Rand) [][]float32 {
	W := make([][]float32, d)
	scale := float32(1.0 / math.Sqrt(float64(r)))
	for j := range W {
		row := make([]float32, r)
		for k := range row {
			row[k] = float32(rng.NormFloat64()) * scale
		}
		W[j] = row
	}
	return W
}

// sampleFrom draws n observations x = W z + noise, z ~ N(0, I_r).
func sampleFrom(W [][]float32, n int, noise float64, rng *rand.Rand) [][]float32 {
	d := len(W)
	r := len(W[0])
	out := make([][]float32, n)
	for i := range out {
		z := make([]float32, r)
		for k := range z {
			z[k] = float32(rng.NormFloat64())
		}
		v := make([]float32, d)
		for j := 0; j < d; j++ {
			var s float32
			row := W[j]
			for k := 0; k < r; k++ {
				s += row[k] * z[k]
			}
			v[j] = s + float32(rng.NormFloat64()*noise)
		}
		out[i] = v
	}
	return out
}

func normalize(v []float32) {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s == 0 {
		return
	}
	inv := float32(1.0 / math.Sqrt(s))
	for i := range v {
		v[i] *= inv
	}
}

// exactTopK is brute-force ground truth. Vectors are normalized, so cosine
// similarity is just the dot product, and max-dot == min-L2.
func exactTopK(data [][]float32, query []float32, k int) []int {
	type pair struct {
		id  int
		dot float32
	}
	ps := make([]pair, len(data))
	for i, v := range data {
		var dot float32
		for j := range v {
			dot += v[j] * query[j]
		}
		ps[i] = pair{i, dot}
	}
	sort.Slice(ps, func(a, b int) bool { return ps[a].dot > ps[b].dot })
	out := make([]int, k)
	for i := 0; i < k; i++ {
		out[i] = ps[i].id
	}
	return out
}
