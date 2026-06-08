// Command opqtest measures whether a rotation before product quantization
// (Mechanism F) improves flat (full-scan) recall — isolating the QUANTIZATION
// effect with no IVF routing confound. It compares plain PQ vs RR-PQ (random
// rotation) on a {prefix}_base/query.fvecs + groundtruth.ivecs dataset.
//
//	go build -o opqtest ./cmd/opqtest
//	./opqtest -dir /tmp/sembed -prefix real -m 16
package main

import (
	"flag"
	"fmt"
	"time"

	"matrixsentry/internal/lab"
	"matrixsentry/internal/opq"
	"matrixsentry/pq"
)

func main() {
	dir := flag.String("dir", "/tmp/sembed", "data directory")
	prefix := flag.String("prefix", "real", "file prefix")
	m := flag.Int("m", 16, "PQ subspaces")
	k := flag.Int("k", 256, "centroids per subspace")
	iter := flag.Int("iter", 25, "k-means iterations")
	seed := flag.Int64("seed", 1, "rotation seed")
	flag.Parse()

	base := lab.ReadFvecs(fmt.Sprintf("%s/%s_base.fvecs", *dir, *prefix))
	query := lab.ReadFvecs(fmt.Sprintf("%s/%s_query.fvecs", *dir, *prefix))
	gt := lab.ReadIvecs(fmt.Sprintf("%s/%s_groundtruth.ivecs", *dir, *prefix))
	d := len(base[0])
	fmt.Printf("== opqtest · %s  base=%d query=%d dim=%d  M=%d K=%d (%.0fx compression) ==\n",
		*prefix, len(base), len(query), d, *m, *k, float64(d*4)/float64(*m))

	report("plain PQ", base, query, gt, *m, *k, *iter)

	t := time.Now()
	R := opq.RandomRotation(d, *seed)
	rbase := opq.RotateBatch(R, base)
	rquery := opq.RotateBatch(R, query)
	fmt.Printf("[rotation built + applied: %v]\n", time.Since(t).Round(time.Millisecond))
	report("RR-PQ   ", rbase, rquery, gt, *m, *k, *iter)
}

func report(name string, base, query [][]float32, gt [][]int32, m, k, iter int) {
	q, err := pq.New(len(base[0]), m, k)
	if err != nil {
		panic(err)
	}
	if err := q.Train(base, iter, 1); err != nil {
		panic(err)
	}
	codes := q.EncodeBatch(base)
	for _, R := range []int{1, 10, 100} {
		res := q.SearchBatch(query, codes, R)
		fmt.Printf("  %s  recall1@%-3d %.4f\n", name, R, recall1(res, gt, R))
	}
}

// recall1@R: fraction of queries whose true top-1 neighbour (gt[q][0]) is in the
// returned top-R.
func recall1(res [][]pq.Result, gt [][]int32, R int) float64 {
	hits := 0
	for qi := range res {
		truth := int(gt[qi][0])
		for _, r := range res[qi] {
			if r.ID == truth {
				hits++
				break
			}
		}
	}
	return float64(hits) / float64(len(res))
}
