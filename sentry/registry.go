package sentry

import "sync"

// Registry maps file paths to stable, sequential item ids per tenant. The first
// time a path is seen it is assigned the next id and persisted as an
// EventPathMap record, so the journal itself is the single source of truth for
// the dictionary. Safe for concurrent use.
type Registry struct {
	mu     sync.Mutex
	store  *Store
	paths  map[TenantID]map[string]uint64
	nextID map[TenantID]uint64 // highest id assigned per tenant
}

// NewRegistry builds a Registry and rebuilds the path→id map by scanning the
// journal's EventPathMap records, so ids stay stable across restarts.
func NewRegistry(s *Store) (*Registry, error) {
	r := &Registry{
		store:  s,
		paths:  make(map[TenantID]map[string]uint64),
		nextID: make(map[TenantID]uint64),
	}
	etype := EventPathMap
	err := s.Scan(Filter{Type: &etype}, func(rec Record) bool {
		var p PathMapPayload
		if UnmarshalPayload(rec.Payload, &p) != nil {
			return true // skip undecodable dictionary entries
		}
		r.put(rec.Tenant, p.Path, p.ID)
		return true
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Resolve returns the stable id for path under tenant, assigning and persisting
// a new sequential id (via an EventPathMap record) the first time it is seen.
func (r *Registry) Resolve(tenant TenantID, path string) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m := r.paths[tenant]; m != nil {
		if id, ok := m[path]; ok {
			return id, nil
		}
	}
	id := r.nextID[tenant] + 1
	if _, err := r.store.Append(tenant, EventPathMap, PathMapPayload{ID: id, Path: path}); err != nil {
		return 0, err
	}
	r.put(tenant, path, id)
	return id, nil
}

// Record resolves path→id and appends an access event tagged with the source
// tool. It returns the item id and the access record's sequence number.
func (r *Registry) Record(tenant TenantID, path, source string) (uint64, Seq, error) {
	id, err := r.Resolve(tenant, path)
	if err != nil {
		return 0, 0, err
	}
	seq, err := r.store.Append(tenant, EventAccess, AccessPayload{ItemID: id, Source: source})
	if err != nil {
		return id, 0, err
	}
	return id, seq, nil
}

func (r *Registry) put(tenant TenantID, path string, id uint64) {
	if r.paths[tenant] == nil {
		r.paths[tenant] = make(map[string]uint64)
	}
	r.paths[tenant][path] = id
	if id > r.nextID[tenant] {
		r.nextID[tenant] = id
	}
}
