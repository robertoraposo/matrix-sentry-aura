// Command sift evaluates the hand-built Product Quantizer on SIFT10K — real SIFT
// image descriptors with ground-truth nearest neighbors precomputed by INRIA
// (corpus TEXMEX). This is an independent test: we generated neither the vectors
// nor the ground truth, and the recall is directly comparable to published
// FAISS / Jégou-et-al. numbers for the same PQ configuration.
//
// SIFT ground truth uses Euclidean (L2) distance, so vectors are NOT normalized;
// our ADC is squared-L2, which matches exactly.
package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"time"

	"matrixsentry/pq"
)

const dir = "/tmp/siftsmall"

func main() {
	fmt.Println("== Matrix Sentry · PQ on SIFT10K (real data, independent ground truth) ==")

	base := readFvecs(dir + "/siftsmall_base.fvecs")      // 10000 x 128
	learn := readFvecs(dir + "/siftsmall_learn.fvecs")    // 25000 x 128
	query := readFvecs(dir + "/siftsmall_query.fvecs")    // 100 x 128
	gt := readIvecs(dir + "/siftsmall_groundtruth.ivecs") // 100 x 100 (true NN ids)
	d := len(base[0])
	fmt.Printf("base=%d  learn=%d  query=%d  dim=%d  gt/query=%d\n\n",
		len(base), len(learn), len(query), d, len(gt[0]))

	// Sanity: confirm our fvecs reader + L2 metric agree with INRIA's ground truth.
	// If our brute-force top-1 matches their top-1, the pipeline is trustworthy.
	agree := 0
	for qi, q := range query {
		if bruteTop1(base, q) == int(gt[qi][0]) {
			agree++
		}
	}
	fmt.Printf("pipeline check: brute-force top-1 == INRIA ground truth on %d/%d queries\n\n", agree, len(query))

	// Standard SIFT PQ config: M=8 subspaces, K=256 centroids -> 8-byte codes.
	const (
		M    = 8
		K    = 256
		Iter = 20
	)
	q, err := pq.New(d, M, K)
	if err != nil {
		panic(err)
	}

	t0 := time.Now()
	if err := q.Train(learn, Iter, 1); err != nil {
		panic(err)
	}
	fmt.Printf("train (M=%d K=%d, %d learn vecs, %d iters): %v\n", M, K, len(learn), Iter, time.Since(t0).Round(time.Millisecond))

	t1 := time.Now()
	codes := q.EncodeBatch(base)
	fmt.Printf("encode %d base vecs -> %d-byte codes: %v\n\n", len(base), M, time.Since(t1).Round(time.Millisecond))

	// Two standard metrics, both averaged over queries:
	//   recall1@R  : is the single true nearest neighbor among the top-R returned?
	//                (the canonical ANN curve; this is what FAISS/Jégou report)
	//   inter@R    : |PQ top-R  ∩  ground-truth top-R| / R  (a stricter ranking test)
	for _, R := range []int{1, 10, 100} {
		var found1, interHit, interTot int
		var pqDur, exDur time.Duration
		for qi, qv := range query {
			tp := time.Now()
			res := q.Search(qv, codes, R)
			pqDur += time.Since(tp)

			te := time.Now()
			_ = bruteTop1(base, qv) // timed reference for speed comparison
			exDur += time.Since(te)

			trueNN := int(gt[qi][0])
			truth := make(map[int]bool, R)
			for i := 0; i < R; i++ {
				truth[int(gt[qi][i])] = true
			}
			for _, r := range res {
				if r.ID == trueNN {
					found1++
				}
				if truth[r.ID] {
					interHit++
				}
			}
			interTot += R
		}
		fmt.Printf("recall1@%-3d : %.4f      inter@%-3d : %.4f      (PQ %v/q vs exact %v/q)\n",
			R, float64(found1)/float64(len(query)),
			R, float64(interHit)/float64(interTot),
			(pqDur / time.Duration(len(query))).Round(time.Microsecond),
			(exDur / time.Duration(len(query))).Round(time.Microsecond))
	}

	rawMB := float64(len(base)*d*4) / 1e6
	pqMB := float64(len(base)*M) / 1e6
	fmt.Printf("\nmemory: raw float32 %.2f MB -> PQ codes %.3f MB  (%.0fx compression)\n", rawMB, pqMB, rawMB/pqMB)

	fmt.Println("\nReference (published FAISS / Jégou et al., PQ M=8 K=256 ADC):")
	fmt.Println("  SIFT1M: recall@1 ~0.22, recall@10 ~0.60, recall@100 ~0.92")
	fmt.Println("  SIFT10K runs higher (fewer distractors among 10k vs 1M base).")
}

// bruteTop1 returns the id of the exact L2 nearest neighbor of q in data.
func bruteTop1(data [][]float32, q []float32) int {
	best, bestD := -1, math.MaxFloat64
	for i, v := range data {
		var s float64
		for j := range v {
			dd := float64(v[j]) - float64(q[j])
			s += dd * dd
		}
		if s < bestD {
			bestD, best = s, i
		}
	}
	return best
}

// --- .fvecs / .ivecs readers (little-endian: int32 dim, then dim values) ---

func readFvecs(path string) [][]float32 {
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

func readIvecs(path string) [][]int32 {
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
