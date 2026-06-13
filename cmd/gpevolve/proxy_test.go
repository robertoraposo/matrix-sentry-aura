package main

import "testing"

func TestSnapToGrid(t *testing.T) {
	if got := snap(nprobeGrid, 30); got != 32 {
		t.Fatalf("snap nprobe 30 -> %d, want 32", got)
	}
	if got := snap(rerankGrid, 7); got != 0 {
		t.Fatalf("snap rerank 7 -> %d, want 0", got)
	}
	if got := snap(nprobeGrid, 1000); got != nprobeGrid[len(nprobeGrid)-1] {
		t.Fatalf("snap above range -> %d, want max", got)
	}
	if got := snap(nprobeGrid, -5); got != nprobeGrid[0] {
		t.Fatalf("snap below range -> %d, want min", got)
	}
}

func TestQueryProxyMonotonic(t *testing.T) {
	cheap := queryProxy(8, 0, 768, 47000, 64)
	mid := queryProxy(32, 0, 768, 47000, 64)
	deep := queryProxy(32, 200, 768, 47000, 64)
	if !(cheap < mid && mid < deep) {
		t.Fatalf("proxy not monotonic: %.1f %.1f %.1f", cheap, mid, deep)
	}
}

func TestRecallHit(t *testing.T) {
	if recallHit([]int32{1, 5, 7}, 5) != 1.0 {
		t.Fatal("true NN present should score 1.0")
	}
	if recallHit([]int32{1, 7, 3}, 5) != 0.0 {
		t.Fatal("true NN absent should score 0.0")
	}
}

func TestPearson(t *testing.T) {
	// perfectly correlated
	xs := []float64{1, 2, 3, 4, 5}
	ys := []float64{2, 4, 6, 8, 10}
	if r := pearson(xs, ys); r < 0.999 {
		t.Fatalf("pearson perfectly correlated: got %.4f, want ~1.0", r)
	}
	// perfectly anti-correlated
	ys2 := []float64{10, 8, 6, 4, 2}
	if r := pearson(xs, ys2); r > -0.999 {
		t.Fatalf("pearson anti-correlated: got %.4f, want ~-1.0", r)
	}
	// constant y => 0
	ys3 := []float64{5, 5, 5, 5, 5}
	if r := pearson(xs, ys3); r != 0 {
		t.Fatalf("pearson constant y: got %.4f, want 0", r)
	}
}
