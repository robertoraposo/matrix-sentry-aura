package ivf

import "testing"

func TestHashVecDeterministicAndDistinct(t *testing.T) {
	a := []float32{1, 2, 3, 4}
	b := []float32{1, 2, 3, 4}
	c := []float32{1, 2, 3, 5}

	if hashVec(a) != hashVec(b) {
		t.Fatal("hashVec must be identical for identical vectors")
	}
	if hashVec(a) == hashVec(c) {
		t.Fatal("hashVec must differ for different vectors")
	}
}

func TestHashVecExportedMatchesAddedHandle(t *testing.T) {
	if HashVec([]float32{1, 2, 3, 4}) != hashVec([]float32{1, 2, 3, 4}) {
		t.Fatal("HashVec must equal the internal hashVec")
	}
	ix, err := New(Config{Dim: 4, Nlist: 2, M: 2, K: 4, Iter: 5, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.Train([][]float32{{0, 0, 0, 0}, {9, 9, 9, 9}, {1, 1, 1, 1}, {8, 8, 8, 8}}); err != nil {
		t.Fatal(err)
	}
	v := []float32{5, 6, 7, 8}
	res := ix.Add([][]float32{v})
	if res[0].Handle.Hash != HashVec(v) {
		t.Fatal("HashVec must equal the Handle.Hash the index assigned (so callers can build a fetch map)")
	}
}

func TestHammingBytesCountsDifferingSubquantizers(t *testing.T) {
	if g := hammingBytes([]uint8{1, 2, 3, 4}, []uint8{1, 9, 3, 9}); g != 2 {
		t.Fatalf("hammingBytes = %d, want 2", g)
	}
	if g := hammingBytes([]uint8{5, 5}, []uint8{5, 5}); g != 0 {
		t.Fatalf("hammingBytes identical = %d, want 0", g)
	}
}

func TestCodeKeyDistinguishesCellAndCode(t *testing.T) {
	if codeKey(0, []uint8{1, 2}) == codeKey(1, []uint8{1, 2}) {
		t.Fatal("codeKey must distinguish different cells")
	}
	if codeKey(0, []uint8{1, 2}) == codeKey(0, []uint8{1, 3}) {
		t.Fatal("codeKey must distinguish different codes")
	}
	if codeKey(0, []uint8{1, 2}) != codeKey(0, []uint8{1, 2}) {
		t.Fatal("codeKey must be stable for the same input")
	}
}
