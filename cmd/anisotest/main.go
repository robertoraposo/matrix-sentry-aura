// Command anisotest measures whether anisotropic PQ (ScaNN-style parallel-error
// weighting, Mechanism F) improves flat recall on real embeddings. It sweeps the
// anisotropy weight h (h=1 = standard PQ baseline) and reports full-scan recall
// over exact reconstructions, isolating codebook quality (no routing, no ADC).
//
//	go build -o anisotest ./cmd/anisotest
//	./anisotest -dir /tmp/sembed -prefix real -m 16 -hs "1,2,4,8"
package main

import (
	"flag"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"matrixsentry/internal/aniso"
	"matrixsentry/internal/lab"
)

func main() {
	dir := flag.String("dir", "/tmp/sembed", "data directory")
	prefix := flag.String("prefix", "real", "file prefix")
	m := flag.Int("m", 16, "PQ subspaces")
	k := flag.Int("k", 256, "centroids per subspace")
	iter := flag.Int("iter", 20, "k-means iterations")
	hs := flag.String("hs", "1,2,4,8", "anisotropy weights to sweep (h=1 = plain PQ)")
	seed := flag.Int64("seed", 1, "seed")
	flag.Parse()

	base := lab.ReadFvecs(fmt.Sprintf("%s/%s_base.fvecs", *dir, *prefix))
	query := lab.ReadFvecs(fmt.Sprintf("%s/%s_query.fvecs", *dir, *prefix))
	gt := lab.ReadIvecs(fmt.Sprintf("%s/%s_groundtruth.ivecs", *dir, *prefix))
	d := len(base[0])
	fmt.Printf("== anisotest · %s base=%d query=%d dim=%d M=%d K=%d (%.0fx) ==\n",
		*prefix, len(base), len(query), d, *m, *k, float64(d*4)/float64(*m))
	fmt.Printf("%-6s %-10s %-10s %-10s %-8s\n", "h", "recall@1", "recall@10", "recall@100", "train")

	for _, h := range parseFloats(*hs) {
		t := time.Now()
		a := aniso.Train(base, *m, *k, *iter, h, *seed)
		recon := make([][]float32, len(base))
		lab.ParallelFor(len(base), func(i int) { recon[i] = a.Reconstruct(a.Encode(base[i])) })
		train := time.Since(t).Round(time.Millisecond)

		r1, r10, r100 := recallAt(query, recon, gt)
		fmt.Printf("%-6.1f %-10.4f %-10.4f %-10.4f %-8s\n", h, r1, r10, r100, train)
	}
}

// recallAt does full-scan exact-L2 over reconstructions and returns recall1@{1,10,100}.
func recallAt(query, recon [][]float32, gt [][]int32) (float64, float64, float64) {
	var h1, h10, h100 int
	hits := make([][3]bool, len(query))
	lab.ParallelFor(len(query), func(qi int) {
		q := query[qi]
		type cd struct {
			id int
			d  float64
		}
		cands := make([]cd, len(recon))
		for i, r := range recon {
			var s float64
			for j := range q {
				e := float64(q[j]) - float64(r[j])
				s += e * e
			}
			cands[i] = cd{i, s}
		}
		sort.Slice(cands, func(a, b int) bool { return cands[a].d < cands[b].d })
		truth := int(gt[qi][0])
		for rank := 0; rank < 100 && rank < len(cands); rank++ {
			if cands[rank].id == truth {
				if rank < 1 {
					hits[qi][0] = true
				}
				if rank < 10 {
					hits[qi][1] = true
				}
				hits[qi][2] = true
				break
			}
		}
	})
	for _, h := range hits {
		if h[0] {
			h1++
		}
		if h[1] {
			h10++
		}
		if h[2] {
			h100++
		}
	}
	n := float64(len(query))
	return float64(h1) / n, float64(h10) / n, float64(h100) / n
}

func parseFloats(s string) []float64 {
	var out []float64
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			if v, err := strconv.ParseFloat(p, 64); err == nil {
				out = append(out, v)
			}
		}
	}
	return out
}
