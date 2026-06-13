// Command ivfverify closes the loop on the GA auto-tuner: it builds an index
// through the PRODUCTION ivf API (New → Train → Add → SearchRerank) using the
// canonical ivf.Recommended config, and reports recall@10 on the real 768-d
// embeddings against the baseline. The GA evolved and validated its config on
// lab's bit-identical reimplementation; this proves the production code path
// reproduces the number (~0.985 vs baseline ~0.916) before we call it applied.
//
//	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ivfverify ./cmd/ivfverify
//	./ivfverify -dir /tmp/sembed-big -prefix big
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"matrixsentry/ivf"
)

func main() {
	dir := flag.String("dir", "/tmp/sembed-big", "dataset directory")
	prefix := flag.String("prefix", "big", "dataset file prefix")
	flag.Parse()

	base := readFvecs(fmt.Sprintf("%s/%s_base.fvecs", *dir, *prefix))
	learn := readFvecs(fmt.Sprintf("%s/%s_learn.fvecs", *dir, *prefix))
	query := readFvecs(fmt.Sprintf("%s/%s_query.fvecs", *dir, *prefix))
	gt := readIvecs(fmt.Sprintf("%s/%s_groundtruth.ivecs", *dir, *prefix))
	dim := len(base[0])
	fmt.Printf("== ivfverify · N=%d dim=%d queries=%d (production ivf API) ==\n", len(base), dim, len(query))

	cfg, sp, err := ivf.Recommended(dim)
	must(err)
	fmt.Printf("ivf.Recommended(%d): nlist=%d M=%d K=%d · nprobe=%d rerankK=%d\n",
		dim, cfg.Nlist, cfg.M, cfg.K, sp.Nprobe, sp.RerankK)

	t0 := time.Now()
	ix, err := ivf.New(cfg)
	must(err)
	must(ix.Train(learn))
	ix.Add(base)
	fmt.Printf("build: %v · Ntotal=%d\n", time.Since(t0).Round(time.Millisecond), ix.Ntotal())

	// originals keyed by content hash — the fetch a CAS/RAM registry would serve.
	byHash := make(map[uint64][]float32, len(base))
	for _, v := range base {
		byHash[ivf.HashVec(v)] = v
	}
	fetch := func(h uint64) []float32 { return byHash[h] }

	// pos maps content hash → base index, to score against position-keyed GT.
	pos := make(map[uint64]int, len(base))
	for i, v := range base {
		pos[ivf.HashVec(v)] = i
	}

	recall := func(search func(q []float32) []ivf.Hit) float64 {
		hit := 0
		for qi, q := range query {
			want := int(gt[qi][0])
			for _, h := range search(q) {
				if p, ok := pos[h.Handle.Hash]; ok && p == want {
					hit++
					break
				}
			}
		}
		return float64(hit) / float64(len(query))
	}

	baseRecall := recall(func(q []float32) []ivf.Hit { return ix.Search(q, 16, 10) })
	prodRecall := recall(func(q []float32) []ivf.Hit { return ix.SearchRerank(q, sp.Nprobe, 10, sp.RerankK, fetch) })

	fmt.Printf("\n== recall@10 (all %d queries, production path) ==\n", len(query))
	fmt.Printf("  baseline Search(nprobe=16)             : %.4f\n", baseRecall)
	fmt.Printf("  Recommended SearchRerank(np=%d,rr=%d) : %.4f  (%+.2fpp)\n",
		sp.Nprobe, sp.RerankK, prodRecall, (prodRecall-baseRecall)*100)
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
