// Command streamlift calibrates the synthetic access-stream generator used by
// cmd/ivfpredict to the real predictability (lift) the live agent exhibits. For
// each eta it reproduces the EXACT stream ivfpredict builds (same seeds, bag,
// successor, GT mapping) and reports the realized Markov-vs-marginal lift — the
// same metric the live analyzer reports — so we can read ivfpredict's recall
// table at the eta whose lift matches the η measured in production.
//
//	go build -o streamlift ./cmd/streamlift
//	./streamlift -dir /data/sift -prefix sift -t 30000 -zipf 0.5
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"

	"matrixsentry/internal/lab"
	"matrixsentry/internal/refine"
)

func main() {
	dir := flag.String("dir", "/data/sift", "data directory")
	prefix := flag.String("prefix", "sift", "file prefix")
	T := flag.Int("t", 30000, "stream length (match the ivfpredict run)")
	zipf := flag.Float64("zipf", 0.5, "Zipf exponent for the i.i.d. part")
	etas := flag.String("etas", "0,0.1,0.2,0.3,0.5,0.7,0.9", "branching ratios to calibrate")
	flag.Parse()

	const seed = int64(1) // must match cmd/ivfpredict

	gt := lab.ReadIvecs(fmt.Sprintf("%s/%s_groundtruth.ivecs", *dir, *prefix))
	Nq := len(gt)

	// Exactly cmd/ivfpredict's stream construction (lines 131-139, seed=1).
	weights := refine.ZipfWeights(Nq, *zipf, seed+9)
	bag := buildBag(weights, 200000, seed+13)
	successor := rand.New(rand.NewSource(seed + 17)).Perm(Nq)

	fmt.Printf("== streamlift · Nq=%d  T=%d  zipf=%.2f ==\n", Nq, *T, *zipf)
	fmt.Printf("%-6s %-10s %-10s %-12s %-10s\n", "eta", "lift", "markov", "marginal", "coverage")
	for _, eta := range parseFloats(*etas) {
		stream := refine.SelfExcitingStream(*T, eta, bag, successor, seed+21)
		aSeq := make([]int, *T)
		for t := 0; t < *T; t++ {
			aSeq[t] = int(gt[stream[t]][0])
		}
		l := refine.StreamLift(aSeq)
		fmt.Printf("%-6.2f %-10.1f %-10.1f %-12.1f %-10.1f\n",
			eta, l.Lift*100, l.MarkovHit*100, l.MarginalHit*100, l.Coverage*100)
	}
}

// buildBag mirrors cmd/ivfpredict.buildBag exactly (weighted-sample multiset).
func buildBag(weights []float64, size int, seed int64) []int {
	cum := make([]float64, len(weights))
	var s float64
	for i, w := range weights {
		s += w
		cum[i] = s
	}
	rng := rand.New(rand.NewSource(seed))
	bag := make([]int, size)
	for i := range bag {
		x := rng.Float64() * s
		bag[i] = sort.Search(len(cum), func(j int) bool { return cum[j] >= x })
	}
	return bag
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
