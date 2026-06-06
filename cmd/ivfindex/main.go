// Command ivfindex demonstrates the production CA-IVFADC Index on SIFT1M (or any
// TEXMEX set): build → Save → Load → Search recall from the loaded index, plus a
// content-address demo (re-adding a stored vector is recognized; Recall finds a
// perturbed near-duplicate as the tolerance widens).
//
//	go build -o ivfindex ./cmd/ivfindex
//	./ivfindex -dir /data/sift -prefix sift -nlist 1024 -m 8 -k 256 \
//	  -coarseiter 10 -coarsesample 39936 -pqsample 65536 -nprobe 16
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"time"

	"matrixsentry/ivf"
)

func main() {
	dir := flag.String("dir", "/data/sift", "directory holding the .fvecs/.ivecs files")
	prefix := flag.String("prefix", "sift", "file prefix, e.g. 'sift' for sift_base.fvecs")
	nlist := flag.Int("nlist", 1024, "coarse cells")
	M := flag.Int("m", 8, "PQ subspaces (Dim must be divisible by M)")
	K := flag.Int("k", 256, "centroids per subspace (<=256)")
	iter := flag.Int("iter", 25, "PQ k-means iterations")
	coarseIter := flag.Int("coarseiter", 10, "coarse k-means iterations (FAISS IVF uses 10)")
	coarseSample := flag.Int("coarsesample", 39936, "coarse training subsample (0 = all)")
	pqSample := flag.Int("pqsample", 65536, "PQ training subsample (0 = all)")
	nprobe := flag.Int("nprobe", 16, "cells probed per query")
	flag.Parse()

	fmt.Printf("== CA-IVFADC Index · %s (%d cores) ==\n", *prefix, runtime.GOMAXPROCS(0))
	base := readFvecs(fmt.Sprintf("%s/%s_base.fvecs", *dir, *prefix))
	learn := readFvecs(fmt.Sprintf("%s/%s_learn.fvecs", *dir, *prefix))
	query := readFvecs(fmt.Sprintf("%s/%s_query.fvecs", *dir, *prefix))
	gt := readIvecs(fmt.Sprintf("%s/%s_groundtruth.ivecs", *dir, *prefix))
	d := len(base[0])
	fmt.Printf("base=%d learn=%d query=%d dim=%d\n", len(base), len(learn), len(query), d)

	// Map each base vector's content hash to its position, so content-addressed
	// hits can be scored against position-keyed ground truth.
	pos := make(map[uint64]int, len(base))
	for i, v := range base {
		pos[hashOf(v)] = i
	}

	ix, err := ivf.New(ivf.Config{
		Dim: d, Nlist: *nlist, M: *M, K: *K, Iter: *iter, Seed: 1,
		Train: ivf.TrainOpts{CoarseIter: *coarseIter, CoarseSample: *coarseSample, PQSample: *pqSample},
	})
	must(err)

	t0 := time.Now()
	must(ix.Train(learn))
	fmt.Printf("train: %v\n", time.Since(t0).Round(time.Millisecond))
	t1 := time.Now()
	ix.Add(base)
	fmt.Printf("add (%d vecs): %v  Ntotal=%d\n", len(base), time.Since(t1).Round(time.Millisecond), ix.Ntotal())

	// Persist, then load into a fresh index — the build-once / serve-instantly path.
	path := "/tmp/ca-ivfadc.gob"
	f, err := os.Create(path)
	must(err)
	must(ix.Save(f))
	must(f.Close())
	fi, _ := os.Stat(path)
	g, err := os.Open(path)
	must(err)
	t2 := time.Now()
	loaded, err := ivf.Load(g)
	must(err)
	must(g.Close())
	fmt.Printf("save+load: %.1f MB on disk, load %v, Ntotal=%d\n",
		float64(fi.Size())/1e6, time.Since(t2).Round(time.Millisecond), loaded.Ntotal())

	// Recall metrics from the LOADED index — proves persistence is lossless.
	for _, R := range []int{1, 10, 100} {
		t := time.Now()
		hits := loaded.SearchBatch(query, *nprobe, R)
		dur := time.Since(t)
		r1, inter := score(hits, gt, R, pos)
		fmt.Printf("  loaded recall1@%-3d %.4f  inter@%-3d %.4f  %.3f ms/q\n",
			R, r1, R, inter, float64(dur.Microseconds())/float64(len(query))/1000.0)
	}

	// Content-address demo: re-add a stored base vector → recognized; perturb it → Recall finds it.
	re := loaded.Add([][]float32{base[0]})
	fmt.Printf("\ncontent-address demo:\n  re-add base[0]: %s (Ntotal still %d)\n", re[0].Status, loaded.Ntotal())
	pert := append([]float32(nil), base[0]...)
	pert[0] += 1
	for _, tol := range []int{0, 1, 2, 4, *M} {
		h, ok := loaded.Recall(pert, tol)
		hit := "-"
		if ok {
			if p, found := pos[h.Hash]; found {
				hit = fmt.Sprintf("base[%d]", p)
			} else {
				hit = fmt.Sprintf("hash=%d", h.Hash)
			}
		}
		fmt.Printf("  Recall(perturbed base[0], tol=%d): found=%v %s\n", tol, ok, hit)
	}
}

// score returns recall1@R and inter@R, matching cmd/ivf1m's definitions but
// resolving each content-addressed Handle back to a base position via its hash.
func score(hits [][]ivf.Hit, gt [][]int32, R int, pos map[uint64]int) (recall1, inter float64) {
	var found1, interHit, interTot int
	for qi := range hits {
		trueNN := int(gt[qi][0])
		truth := make(map[int]bool, R)
		for i := 0; i < R && i < len(gt[qi]); i++ {
			truth[int(gt[qi][i])] = true
		}
		for _, hit := range hits[qi] {
			p, ok := pos[hit.Handle.Hash]
			if !ok {
				continue
			}
			if p == trueNN {
				found1++
			}
			if truth[p] {
				interHit++
			}
		}
		interTot += R
	}
	return float64(found1) / float64(len(hits)), float64(interHit) / float64(interTot)
}

// hashOf mirrors ivf's unexported FNV-1a over a vector's float32 bytes so the
// demo can map a returned Handle.Hash back to its base position.
func hashOf(v []float32) uint64 {
	const offset = 14695981039346656037
	const prime = 1099511628211
	h := uint64(offset)
	var buf [4]byte
	for _, x := range v {
		binary.LittleEndian.PutUint32(buf[:], math.Float32bits(x))
		for _, b := range buf {
			h ^= uint64(b)
			h *= prime
		}
	}
	return h
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
