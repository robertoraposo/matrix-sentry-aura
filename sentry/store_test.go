package sentry

import (
	"path/filepath"
	"testing"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sentry")
	s, err := Open(dir, Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	payload := AccessPayload{ItemID: 123}
	seq, err := s.Append(1, EventAccess, payload)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 {
		t.Errorf("expected seq 1, got %d", seq)
	}

	rec, err := s.Read(seq)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Tenant != 1 {
		t.Errorf("expected tenant 1, got %d", rec.Tenant)
	}

	var p2 AccessPayload
	if err := UnmarshalPayload(rec.Payload, &p2); err != nil {
		t.Fatal(err)
	}
	if p2.ItemID != 123 {
		t.Errorf("expected item 123, got %d", p2.ItemID)
	}
}

func TestStoreIsolation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sentry")
	s, err := Open(dir, Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.Append(1, EventAccess, AccessPayload{ItemID: 101})
	s.Append(2, EventAccess, AccessPayload{ItemID: 202})
	s.Append(1, EventAccess, AccessPayload{ItemID: 103})

	tenant1 := TenantID(1)
	var items []uint64
	s.Scan(Filter{Tenant: &tenant1}, func(r Record) bool {
		var p AccessPayload
		UnmarshalPayload(r.Payload, &p)
		items = append(items, p.ItemID)
		return true
	})

	if len(items) != 2 || items[0] != 101 || items[1] != 103 {
		t.Errorf("unexpected items for tenant 1: %v", items)
	}
}

func TestScanReverseNewestFirstAndStops(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sentry")
	st, err := Open(dir, Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := 0; i < 5; i++ {
		if _, err := st.Append(1, EventAccess, AccessPayload{ItemID: uint64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	var seqs []Seq
	st.ScanReverse(Filter{}, func(r Record) bool { seqs = append(seqs, r.Seq); return true })
	if len(seqs) != 5 || seqs[0] != 5 || seqs[4] != 1 {
		t.Fatalf("want 5..1 newest-first, got %v", seqs)
	}
	calls := 0
	var got []Seq
	st.ScanReverse(Filter{}, func(r Record) bool {
		calls++
		got = append(got, r.Seq)
		return len(got) < 2
	})
	if len(got) != 2 || got[0] != 5 || calls != 2 {
		t.Fatalf("early stop failed: got=%v calls=%d", got, calls)
	}
}

func TestStoreRecovery(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sentry")
	s, err := Open(dir, Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	s.Append(1, EventAccess, AccessPayload{ItemID: 1})
	s.Append(1, EventAccess, AccessPayload{ItemID: 2})
	s.Close()

	s2, err := Open(dir, Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if s2.nextSeq != 3 {
		t.Errorf("expected nextSeq 3, got %d", s2.nextSeq)
	}
	rec, err := s2.Read(2)
	if err != nil {
		t.Fatal(err)
	}
	var p AccessPayload
	UnmarshalPayload(rec.Payload, &p)
	if p.ItemID != 2 {
		t.Errorf("expected item 2, got %d", p.ItemID)
	}
}
