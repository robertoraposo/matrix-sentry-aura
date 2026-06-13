package ivf

import (
	"encoding/binary"
	"hash/fnv"
	"math"
)

// HashVec exposes the index's content-identity hash so a caller can key its own
// originals store by the same value Add/Search put on Handle.Hash — e.g. to
// build the fetch map SearchRerank reads. It is exactly the internal hashVec.
func HashVec(v []float32) uint64 { return hashVec(v) }

// hashVec is the FNV-1a hash of a vector's raw float32 bytes — an exact,
// order-stable identity. Identical vectors hash identically; any differing
// element changes the hash. This is the primary key of the identity ladder.
func hashVec(v []float32) uint64 {
	h := fnv.New64a()
	var buf [4]byte
	for _, x := range v {
		binary.LittleEndian.PutUint32(buf[:], math.Float32bits(x))
		h.Write(buf[:])
	}
	return h.Sum64()
}

// hammingBytes counts how many subquantizers two PQ codes disagree on (per-byte
// Hamming). It is the tolerance metric of the identity ladder: tol=0 means the
// same code, tol=k allows k differing subquantizers. Lengths must match.
func hammingBytes(a, b []uint8) int {
	n := 0
	for i := range a {
		if a[i] != b[i] {
			n++
		}
	}
	return n
}

// codeKey is a hashable key for the (cell, code) quantized fingerprint, used for
// the cheap exact-collision map. Not a primary key — codes are lossy.
func codeKey(cell uint32, code []uint8) string {
	b := make([]byte, 4+len(code))
	binary.LittleEndian.PutUint32(b[:4], cell)
	copy(b[4:], code)
	return string(b)
}
