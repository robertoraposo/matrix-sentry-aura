package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"os"

	"matrixsentry/sentry"
)

// tokenEntry is one credential→tenant mapping (a secret usable as a static
// bearer or an OAuth consent passphrase, and the tenant it resolves to).
type tokenEntry struct {
	Secret string          `json:"secret"`
	Tenant sentry.TenantID `json:"tenant"`
	Label  string          `json:"label"`
}

// tokenRegistry resolves a credential to its tenant. The owner secret
// (SENTRY_MCP_TOKEN) is always present → the server's default tenant; extra
// teams come from tokens.json. With no file it holds only the owner entry, so
// behavior is identical to the single-tenant server.
type tokenRegistry struct {
	entries []tokenEntry
}

// loadTokenRegistry builds the registry: the owner entry (ownerSecret →
// ownerTenant) plus, if path != "", the JSON array of team entries. It fails
// fast on a zero tenant id or a team secret that duplicates the owner's, so a
// misconfig can never silently route a team into the owner's tenant.
func loadTokenRegistry(path, ownerSecret string, ownerTenant sentry.TenantID) (*tokenRegistry, error) {
	r := &tokenRegistry{}
	if ownerSecret != "" {
		r.entries = append(r.entries, tokenEntry{Secret: ownerSecret, Tenant: ownerTenant, Label: "owner"})
	}
	if path == "" {
		return r, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tokens file: %w", err)
	}
	var teams []tokenEntry
	if err := json.Unmarshal(data, &teams); err != nil {
		return nil, fmt.Errorf("tokens file: parse: %w", err)
	}
	for _, e := range teams {
		if e.Secret == "" || e.Tenant == 0 {
			return nil, fmt.Errorf("tokens file: entry %q has empty secret or tenant 0", e.Label)
		}
		if ownerSecret != "" && e.Secret == ownerSecret {
			return nil, fmt.Errorf("tokens file: entry %q reuses the owner secret", e.Label)
		}
		r.entries = append(r.entries, e)
	}
	return r, nil
}

// Tenant returns the tenant for a secret (constant-time per entry to avoid
// leaking which secret matched via timing), or (_, false) if unknown.
func (r *tokenRegistry) Tenant(secret string) (sentry.TenantID, bool) {
	if secret == "" {
		return 0, false
	}
	for _, e := range r.entries {
		if subtle.ConstantTimeCompare([]byte(secret), []byte(e.Secret)) == 1 {
			return e.Tenant, true
		}
	}
	return 0, false
}
