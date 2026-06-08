package sentry

import (
	"path/filepath"
	"testing"
)

// EventPathMap records the path→id dictionary entry the first time a path is
// seen, so the journal itself is the single source of truth for the mapping.
func TestPathMapPayloadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sentry")
	s, err := Open(dir, Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	seq, err := s.Append(1, EventPathMap, PathMapPayload{ID: 7, Path: "/repo/main.go"})
	if err != nil {
		t.Fatal(err)
	}

	rec, err := s.Read(seq)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Type != EventPathMap {
		t.Errorf("expected type EventPathMap, got %d", rec.Type)
	}
	var p PathMapPayload
	if err := UnmarshalPayload(rec.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.ID != 7 || p.Path != "/repo/main.go" {
		t.Errorf("round-trip mismatch: got id=%d path=%q", p.ID, p.Path)
	}
}

// AccessPayload gains an optional Source (the originating tool). New records
// carry it; old records (no "src" field) must still decode to an empty Source.
func TestAccessPayloadSource(t *testing.T) {
	withSrc := AccessPayload{ItemID: 42, Source: "Read"}
	b, err := MarshalPayload(withSrc)
	if err != nil {
		t.Fatal(err)
	}
	var got AccessPayload
	if err := UnmarshalPayload(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.ItemID != 42 || got.Source != "Read" {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// Back-compat: a payload written before Source existed.
	var old AccessPayload
	if err := UnmarshalPayload([]byte(`{"item":99}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.ItemID != 99 || old.Source != "" {
		t.Errorf("legacy decode mismatch: %+v", old)
	}
}
