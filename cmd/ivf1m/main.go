// Command ivf1m benchmarks the IVFADC index (ivf package) against the flat PQ
// scan on SIFT1M (or any TEXMEX .fvecs/.ivecs set). It sweeps nprobe and reports,
// per setting, recall vs the dataset's Euclidean ground truth, latency, and the
// fraction of the index actually scanned — the honest source of IVF's speedup.
//
//	go build -o ivf1m ./cmd/ivf1m
//	./ivf1m -dir /data/sift -prefix sift -nlist 1024 -m 8 -k 256 -nprobe 1,8,16,32,64,128
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"matrixsentry/ivf"
	"matrixsentry/pq"
)

func main() {
	dir := flag.String("dir", "/data/sift", "directory holding the .fvecs/.ivecs files")
	prefix := flag.String("prefix", "sift", "file prefix, e.g. 'sift' for sift_base.fvecs")
	nlist := flag.Int("nlist", 1024, "coarse cells (rule of thumb ~sqrt(N))")
	M := flag.Int("m", 8, "PQ subspaces (D must be divisible by M)")
	K := flag.Int("k", 256, "centroids per subspace (<=256)")
	iter := flag.Int("iter", 25, "k-means iterations (PQ; also coarse unless -coarseiter set)")
	coarseIter := flag.Int("coarseiter", 0, "coarse k-means iterations (0 = use -iter; FAISS IVF uses 10)")
	coarseSample := flag.Int("coarsesample", 0, "coarse training subsample (0 = all; FAISS rule ~39*nlist)")
	pqSample := flag.Int("pqsample", 0, "PQ training subsample (0 = all; FAISS caps at K*256)")
	trainMax := flag.Int("train", 0, "cap training vectors (0 = all of the learn set)")
	nprobeCSV := flag.String("nprobe", "1,8,16,32,64,128", "comma-separated nprobe sweep")
	baseline := flag.Bool("baseline", true, "also run the flat PQ full scan for comparison")
	flag.Parse()

	fmt.Printf("== Matrix Sentry · IVFADC on %s (%d cores) ==\n", *prefix, runtime.GOMAXPROCS(0))

	base := readFvecs(fmt.Sprintf("%s/%s_base.fvecs", *dir, *prefix))
	learn := readFvecs(fmt.Sprintf("%s/%s_learn.fvecs", *dir, *prefix))
	query := readFvecs(fmt.Sprintf("%s/%s_query.fvecs", *dir, *prefix))
	gt := readIvecs(fmt.Sprintf("%s/%s_groundtruth.ivecs", *dir, *prefix))
	d := len(base[0])
	fmt.Printf("base=%d  learn=%d  query=%d  dim=%d  gt/query=%d\n", len(base), len(learn), len(query), d, len(gt[0]))

	if *trainMax > 0 && *trainMax < len(learn) {
		learn = learn[:*trainMax]
	}

	nprobes := parseInts(*nprobeCSV)

	// --- flat PQ baseline (reuses pq directly, scans every code) ---
	if *baseline {
		fmt.Printf("\n-- flat PQ baseline (M=%d K=%d, full scan) --\n", *M, *K)
		flat, err := pq.New(d, *M, *K)
		if err != nil {
			panic(err)
		}
		if err := flat.Train(learn, *iter, 1); err != nil {
			panic(err)
		}
		codes := flat.EncodeBatch(base)
		for _, R := range []int{1, 10, 100} {
			t := time.Now()
			res := flat.SearchBatch(query, codes, R)
			dur := time.Since(t)
			r1, inter := score(res, gt, R)
			fmt.Printf("  flat  recall1@%-3d %.4f  inter@%-3d %.4f  %6.3f ms/q  (scan 100.0%%)\n",
				R, r1, R, inter, msPerQuery(dur, len(query)))
		}
	}

	// --- IVFADC build (coarse + residual PQ), then nprobe sweep ---
	opts := ivf.TrainOpts{CoarseIter: *coarseIter, CoarseSample: *coarseSample, PQSample: *pqSample}
	fmt.Printf("\n-- IVFADC (nlist=%d, M=%d K=%d, coarseIter=%d coarseSample=%d pqSample=%d) --\n",
		*nlist, *M, *K, *coarseIter, *coarseSample, *pqSample)
	t0 := time.Now()
	ix, err := ivf.TrainWithOpts(learn, *nlist, *M, *K, *iter, 1, opts)
	if err != nil {
		panic(err)
	}
	fmt.Printf("train  (coarse+residual PQ, %d learn vecs): %v\n", len(learn), time.Since(t0).Round(time.Millisecond))
	t1 := time.Now()
	ix.Add(base)
	fmt.Printf("add    (%d base vecs encoded into cells): %v\n\n", len(base), time.Since(t1).Round(time.Millisecond))

	fmt.Printf("%-7s %-12s %-12s %-13s %-11s %-10s\n", "nprobe", "recall1@1", "recall1@10", "recall1@100", "inter@100", "ms/q  scan%")
	for _, np := range nprobes {
		// recall at each R, plus latency and scan fraction from the @100 pass.
		var r1at1, r1at10, r1at100, inter100, msq, scanPct float64
		for _, R := range []int{1, 10, 100} {
			t := time.Now()
			res := ix.SearchBatch(query, np, R)
			dur := time.Since(t)
			r1, inter := score(res, gt, R)
			switch R {
			case 1:
				r1at1 = r1
			case 10:
				r1at10 = r1
			case 100:
				r1at100, inter100, msq = r1, inter, msPerQuery(dur, len(query))
				scanPct = avgScanPct(ix, query, np)
			}
		}
		fmt.Printf("%-7d %-12.4f %-12.4f %-13.4f %-11.4f %6.3f %5.1f%%\n",
			np, r1at1, r1at10, r1at100, inter100, msq, scanPct)
	}
}

// score returns recall1@R (true NN found in topR) and inter@R (overlap of the
// returned R with the true top-R), matching cmd/sift1m's definitions exactly.
func score(results [][]pq.Result, gt [][]int32, R int) (recall1, inter float64) {
	var found1, interHit, interTot int
	for qi := range results {
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
	return float64(found1) / float64(len(results)), float64(interHit) / float64(interTot)
}

func avgScanPct(ix *ivf.IVFADC, query [][]float32, nprobe int) float64 {
	total := 0
	for _, q := range query {
		total += ix.Scanned(q, nprobe)
	}
	mean := float64(total) / float64(len(query))
	return 100.0 * mean / float64(ix.Ntotal())
}

func msPerQuery(d time.Duration, n int) float64 {
	return float64(d.Microseconds()) / float64(n) / 1000.0
}

func parseInts(csv string) []int {
	var out []int
	for _, s := range strings.Split(csv, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			panic(fmt.Sprintf("bad nprobe value %q: %v", s, err))
		}
		out = append(out, n)
	}
	return out
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
