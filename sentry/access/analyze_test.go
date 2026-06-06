package access

import (
	"matrixsentry/sentry"
	"path/filepath"
	"testing"
)

func TestAnalyzeStructuredStream(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "access")
	s, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Sequence: 1->2, 2->1, 1->2, 2->1 ... (Perfectly predictable by Markov, hard for Marginal)
	tenant := sentry.TenantID(1)
	for i := 0; i < 50; i++ {
		s.Append(tenant, sentry.EventAccess, sentry.AccessPayload{ItemID: uint64(1 + (i % 2))})
	}

	report, err := Analyze(s, tenant)
	if err != nil {
		t.Fatal(err)
	}

	if report.MarkovHit < 0.9 {
		t.Errorf("expected high MarkovHit for structured stream, got %.4f", report.MarkovHit)
	}
	if report.Lift <= 0 {
		t.Errorf("expected positive Lift for structured stream, got %.4f", report.Lift)
	}
}

func TestAnalyzeRandomStream(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "random")
	s, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// High cardinality random stream -> Lift should be near zero or negative
	tenant := sentry.TenantID(1)
	for i := 0; i < 100; i++ {
		s.Append(tenant, sentry.EventAccess, sentry.AccessPayload{ItemID: uint64(i)})
	}

	report, err := Analyze(s, tenant)
	if err != nil {
		t.Fatal(err)
	}

	if report.Lift > 0.1 {
		t.Errorf("expected low/zero Lift for random stream, got %.4f", report.Lift)
	}
}
