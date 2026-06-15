package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryOwnerAndTeams(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "tokens.json")
	os.WriteFile(f, []byte(`[{"secret":"team-acme-sec","tenant":2,"label":"acme"},{"secret":"team-bolt-sec","tenant":3,"label":"bolt"}]`), 0o600)

	reg, err := loadTokenRegistry(f, "owner-sec", 1)
	if err != nil {
		t.Fatal(err)
	}
	if tn, ok := reg.Tenant("owner-sec"); !ok || tn != 1 {
		t.Fatalf("owner → (%d,%v), want (1,true)", tn, ok)
	}
	if tn, ok := reg.Tenant("team-acme-sec"); !ok || tn != 2 {
		t.Fatalf("acme → (%d,%v), want (2,true)", tn, ok)
	}
	if tn, ok := reg.Tenant("team-bolt-sec"); !ok || tn != 3 {
		t.Fatalf("bolt → (%d,%v), want (3,true)", tn, ok)
	}
	if _, ok := reg.Tenant("nope"); ok {
		t.Fatal("unknown secret must be (_, false)")
	}
}

func TestRegistryStandaloneNoFile(t *testing.T) {
	reg, err := loadTokenRegistry("", "owner-sec", 1)
	if err != nil {
		t.Fatal(err)
	}
	if tn, ok := reg.Tenant("owner-sec"); !ok || tn != 1 {
		t.Fatalf("owner → (%d,%v), want (1,true)", tn, ok)
	}
	if _, ok := reg.Tenant("anything-else"); ok {
		t.Fatal("standalone registry must only know the owner secret")
	}
}

func TestRegistryRejectsBadEntries(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{
		`[{"secret":"x","tenant":0,"label":"z"}]`,
		`[{"secret":"owner-sec","tenant":9,"label":"dup"}]`,
	} {
		f := filepath.Join(dir, "t.json")
		os.WriteFile(f, []byte(bad), 0o600)
		if _, err := loadTokenRegistry(f, "owner-sec", 1); err == nil {
			t.Fatalf("expected load error for %s", bad)
		}
	}
}
