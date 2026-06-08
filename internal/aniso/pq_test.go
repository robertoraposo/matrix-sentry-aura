package aniso

import (
	"math"
	"math/rand"
	"testing"
)

func synth(n, d int, seed int64) [][]float32 {
	rng := rand.New(rand.NewSource(seed))
	out := make([][]float32, n)
	for i := range out {
		out[i] = make([]float32, d)
		for j := range out[i] {
			out[i][j] = float32(rng.NormFloat64())
		}
	}
	return out
}

func meanSqErr(vecs [][]float32, a *APQ) float64 {
	var tot float64
	for _, v := range vecs {
		r := a.Reconstruct(a.Encode(v))
		for j := range v {
			e := float64(v[j]) - float64(r[j])
			tot += e * e
		}
	}
	return tot / float64(len(vecs))
}

// Training then encode/reconstruct must capture real structure: reconstruction
// error well below the raw vector energy.
func TestAPQReducesError(t *testing.T) {
	vecs := synth(800, 16, 5)
	a := Train(vecs, 4, 16, 15, 1.0, 1)
	mse := meanSqErr(vecs, a)
	// raw energy per vector ≈ d (unit-variance gaussian) = 16
	if mse > 12 {
		t.Errorf("reconstruction MSE %.3f too high (should be well under raw ~16)", mse)
	}
	if math.IsNaN(mse) {
		t.Fatal("NaN reconstruction")
	}
}

func TestAPQDeterministic(t *testing.T) {
	vecs := synth(400, 16, 9)
	a := Train(vecs, 4, 16, 10, 2.0, 7)
	b := Train(vecs, 4, 16, 10, 2.0, 7)
	for i := range vecs {
		ca, cb := a.Encode(vecs[i]), b.Encode(vecs[i])
		for j := range ca {
			if ca[j] != cb[j] {
				t.Fatalf("same seed gave different code at vec %d sub %d", i, j)
			}
		}
	}
}
