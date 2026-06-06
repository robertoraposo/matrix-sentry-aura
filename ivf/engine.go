package ivf

import (
	"sort"

	"matrixsentry/pq"
)

// probeCells returns the indices of the nprobe nearest coarse cells to query,
// nearest first. Ties resolve to the lower cell index (deterministic).
func probeCells(coarse [][]float32, query []float32, nprobe int) []int {
	type cd struct {
		c int
		d float64
	}
	cds := make([]cd, len(coarse))
	for c := range coarse {
		cds[c] = cd{c, sqL2(coarse[c], query)}
	}
	sort.Slice(cds, func(a, b int) bool {
		if cds[a].d != cds[b].d {
			return cds[a].d < cds[b].d
		}
		return cds[a].c < cds[b].c
	})
	if nprobe > len(cds) {
		nprobe = len(cds)
	}
	out := make([]int, nprobe)
	for i := 0; i < nprobe; i++ {
		out[i] = cds[i].c
	}
	return out
}

// buildADCTable builds the per-cell ADC lookup table from a residual, reading
// the frozen pq package's exported codebooks. table[m*K + c] = squared distance
// from residual subvector m to centroid c of subspace m.
func buildADCTable(q *pq.PQ, residual []float32) []float32 {
	table := make([]float32, q.M*q.K)
	for m := 0; m < q.M; m++ {
		start := m * q.Dsub
		sv := residual[start : start+q.Dsub]
		cb := q.Codebooks[m]
		base := m * q.K
		for c := 0; c < q.K; c++ {
			table[base+c] = float32(sqL2(sv, cb[c]))
		}
	}
	return table
}

// trainResidual learns the coarse quantizer and a residual PQ from a learn set,
// applying FAISS-style subsampling per opts. It is the shared core of both the
// prototype IVFADC.Train path and the production Index.Train path.
func trainResidual(learn [][]float32, nlist, M, K, iter int, seed int64, opts TrainOpts) ([][]float32, *pq.PQ, error) {
	d := len(learn[0])

	coarseIter := iter
	if opts.CoarseIter > 0 {
		coarseIter = opts.CoarseIter
	}
	coarseLearn := learn
	if opts.CoarseSample > 0 {
		coarseLearn = subsample(learn, opts.CoarseSample, seed+1)
	}
	coarse := coarseKMeans(coarseLearn, nlist, coarseIter, seed)

	residuals := make([][]float32, len(learn))
	for i, v := range learn {
		residuals[i] = sub(v, coarse[nearest(coarse, v)])
	}
	pqLearn := residuals
	if opts.PQSample > 0 {
		pqLearn = subsample(residuals, opts.PQSample, seed+2)
	}

	q, err := pq.New(d, M, K)
	if err != nil {
		return nil, nil, err
	}
	if err := q.Train(pqLearn, iter, seed); err != nil {
		return nil, nil, err
	}
	return coarse, q, nil
}
