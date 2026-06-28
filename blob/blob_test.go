package blob

import (
	"bytes"
	"testing"
)

func TestPutGetDedupAndSweep(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	data := []byte("fake png bytes")
	sha1, err := s.Put(data)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(sha1) != 64 {
		t.Fatalf("sha must be 64 hex chars, got %q", sha1)
	}

	// Dedup: same bytes → same sha, written once.
	sha2, err := s.Put(data)
	if err != nil || sha2 != sha1 {
		t.Fatalf("dedup: got %q,%v want %q", sha2, err, sha1)
	}
	if list, _ := s.List(); len(list) != 1 {
		t.Fatalf("expected 1 blob after dedup, got %d", len(list))
	}

	// Round-trip.
	got, err := s.Get(sha1)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get round-trip: got %q,%v", got, err)
	}

	// Missing / invalid sha → ErrNotFound.
	if _, err := s.Get("deadbeef"); err != ErrNotFound {
		t.Fatalf("Get(invalid) = %v, want ErrNotFound", err)
	}

	// Sweep keeps the keep-set, deletes the rest.
	other, _ := s.Put([]byte("second blob"))
	n, err := s.Sweep(map[string]bool{sha1: true})
	if err != nil || n != 1 {
		t.Fatalf("Sweep deleted %d,%v want 1", n, err)
	}
	if _, err := s.Get(other); err != ErrNotFound {
		t.Fatal("swept blob should be gone")
	}
	if _, err := s.Get(sha1); err != nil {
		t.Fatal("kept blob should survive sweep")
	}
}
