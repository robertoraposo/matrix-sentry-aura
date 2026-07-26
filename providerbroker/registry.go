// Package providerbroker manages AI provider definitions and tenant-isolated
// connection state. It stores metadata only; credentials and provider processes
// belong to the later sentryproviderd service.
package providerbroker

import (
	"errors"
	"sort"
	"strings"
	"sync"

	"matrixsentry/sentry"
)

type AuthMethod string

const (
	AuthNone  AuthMethod = "none"
	AuthCLI   AuthMethod = "cli"
	AuthOAuth AuthMethod = "oauth"
)

type State string

const (
	StateDisconnected State = "disconnected"
	StateConnecting   State = "connecting"
	StateConnected    State = "connected"
	StateError        State = "error"
)

type Provider struct {
	ID           string
	Name         string
	Auth         AuthMethod
	Capabilities []string
}

type Status struct {
	State   State
	Account string
}

var (
	ErrInvalidProvider = errors.New("providerbroker: invalid provider")
	ErrDuplicate       = errors.New("providerbroker: provider already registered")
	ErrUnknownProvider = errors.New("providerbroker: unknown provider")
)

type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
	defaults  map[string]Status
	statuses  map[sentry.TenantID]map[string]Status
}

func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[string]Provider),
		defaults:  make(map[string]Status),
		statuses:  make(map[sentry.TenantID]map[string]Status),
	}
}

func (r *Registry) Register(p Provider) error {
	p.ID = strings.TrimSpace(p.ID)
	p.Name = strings.TrimSpace(p.Name)

	if p.ID == "" || p.Name == "" {
		return ErrInvalidProvider
	}

	p.Capabilities = append([]string(nil), p.Capabilities...)

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.providers[p.ID]; exists {
		return ErrDuplicate
	}

	r.providers[p.ID] = p
	return nil
}

func (r *Registry) List() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Provider, 0, len(r.providers))
	for _, p := range r.providers {
		p.Capabilities = append([]string(nil), p.Capabilities...)
		out = append(out, p)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})

	return out
}

func (r *Registry) SetDefaultStatus(
	providerID string,
	status Status,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.providers[providerID]; !exists {
		return ErrUnknownProvider
	}

	if status.State == "" {
		status.State = StateDisconnected
	}

	r.defaults[providerID] = status
	return nil
}

func (r *Registry) SetStatus(
	tenant sentry.TenantID,
	providerID string,
	status Status,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.providers[providerID]; !exists {
		return ErrUnknownProvider
	}

	if status.State == "" {
		status.State = StateDisconnected
	}

	if r.statuses[tenant] == nil {
		r.statuses[tenant] = make(map[string]Status)
	}

	r.statuses[tenant][providerID] = status
	return nil
}

func (r *Registry) Status(
	tenant sentry.TenantID,
	providerID string,
) (Status, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if _, exists := r.providers[providerID]; !exists {
		return Status{}, false
	}

	if tenantStatuses := r.statuses[tenant]; tenantStatuses != nil {
		if status, exists := tenantStatuses[providerID]; exists {
			return status, true
		}
	}

	if status, exists := r.defaults[providerID]; exists {
		return status, true
	}

	return Status{State: StateDisconnected}, true
}
