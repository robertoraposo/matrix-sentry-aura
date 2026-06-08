package main

import (
	"path/filepath"
	"reflect"
	"testing"

	"matrixsentry/internal/lab"
)

// writeIvecs must produce TEXMEX .ivecs that lab.ReadIvecs reads back exactly,
// so the emitted ground truth plugs straight into ivfsweep/ivfpredict.
func TestWriteIvecsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gt.ivecs")
	gt := [][]int32{
		{5, 9, 2, 7},
		{1, 0, 3, 4},
	}
	writeIvecs(path, gt)

	got := lab.ReadIvecs(path)
	if !reflect.DeepEqual(got, gt) {
		t.Errorf("round-trip mismatch:\n got %v\nwant %v", got, gt)
	}
}
