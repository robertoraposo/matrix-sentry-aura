package main

import "testing"

func TestGridAllDecodesValid(t *testing.T) {
	grid := TunerGrid()
	var rec func(genome []int)
	count := 0
	rec = func(genome []int) {
		if len(genome) == len(grid) {
			p := DecodePheno(grid, genome)
			count++
			if 768%p.M != 0 {
				t.Fatalf("Dim%%M != 0 for %+v", p)
			}
			if p.M > 96 {
				t.Fatalf("M > 96 breaks the 32x compression class: %+v", p)
			}
			if p.K > 256 || p.K < 1 {
				t.Fatalf("K out of range: %+v", p)
			}
			if p.Nprobe < 1 || p.Nprobe > p.Nlist {
				t.Fatalf("nprobe out of [1, nlist]: %+v", p)
			}
			return
		}
		for i := range grid[len(genome)].Values {
			rec(append(genome, i))
		}
	}
	rec(nil)
	if count != 6*6*3*6*5 {
		t.Fatalf("enumerated %d genomes, want %d", count, 6*6*3*6*5)
	}
}

func TestNprobeIsFractionOfNlist(t *testing.T) {
	grid := TunerGrid()
	// gene order: nlist, m, k, probeDen, rerankk
	p := DecodePheno(grid, []int{2, 5, 2, 4, 0}) // nlist=64, den=4
	if p.Nlist != 64 || p.Nprobe != 16 {
		t.Fatalf("want nlist=64 nprobe=16, got %+v", p)
	}
	p = DecodePheno(grid, []int{0, 5, 2, 0, 0}) // nlist=16, den=64 -> clamp to 1
	if p.Nprobe != 1 {
		t.Fatalf("want clamped nprobe=1, got %+v", p)
	}
}

func TestPhenoKeyCollapsesDuplicatePhenotypes(t *testing.T) {
	grid := TunerGrid()
	a := DecodePheno(grid, []int{0, 5, 2, 0, 0}) // nlist=16, den=64 -> nprobe=1
	b := DecodePheno(grid, []int{0, 5, 2, 1, 0}) // nlist=16, den=32 -> nprobe=1
	if a.Key() != b.Key() {
		t.Fatalf("same phenotype must share a memo key: %q vs %q", a.Key(), b.Key())
	}
}

func TestBaselineGenomeDecodes(t *testing.T) {
	p := DecodePheno(TunerGrid(), BaselineGenome())
	want := Phenotype{Nlist: 64, M: 96, K: 256, Nprobe: 16, RerankK: 0}
	if p != want {
		t.Fatalf("baseline decodes to %+v, want %+v", p, want)
	}
}

func TestCostProxyMonotoneInWork(t *testing.T) {
	base := Phenotype{Nlist: 64, M: 96, K: 256, Nprobe: 16, RerankK: 0}
	n := 47000
	c := CostProxy(base, 768, n)
	for _, more := range []Phenotype{
		{Nlist: 64, M: 96, K: 256, Nprobe: 32, RerankK: 0},   // more probes
		{Nlist: 64, M: 96, K: 256, Nprobe: 16, RerankK: 200}, // rerank stage
		{Nlist: 512, M: 96, K: 256, Nprobe: 128, RerankK: 0}, // bigger coarse scan + probes
	} {
		if CostProxy(more, 768, n) <= c {
			t.Fatalf("proxy not monotone: %+v <= base", more)
		}
	}
	// exact full scan must dominate any feasible config's proxy budget check
	if c >= ExactScanProxy(768, n) {
		t.Fatalf("baseline proxy %f >= exact scan %f", c, ExactScanProxy(768, n))
	}
}
