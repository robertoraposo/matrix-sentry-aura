// Package blob is a content-addressed byte store on disk: each blob is keyed by
// the hex sha256 of its contents, written once (dedup), under <root>/blobs. It
// has no knowledge of tenants or the journal — callers gate access via a
// tenant-scoped reference. Used by Matrix Sentry to hold agent-shared images
// out of the in-RAM comms index and out of the 16 MiB-capped journal records.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// ErrNotFound is returned by Get when a blob is absent or the sha is malformed.
var ErrNotFound = errors.New("blob: not found")

type Store struct {
	dir string // <root>/blobs
	mu  sync.Mutex
}

// Open prepares <root>/blobs (creating it if needed) and returns the store.
func Open(root string) (*Store, error) {
	dir := filepath.Join(root, "blobs")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func validSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Put stores data and returns its hex sha256. If a blob with that sha already
// exists it is not rewritten (write-once dedup). Atomic via temp-file + rename.
func (s *Store) Put(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	path := filepath.Join(s.dir, sha)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(path); err == nil {
		return sha, nil // already stored
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return sha, nil
}

// Get returns the bytes for sha, or ErrNotFound if absent/malformed.
func (s *Store) Get(sha string) ([]byte, error) {
	if !validSHA(sha) {
		return nil, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(s.dir, sha))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

// Delete removes sha if present; absent/malformed is a no-op (idempotent).
func (s *Store) Delete(sha string) error {
	if !validSHA(sha) {
		return nil
	}
	err := os.Remove(filepath.Join(s.dir, sha))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// List returns every stored blob sha (skips in-progress temp files).
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if validSHA(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// Sweep deletes every stored blob whose sha is NOT in keep, returning the count
// deleted. This is the GC primitive; the keep-set comes from comms.LiveBlobIDs.
func (s *Store) Sweep(keep map[string]bool) (int, error) {
	shas, err := s.List()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, sha := range shas {
		if !keep[sha] {
			if err := s.Delete(sha); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}
