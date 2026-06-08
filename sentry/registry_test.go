package sentry

import (
	"path/filepath"
	"testing"
)

func itemsFor(t *testing.T, s *Store, tenant TenantID) []AccessPayload {
	t.Helper()
	var out []AccessPayload
	etype := EventAccess
	s.Scan(Filter{Tenant: &tenant, Type: &etype}, func(r Record) bool {
		var p AccessPayload
		if err := UnmarshalPayload(r.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
		return true
	})
	return out
}

// New paths get sequential ids starting at 1; a repeated path returns its
// existing id; every Record call appends an access carrying the source tool.
func TestRegistryAssignsSequentialIds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sentry")
	s, err := Open(dir, Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	reg, err := NewRegistry(s)
	if err != nil {
		t.Fatal(err)
	}

	id1, _, _ := reg.Record(1, "/repo/a.go", "Read")
	id2, _, _ := reg.Record(1, "/repo/b.go", "Edit")
	id1again, _, _ := reg.Record(1, "/repo/a.go", "Bash")

	if id1 != 1 || id2 != 2 {
		t.Errorf("expected sequential ids 1,2; got %d,%d", id1, id2)
	}
	if id1again != id1 {
		t.Errorf("repeated path should reuse id %d, got %d", id1, id1again)
	}

	items := itemsFor(t, s, 1)
	if len(items) != 3 {
		t.Fatalf("expected 3 access events, got %d", len(items))
	}
	if items[0].ItemID != 1 || items[0].Source != "Read" {
		t.Errorf("access[0] = %+v", items[0])
	}
	if items[2].ItemID != 1 || items[2].Source != "Bash" {
		t.Errorf("access[2] = %+v", items[2])
	}
}

// After a restart, the registry rebuilds path→id from the journal so ids stay
// stable and new paths continue the sequence.
func TestRegistryReloadsFromJournal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sentry")
	s, err := Open(dir, Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := NewRegistry(s)
	reg.Record(1, "/repo/a.go", "Read")
	reg.Record(1, "/repo/b.go", "Read")
	s.Close()

	s2, err := Open(dir, Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	reg2, err := NewRegistry(s2)
	if err != nil {
		t.Fatal(err)
	}

	if id, _, _ := reg2.Record(1, "/repo/a.go", "Read"); id != 1 {
		t.Errorf("expected stable id 1 for a.go after reload, got %d", id)
	}
	if id, _, _ := reg2.Record(1, "/repo/c.go", "Read"); id != 3 {
		t.Errorf("expected new path to continue at id 3, got %d", id)
	}
}

// Each tenant has an independent id space.
func TestRegistryPerTenant(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sentry")
	s, err := Open(dir, Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	reg, _ := NewRegistry(s)

	id1, _, _ := reg.Record(1, "/repo/a.go", "Read")
	id2, _, _ := reg.Record(2, "/repo/a.go", "Read")
	if id1 != 1 || id2 != 1 {
		t.Errorf("each tenant should start ids at 1; got t1=%d t2=%d", id1, id2)
	}
}
