package main

import "math"

// pca3 projects d-dimensional vectors to 3D via the top-3 principal components,
// found by power iteration on the implicit covariance action C·v = Xᵀ(X·v),
// orthogonalizing each candidate against the already-found components. Output is
// std-normalized per axis and scaled (~±15) to match the dashboard's visual
// scale. Deterministic: fixed init vectors. Returns one [3]float32 per input.
func pca3(vecs [][]float32) [][3]float32 {
	n := len(vecs)
	out := make([][3]float32, n)
	if n == 0 {
		return out
	}
	d := len(vecs[0])
	if d == 0 {
		return out
	}
	mean := make([]float64, d)
	X := make([][]float64, n)
	for i, v := range vecs {
		X[i] = make([]float64, d)
		for j := 0; j < d && j < len(v); j++ {
			X[i][j] = float64(v[j])
			mean[j] += X[i][j]
		}
	}
	for j := range mean {
		mean[j] /= float64(n)
	}
	for i := range X {
		for j := range X[i] {
			X[i][j] -= mean[j]
		}
	}
	comps := make([][]float64, 0, 3)
	for c := 0; c < 3; c++ {
		v := make([]float64, d)
		for j := range v {
			v[j] = math.Sin(float64((j+1)*(c+1))) + 0.1
		}
		normalize(v)
		for iter := 0; iter < 40; iter++ {
			Xv := make([]float64, n)
			for i := 0; i < n; i++ {
				s := 0.0
				for j := 0; j < d; j++ {
					s += X[i][j] * v[j]
				}
				Xv[i] = s
			}
			w := make([]float64, d)
			for i := 0; i < n; i++ {
				xv := Xv[i]
				for j := 0; j < d; j++ {
					w[j] += X[i][j] * xv
				}
			}
			for _, pc := range comps {
				dot := 0.0
				for j := 0; j < d; j++ {
					dot += w[j] * pc[j]
				}
				for j := 0; j < d; j++ {
					w[j] -= dot * pc[j]
				}
			}
			if normalize(w) == 0 {
				break
			}
			v = w
		}
		comps = append(comps, v)
	}
	var mom [3]float64
	for i := 0; i < n; i++ {
		for c := 0; c < 3 && c < len(comps); c++ {
			s := 0.0
			for j := 0; j < d; j++ {
				s += X[i][j] * comps[c][j]
			}
			out[i][c] = float32(s)
			mom[c] += s * s
		}
	}
	// Single global scale (by PC0's std) preserves the relative variance
	// ordering — PC0 is the widest spread — while bounding the cloud to a
	// sensible display size. (Per-axis normalization would flatten the
	// structure into a sphere and erase which axis carries the most variance.)
	const scale = 12.0
	std0 := math.Sqrt(mom[0] / float64(n))
	if std0 < 1e-9 {
		std0 = 1
	}
	factor := scale / std0
	for i := 0; i < n; i++ {
		for c := 0; c < 3; c++ {
			out[i][c] = float32(float64(out[i][c]) * factor)
		}
	}
	return out
}

func normalize(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x * x
	}
	n := math.Sqrt(s)
	if n < 1e-12 {
		return 0
	}
	for i := range v {
		v[i] /= n
	}
	return n
}

// kmeans clusters 3D points into k groups: deterministic farthest-point init +
// Lloyd iterations. Returns each point's cluster index and the cluster centers.
func kmeans(pts [][3]float32, k int) (assign []int, centers [][3]float32) {
	n := len(pts)
	assign = make([]int, n)
	if n == 0 || k <= 0 {
		return assign, nil
	}
	if k > n {
		k = n
	}
	centers = make([][3]float32, 0, k)
	centers = append(centers, pts[0])
	for len(centers) < k {
		best, bestD := -1, -1.0
		for i := 0; i < n; i++ {
			dmin := math.MaxFloat64
			for _, c := range centers {
				if dd := dist2(pts[i], c); dd < dmin {
					dmin = dd
				}
			}
			if dmin > bestD {
				bestD, best = dmin, i
			}
		}
		centers = append(centers, pts[best])
	}
	for iter := 0; iter < 25; iter++ {
		changed := false
		for i := 0; i < n; i++ {
			best, bestD := 0, math.MaxFloat64
			for c := range centers {
				if dd := dist2(pts[i], centers[c]); dd < bestD {
					bestD, best = dd, c
				}
			}
			if assign[i] != best {
				assign[i] = best
				changed = true
			}
		}
		sums := make([][3]float64, len(centers))
		cnts := make([]int, len(centers))
		for i := 0; i < n; i++ {
			a := assign[i]
			sums[a][0] += float64(pts[i][0])
			sums[a][1] += float64(pts[i][1])
			sums[a][2] += float64(pts[i][2])
			cnts[a]++
		}
		for c := range centers {
			if cnts[c] > 0 {
				centers[c] = [3]float32{
					float32(sums[c][0] / float64(cnts[c])),
					float32(sums[c][1] / float64(cnts[c])),
					float32(sums[c][2] / float64(cnts[c])),
				}
			}
		}
		if !changed && iter > 0 {
			break
		}
	}
	return assign, centers
}

func dist2(a, b [3]float32) float64 {
	dx := float64(a[0] - b[0])
	dy := float64(a[1] - b[1])
	dz := float64(a[2] - b[2])
	return dx*dx + dy*dy + dz*dz
}
