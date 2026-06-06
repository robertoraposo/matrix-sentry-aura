// Command sift1m evaluates the hand-built Product Quantizer on the full SIFT1M
// benchmark (or any TEXMEX .fvecs/.ivecs set) using the parallel code paths.
// It is scale-agnostic: point -dir/-prefix at SIFT1M or siftsmall.
//
// Recall is measured against the dataset's own (Euclidean) ground truth, so
// vectors are NOT normalized. Numbers are directly comparable to published
// FAISS / Jégou-et-al. results for the same PQ configuration.
//
// Typical run on SIFT1M (multi-core homelab):
//
//	go build -o sift1m ./cmd/sift1m
//	./sift1m -dir /data/sift -prefix sift -m 8 -k 256 -iter 25
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"time"

	"matrixsentry/pq"
)

func main() {
	dir := flag.String("dir", "/tmp/siftsmall", "directory holding the .fvecs/.ivecs files")
	prefix := flag.String("prefix", "siftsmall", "file prefix, e.g. 'sift' for sift_base.fvecs")
	M := flag.Int("m", 8, "PQ subspaces (D must be divisible by M)")
	K := flag.Int("k", 256, "centroids per subspace (<=256)")
	iter := flag.Int("iter", 25, "k-means iterations per subspace")
	trainMax := flag.Int("train", 0, "cap training vectors (0 = use all of the learn set)")
	checkN := flag.Int("check", 50, "queries to verify reader/metric against ground truth")
	flag.Parse()

	fmt.Printf("== Matrix Sentry · PQ on %s (%d cores) ==\n", *prefix, runtime.GOMAXPROCS(0))

	base := readFvecs(fmt.Sprintf("%s/%s_base.fvecs", *dir, *prefix))
	learn := readFvecs(fmt.Sprintf("%s/%s_learn.fvecs", *dir, *prefix))
	query := readFvecs(fmt.Sprintf("%s/%s_query.fvecs", *dir, *prefix))
	gt := readIvecs(fmt.Sprintf("%s/%s_groundtruth.ivecs", *dir, *prefix))
	d := len(base[0])
	fmt.Printf("base=%d  learn=%d  query=%d  dim=%d  gt/query=%d\n\n",
		len(base), len(learn), len(query), d, len(gt[0]))

	if *trainMax > 0 && *trainMax < len(learn) {
		learn = learn[:*trainMax]
	}

	// Pipeline check on a sample: our brute-force top-1 must match the dataset's.
	chk := *checkN
	if chk > len(query) {
		chk = len(query)
	}
	agree := 0
	for qi := 0; qi < chk; qi++ {
		if bruteTop1(base, query[qi]) == int(gt[qi][0]) {
			agree++
		}
	}
	fmt.Printf("pipeline check: brute-force top-1 == ground truth on %d/%d sampled queries\n\n", agree, chk)

	q, err := pq.New(d, *M, *K)
	if err != nil {
		panic(err)
	}

	t0 := time.Now()
	if err := q.Train(learn, *iter, 1); err != nil {
		panic(err)
	}
	fmt.Printf("train  (M=%d K=%d, %d learn vecs, %d iters): %v\n", *M, *K, len(learn), *iter, time.Since(t0).Round(time.Millisecond))

	t1 := time.Now()
	codes := q.EncodeBatch(base)
	fmt.Printf("encode (%d base vecs -> %d-byte codes): %v\n\n", len(base), *M, time.Since(t1).Round(time.Millisecond))

	for _, R := range []int{1, 10, 100} {
		t := time.Now()
		results := q.SearchBatch(query, codes, R) // parallel over queries
		dur := time.Since(t)

		var found1, interHit, interTot int
		for qi := range query {
			trueNN := int(gt[qi][0])
			truth := make(map[int]bool, R)
			for i := 0; i < R && i < len(gt[qi]); i++ {
				truth[int(gt[qi][i])] = true
			}
			for _, r := range results[qi] {
				if r.ID == trueNN {
					found1++
				}
				if truth[r.ID] {
					interHit++
				}
			}
			interTot += R
		}
		fmt.Printf("recall1@%-3d : %.4f      inter@%-3d : %.4f      (%v for %d queries, %v/q)\n",
			R, float64(found1)/float64(len(query)),
			R, float64(interHit)/float64(interTot),
			dur.Round(time.Millisecond), len(query),
			(dur / time.Duration(len(query))).Round(time.Microsecond))
	}

	rawMB := float64(len(base)*d*4) / 1e6
	pqMB := float64(len(base)**M) / 1e6
	fmt.Printf("\nmemory: raw float32 %.1f MB -> PQ codes %.1f MB  (%.0fx compression)\n", rawMB, pqMB, rawMB/pqMB)
	fmt.Println("\nReference (FAISS / Jégou, PQ M=8 K=256 ADC on SIFT1M):")
	fmt.Println("  recall1@1 ~0.22   recall1@10 ~0.60   recall1@100 ~0.92")
	fmt.Println("  (M=16 roughly doubles code size and pushes recall1@100 toward ~0.97)")
}

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
