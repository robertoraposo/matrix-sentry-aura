package main

import (
	"fmt"

	"matrixsentry/internal/evolve"
)

// Phenotype is a decoded engine configuration: the geometry (Nlist, M, K —
// the expensive, trainable part) plus the search knobs (Nprobe, RerankK).
type Phenotype struct {
	Nlist, M, K, Nprobe, RerankK int
}

// TunerGrid is the production genome: 5 ordinal genes, valid by construction
// (panel-reconciled design, 2026-06-12):
//   - M = divisors of 768 capped at 96, so Dim%M==0 AND bytes/vec = M+4 ≤ 100
//     (the ≥32x compression class) hold without any repair or penalty. Codes
//     are 1 uint8 per subcode, so K<256 saves ZERO storage — K only trades
//     table-build cost against quantization error.
//   - nprobe is encoded as a FRACTION of nlist (denominator grid): "1/4 of the
//     cells" keeps its meaning when an nlist mutation changes the genome,
//     where an absolute nprobe gene would silently change semantics.
//   - rerankK is a NEW exact-re-rank stage (lab.SqL2 against the float32
//     originals over the ADC top-rerankK shortlist) — the data-picked next
//     lever from the cachebench diag, distinct from cachebench's -rerankk
//     shortlist-length flag.
func TunerGrid() []evolve.Gene {
	return []evolve.Gene{
		{Name: "nlist", Values: []int{16, 32, 64, 128, 256, 512}},
		{Name: "m", Values: []int{16, 24, 32, 48, 64, 96}},
		{Name: "k", Values: []int{64, 128, 256}},
		{Name: "probeDen", Values: []int{64, 32, 16, 8, 4, 2}},
		{Name: "rerankk", Values: []int{0, 50, 100, 200, 400}},
	}
}

// BaselineGenome encodes the hand-picked Scenario-B config
// (nlist=64, M=96, K=256, nprobe=16 i.e. 1/4 of cells, no re-rank).
func BaselineGenome() []int { return []int{2, 5, 2, 4, 0} }

// DecodePheno maps gene indices to a Phenotype. nprobe = nlist/probeDen,
// clamped to [1, nlist] — a deterministic decode, never a stochastic repair.
func DecodePheno(grid []evolve.Gene, genome []int) Phenotype {
	m := evolve.Decode(grid, genome)
	nlist := m["nlist"]
	nprobe := nlist / m["probeDen"]
	if nprobe < 1 {
		nprobe = 1
	}
	if nprobe > nlist {
		nprobe = nlist
	}
	return Phenotype{Nlist: nlist, M: m["m"], K: m["k"], Nprobe: nprobe, RerankK: m["rerankk"]}
}

// Key is the phenotype memo key. Distinct gene vectors can decode to the same
// phenotype (e.g. two denominators both clamping to nprobe=1), so caches key
// on the DECODED config, never the raw genes.
func (p Phenotype) Key() string {
	return fmt.Sprintf("%d-%d-%d-%d-%d", p.Nlist, p.M, p.K, p.Nprobe, p.RerankK)
}

// GeomKey identifies the trainable part only: genomes differing in search
// knobs share one trained geometry.
func (p Phenotype) GeomKey() string { return fmt.Sprintf("%d-%d-%d", p.Nlist, p.M, p.K) }

// CostProxy is the deterministic per-query work estimate (op count) that
// stands in for latency inside the GA — wall-clock on a shared box is noisy,
// non-deterministic, and would poison the fitness memo. Terms:
//
//	coarse scan      2·dim·nlist      (ProbeCells scores ALL centroids)
//	ADC table build  2·dim·K·nprobe   (per probed cell: K sub-distances over dim)
//	ADC adds         2·M·scanned      (table lookups over scanned codes)
//	exact re-rank    2·dim·rerankK    (SqL2 against RAM-resident originals)
//
// scanned uses the balanced-list estimate n·nprobe/nlist so the proxy is a
// pure function of the phenotype (memo- and resume-sound). List imbalance can
// make true work higher; final validation wall-clocks the finalists and
// enforces measured feasibility, which catches any proxy-cheating champion.
func CostProxy(p Phenotype, dim, n int) float64 {
	estScanned := float64(n) * float64(p.Nprobe) / float64(p.Nlist)
	return 2*float64(dim)*float64(p.Nlist) +
		2*float64(dim)*float64(p.K)*float64(p.Nprobe) +
		2*float64(p.M)*estScanned +
		2*float64(dim)*float64(p.RerankK)
}

// ExactScanProxy is the work of brute-force exact search — if Lmax admits it,
// the run is degenerate (the GA would just converge to exact search).
func ExactScanProxy(dim, n int) float64 { return 2 * float64(dim) * float64(n) }
