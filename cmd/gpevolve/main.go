// Command gpevolve is the GP adaptive policy runner for the IVFADC engine.
// Phase 0 ("feasibility") measures whether per-query margin features predict
// the minimum search depth required to retrieve the true nearest neighbor,
// and whether an oracle min-depth policy holds materially less cost than the
// static recommended config. If both gates pass, a GP policy learner is worth
// building (Phase 1 / Task 5).
//
// Usage (Phase 0 feasibility on tesla):
//
//	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o gpevolve ./cmd/gpevolve
//	./gpevolve -data /tmp/sembed-big/big -e 1000 -h 4000 -out /tmp/gp-out
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"

	"matrixsentry/ivf"
)

func main() {
	dataFlag := flag.String("data", "/tmp/sembed-big/big", "dataset path/prefix (e.g. /tmp/sembed-big/big → big_base.fvecs, big_query.fvecs, big_learn.fvecs)")
	eFlag := flag.Int("e", 1000, "number of evolution queries (E set)")
	hFlag := flag.Int("h", 4000, "number of holdout queries (H set, unused in Phase 0 but reserved)")
	seedFlag := flag.Int64("seed", 1, "RNG seed (unused in Phase 0 query split; reserved for Phase 1)")
	outFlag := flag.String("out", "/tmp/gp-out", "output path prefix (writes <out>.md)")
	phaseFlag := flag.String("phase", "feasibility", "phase to run: feasibility | evolve")
	flag.Parse()

	if *phaseFlag == "evolve" {
		fmt.Println("evolve phase implemented in Task 5")
		return
	}

	_ = seedFlag // reserved for Phase 1 query-split RNG

	// ---- load dataset ----
	base := readFvecs(*dataFlag + "_base.fvecs")
	learn := readFvecs(*dataFlag + "_learn.fvecs")
	query := readFvecs(*dataFlag + "_query.fvecs")

	dim := len(base[0])
	fmt.Printf("== gpevolve Phase-0 · N=%d dim=%d learn=%d queries=%d ==\n",
		len(base), dim, len(learn), len(query))

	// Split queries: E = first -e, H = next -h (disjoint, sequential — no shuffle
	// in Phase 0; the GP policy is trained/validated on these slices in Phase 1).
	eN := *eFlag
	hN := *hFlag
	if eN > len(query) {
		eN = len(query)
	}
	if eN+hN > len(query) {
		hN = len(query) - eN
	}
	queryE := query[:eN]
	_ = query[eN : eN+hN] // H reserved; silence unused warning via blank

	// ---- build production index ----
	cfg, sp, err := ivf.Recommended(dim)
	must(err)
	fmt.Printf("ivf.Recommended(%d): nlist=%d M=%d K=%d · nprobe=%d rerankK=%d\n",
		dim, cfg.Nlist, cfg.M, cfg.K, sp.Nprobe, sp.RerankK)

	ix, err := ivf.New(cfg)
	must(err)
	must(ix.Train(learn))
	ix.Add(base)
	fmt.Printf("index built · Ntotal=%d\n", ix.Ntotal())

	// content-hash → base vector (for SearchRerank fetch) and → base index (for GT scoring)
	byHash := make(map[uint64][]float32, len(base))
	idByHash := make(map[uint64]int32, len(base))
	for i, v := range base {
		h := ivf.HashVec(v)
		byHash[h] = v
		idByHash[h] = int32(i)
	}
	fetch := func(h uint64) []float32 { return byHash[h] }

	// ---- exact ground truth (brute-force) for E ----
	fmt.Printf("computing exact GT for %d E queries...\n", eN)
	gt0E := make([]int32, eN)
	for i, q := range queryE {
		gt0E[i] = exactNN(q, base)
	}

	// ---- static baseline on E ----
	fmt.Printf("running static baseline (nprobe=%d rerankK=%d) on E...\n", sp.Nprobe, sp.RerankK)
	var recallSumStatic, costSumStatic float64
	for i, q := range queryE {
		hits := ix.SearchRerank(q, sp.Nprobe, 10, sp.RerankK, fetch)
		ids := hitsToIDs(hits, idByHash)
		recallSumStatic += recallHit(ids, gt0E[i])
		costSumStatic += queryProxy(sp.Nprobe, sp.RerankK, dim, len(base), cfg.Nlist)
	}
	meanRecallStatic := recallSumStatic / float64(eN)
	meanLatStatic := costSumStatic / float64(eN)
	fmt.Printf("static baseline: recall@10=%.4f  mean proxy=%.0f\n", meanRecallStatic, meanLatStatic)

	// ---- min-depth oracle per E query ----
	// Build sorted grid pairs ascending by queryProxy.
	type depthPair struct {
		np, rk int
		cost   float64
	}
	var pairs []depthPair
	for _, np := range nprobeGrid {
		npCapped := np
		if npCapped > cfg.Nlist {
			npCapped = cfg.Nlist
		}
		for _, rk := range rerankGrid {
			pairs = append(pairs, depthPair{
				np:   npCapped,
				rk:   rk,
				cost: queryProxy(npCapped, rk, dim, len(base), cfg.Nlist),
			})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].cost < pairs[j].cost })
	// deduplicate by (np, rk) after nlist capping
	seen := make(map[[2]int]bool)
	unique := pairs[:0]
	for _, p := range pairs {
		k := [2]int{p.np, p.rk}
		if !seen[k] {
			seen[k] = true
			unique = append(unique, p)
		}
	}
	pairs = unique

	fmt.Printf("oracle: evaluating %d depth pairs × %d E queries...\n", len(pairs), eN)

	// per-query min-depth proxy and features
	minDepthCost := make([]float64, eN)
	features := make([][4]float32, eN)
	var oracleRecallSum float64

	maxPair := pairs[len(pairs)-1]

	for i, q := range queryE {
		feats := ix.QueryFeatures(q)
		features[i] = feats

		found := false
		for _, p := range pairs {
			hits := ix.SearchRerank(q, p.np, 10, p.rk, fetch)
			ids := hitsToIDs(hits, idByHash)
			if recallHit(ids, gt0E[i]) == 1.0 {
				minDepthCost[i] = p.cost
				oracleRecallSum += 1.0
				found = true
				break
			}
		}
		if !found {
			// true NN not retrievable at any grid depth — use max cost
			minDepthCost[i] = maxPair.cost
			// no oracle recall credit
		}
	}

	oracleMeanLat := 0.0
	for _, c := range minDepthCost {
		oracleMeanLat += c
	}
	oracleMeanLat /= float64(eN)
	oracleRecall := oracleRecallSum / float64(eN)

	// ---- Pearson correlations: features vs min-depth proxy ----
	fNames := [4]string{"f0(d_near)", "f1(ambiguity)", "f2(margin)", "f3(spread)"}
	var corrs [4]float64
	ys := make([]float64, eN)
	for i, c := range minDepthCost {
		ys[i] = c
	}
	maxAbsCorr := 0.0
	for k := 0; k < 4; k++ {
		xs := make([]float64, eN)
		for i := range features {
			xs[i] = float64(features[i][k])
		}
		corrs[k] = pearson(xs, ys)
		if a := math.Abs(corrs[k]); a > maxAbsCorr {
			maxAbsCorr = a
		}
	}

	// ---- verdict ----
	latHeadroom := oracleMeanLat < 0.9*meanLatStatic
	predictable := maxAbsCorr > 0.2
	gatePass := latHeadroom && predictable

	// ---- write report ----
	var verdict string
	if gatePass {
		verdict = "GATE PASS"
	} else {
		reason := ""
		if !latHeadroom {
			reason += fmt.Sprintf(" oracle lat %.0f >= 0.9 × static %.0f (no material headroom)", oracleMeanLat, meanLatStatic)
		}
		if !predictable {
			if reason != "" {
				reason += ";"
			}
			reason += fmt.Sprintf(" max|corr|=%.3f <= 0.2 (features not predictive)", maxAbsCorr)
		}
		verdict = "GATE FAIL —" + reason
	}

	report := fmt.Sprintf(`# GP Adaptive Policy — Phase 0 Feasibility Report

## Dataset
- N=%d  dim=%d  learn=%d  |E|=%d  |H|=%d

## Index (ivf.Recommended)
- nlist=%d  M=%d  K=%d  nprobe=%d  rerankK=%d

## Static Baseline (E set)
- recall@10 = %.4f
- mean proxy = %.0f ops

## Oracle Min-Depth (E set)
- recall@10  = %.4f  (queries where true NN retrievable at some grid depth)
- mean proxy = %.0f ops
- headroom vs static: %.1f%%  (oracle / static = %.3f)

## Feature–Depth Correlations (Pearson)
| Feature | Corr |
|---------|------|
| %s | %.4f |
| %s | %.4f |
| %s | %.4f |
| %s | %.4f |

max |corr| = %.4f

## Verdict
**%s**
`,
		len(base), dim, len(learn), eN, hN,
		cfg.Nlist, cfg.M, cfg.K, sp.Nprobe, sp.RerankK,
		meanRecallStatic, meanLatStatic,
		oracleRecall, oracleMeanLat,
		(1-oracleMeanLat/meanLatStatic)*100, oracleMeanLat/meanLatStatic,
		fNames[0], corrs[0],
		fNames[1], corrs[1],
		fNames[2], corrs[2],
		fNames[3], corrs[3],
		maxAbsCorr,
		verdict,
	)

	outPath := *outFlag + ".md"
	must(os.WriteFile(outPath, []byte(report), 0o644))
	fmt.Print(report)
	fmt.Printf("report written → %s\n", outPath)
}

// hitsToIDs converts a slice of ivf.Hit to base-vector ids via idByHash.
func hitsToIDs(hits []ivf.Hit, idByHash map[uint64]int32) []int32 {
	out := make([]int32, 0, len(hits))
	for _, h := range hits {
		if id, ok := idByHash[h.Handle.Hash]; ok {
			out = append(out, id)
		}
	}
	return out
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

// readFvecs reads a TEXMEX .fvecs file (same implementation as cmd/ivfverify).
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
