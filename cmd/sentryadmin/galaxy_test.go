package main

import (
	"math"
	"testing"
)

func TestPCA3OrdersByVariance(t *testing.T) {
	vecs := make([][]float32, 200)
	for i := range vecs {
		v := make([]float32, 5)
		t01 := float32(i) / 200.0
		v[0] = (t01 - 0.5) * 100 // dominant axis
		v[1] = float32(math.Sin(float64(i))) * 0.1
		vecs[i] = v
	}
	pos := pca3(vecs)
	if len(pos) != 200 {
		t.Fatalf("want 200 projected points, got %d", len(pos))
	}
	var v0, v1 float64
	for _, p := range pos {
		v0 += float64(p[0]) * float64(p[0])
		v1 += float64(p[1]) * float64(p[1])
	}
	if !(v0 > v1*5) {
		t.Fatalf("PC0 variance (%.3f) should dominate PC1 (%.3f)", v0, v1)
	}
	for _, p := range pos {
		for _, c := range p {
			if math.IsNaN(float64(c)) || math.IsInf(float64(c), 0) {
				t.Fatal("projection produced non-finite coordinate")
			}
		}
	}
}

func TestKMeansSeparatesBlobs(t *testing.T) {
	var pts [][3]float32
	truth := []int{}
	centers := [][3]float32{{0, 0, 0}, {50, 0, 0}, {0, 50, 50}}
	for c, ctr := range centers {
		for j := 0; j < 30; j++ {
			off := float32(j%5) * 0.2
			pts = append(pts, [3]float32{ctr[0] + off, ctr[1] + off, ctr[2] + off})
			truth = append(truth, c)
		}
	}
	assign, cs := kmeans(pts, 3)
	if len(cs) != 3 || len(assign) != len(pts) {
		t.Fatalf("bad shapes: %d centers, %d assigns", len(cs), len(assign))
	}
	for c := 0; c < 3; c++ {
		first := -1
		for i := range pts {
			if truth[i] != c {
				continue
			}
			if first == -1 {
				first = assign[i]
			} else if assign[i] != first {
				t.Fatalf("blob %d split across clusters", c)
			}
		}
	}
	assign2, _ := kmeans(pts, 3)
	for i := range assign {
		if assign[i] != assign2[i] {
			t.Fatal("kmeans not deterministic")
		}
	}
}
