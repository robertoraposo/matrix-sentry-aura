package providerbroker

import (
	"reflect"
	"testing"

	"matrixsentry/sentry"
)

func TestRegistryListsProvidersSorted(t *testing.T) {
	r := NewRegistry()

	if err := r.Register(Provider{
		ID:           "ollama",
		Name:         "Ollama local",
		Auth:         AuthNone,
		Capabilities: []string{"chat"},
	}); err != nil {
		t.Fatal(err)
	}

	if err := r.Register(Provider{
		ID:           "codex",
		Name:         "Codex CLI",
		Auth:         AuthCLI,
		Capabilities: []string{"chat", "tools"},
	}); err != nil {
		t.Fatal(err)
	}

	got := r.List()
	wantIDs := []string{"codex", "ollama"}

	var gotIDs []string
	for _, p := range got {
		gotIDs = append(gotIDs, p.ID)
	}

	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("provider ids = %v, want %v", gotIDs, wantIDs)
	}
}

func TestRegistryRejectsDuplicateProvider(t *testing.T) {
	r := NewRegistry()

	p := Provider{ID: "ollama", Name: "Ollama", Auth: AuthNone}

	if err := r.Register(p); err != nil {
		t.Fatal(err)
	}

	if err := r.Register(p); err == nil {
		t.Fatal("duplicate provider accepted")
	}
}

func TestStatusIsIsolatedByTenant(t *testing.T) {
	r := NewRegistry()

	if err := r.Register(Provider{
		ID:   "codex",
		Name: "Codex CLI",
		Auth: AuthCLI,
	}); err != nil {
		t.Fatal(err)
	}

	tenantA := sentry.TenantID(1)
	tenantB := sentry.TenantID(2)

	if err := r.SetStatus(tenantA, "codex", Status{
		State:   StateConnected,
		Account: "Roberto",
	}); err != nil {
		t.Fatal(err)
	}

	a, ok := r.Status(tenantA, "codex")
	if !ok || a.State != StateConnected || a.Account != "Roberto" {
		t.Fatalf("tenant A status = %+v, ok=%v", a, ok)
	}

	b, ok := r.Status(tenantB, "codex")
	if !ok || b.State != StateDisconnected || b.Account != "" {
		t.Fatalf("tenant B inherited tenant A status: %+v, ok=%v", b, ok)
	}
}

func TestSetStatusRejectsUnknownProvider(t *testing.T) {
	r := NewRegistry()

	err := r.SetStatus(sentry.TenantID(1), "missing", Status{
		State: StateConnected,
	})

	if err == nil {
		t.Fatal("status accepted for unknown provider")
	}
}
