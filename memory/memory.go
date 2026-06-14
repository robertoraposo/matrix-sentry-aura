// Package memory is the semantic-memory layer of Matrix Sentry: the thing that
// stops a coding agent from having amnesia. Remember persists a piece of text
// with its embedding to the durable journal and indexes it; Recall embeds a
// query and returns the nearest stored memories. It is the bridge between the
// SentryLog journal (sentry/) and the vector engine.
//
// Search is EXACT (brute-force L2 over the stored vectors). At an agent's real
// memory volume (tens to low thousands of items) exact search is sub-millisecond
// and recall-perfect — the honest 100%-correct baseline. The validated
// ivf.Recommended + SearchRerank path is the scale optimization that drops in
// behind this same API when a corpus grows large enough to need it.
package memory

import (
	"fmt"
	"sort"
	"sync"

	"matrixsentry/sentry"
)

// EventMemory is the journal record type for a stored memory (text + vector).
const EventMemory sentry.EventType = 3

// MemoryPayload is the persisted form of one memory. The vector is stored so a
// reopened store rebuilds its index without re-embedding (embedding is the only
// external, expensive, possibly-nondeterministic step).
type MemoryPayload struct {
	ID     uint64    `json:"id"`
	Text   string    `json:"text"`
	Vector []float32 `json:"vec"`
	Tags   []string  `json:"tags,omitempty"`
	Source string    `json:"src,omitempty"`
	// Supersedes, when non-zero, is the id of an earlier memory this record
	// replaces. On rebuild the superseded id is dropped from the in-RAM index;
	// the original record remains on disk (append-only journal = full history).
	Supersedes uint64 `json:"sup,omitempty"`
}

// Memory is a recall result: the stored item plus its distance to the query
// (Score; 0 = identical, smaller = closer).
type Memory struct {
	ID     uint64
	Text   string
	Tags   []string
	Source string
	Score  float32
}

// Embedder turns text into vectors. Implementations must be consistent: the
// same text embeds to the same dimension every call.
type Embedder interface {
	Embed(texts []string) ([][]float32, error)
	Dim() int
}

type entry struct {
	tenant sentry.TenantID
	mem    MemoryPayload
}

// Store is the semantic memory: a journal for durability plus an in-RAM vector
// table for search. Safe for concurrent use.
type Store struct {
	journal *sentry.Store
	embed   Embedder
	mu      sync.Mutex
	entries []entry
	nextID  uint64
	// DedupThreshold is the squared-L2 novelty radius. If a new memory's
	// nearest existing neighbor (same tenant) is strictly closer than this
	// (distance < threshold; the boundary point is treated as novel), Remember
	// treats it as a duplicate and does not persist it. 0 disables dedup.
	DedupThreshold float32
}

// New wraps a journal with semantic memory, rebuilding the in-RAM index from
// any EventMemory records already on disk (no re-embedding). It errors if a
// persisted vector's length disagrees with the embedder dimension, which would
// make recall distances meaningless.
func New(journal *sentry.Store, embed Embedder) (*Store, error) {
	s := &Store{journal: journal, embed: embed, nextID: 1}
	etype := EventMemory
	var scanErr error
	err := journal.Scan(sentry.Filter{Type: &etype}, func(r sentry.Record) bool {
		var p MemoryPayload
		if err := sentry.UnmarshalPayload(r.Payload, &p); err != nil {
			scanErr = fmt.Errorf("memory: decode record seq %d: %w", r.Seq, err)
			return false
		}
		if len(p.Vector) != embed.Dim() {
			scanErr = fmt.Errorf("memory: persisted vector dim %d != embedder dim %d (id %d)", len(p.Vector), embed.Dim(), p.ID)
			return false
		}
		s.entries = append(s.entries, entry{tenant: r.Tenant, mem: p})
		if p.ID >= s.nextID {
			s.nextID = p.ID + 1
		}
		// A superseding record always has a higher id than the record it
		// supersedes (ids are monotonic, assigned after the superseded one), so
		// the target is already in s.entries by the time we replay this record.
		if p.Supersedes != 0 {
			s.dropEntry(r.Tenant, p.Supersedes)
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	if scanErr != nil {
		return nil, scanErr
	}
	return s, nil
}

// dropEntry removes the in-RAM entry for (tenant, id) if present. The journal
// record is untouched — only the live index (current truth) drops the id.
// Caller holds s.mu (or, in New, the store is not yet shared).
func (s *Store) dropEntry(tenant sentry.TenantID, id uint64) {
	for i := range s.entries {
		if s.entries[i].tenant == tenant && s.entries[i].mem.ID == id {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			return
		}
	}
}

// RememberOpts carries the optional inputs to Remember. Zero value = a plain
// store subject to the dedup gate.
type RememberOpts struct {
	Tags       []string
	Src        string
	Supersedes uint64 // replace this existing same-tenant memory (bypasses dedup)
	Force      bool   // store even if a near-duplicate exists (bypasses dedup)
}

// Remember embeds text and persists it as an EventMemory, returning its id.
//
//   - opts.Supersedes names an existing same-tenant memory → the dedup gate is
//     bypassed, the new record carries a Supersedes pointer, the superseded id is
//     dropped from the index, and superseded is set to it. (Takes precedence over
//     Force.)
//   - else opts.Force → store without the dedup gate (persist even within tau).
//   - else → the dedup gate applies: if dedup is enabled (DedupThreshold > 0) and
//     the nearest same-tenant memory is within tau, the text is NOT persisted and
//     the existing id is returned with deduped=true.
func (s *Store) Remember(tenant sentry.TenantID, text string, opts RememberOpts) (id uint64, deduped bool, superseded uint64, err error) {
	vecs, err := s.embed.Embed([]string{text})
	if err != nil {
		return 0, false, 0, fmt.Errorf("memory: embed: %w", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != s.embed.Dim() {
		return 0, false, 0, fmt.Errorf("memory: embedder returned %d vectors / bad dim", len(vecs))
	}
	v := vecs[0]

	s.mu.Lock()
	defer s.mu.Unlock()

	if opts.Supersedes != 0 && s.hasEntry(tenant, opts.Supersedes) {
		p := MemoryPayload{ID: s.nextID, Text: text, Vector: v, Tags: opts.Tags, Source: opts.Src, Supersedes: opts.Supersedes}
		if _, err := s.journal.Append(tenant, EventMemory, p); err != nil {
			return 0, false, 0, fmt.Errorf("memory: append: %w", err)
		}
		s.dropEntry(tenant, opts.Supersedes)
		s.entries = append(s.entries, entry{tenant: tenant, mem: p})
		s.nextID++
		return p.ID, false, opts.Supersedes, nil
	}

	if !opts.Force && s.DedupThreshold > 0 {
		var bestID uint64
		var bestDist float32
		found := false
		for _, e := range s.entries {
			if e.tenant != tenant {
				continue
			}
			d := sqL2(v, e.mem.Vector)
			if !found || d < bestDist {
				found, bestDist, bestID = true, d, e.mem.ID
			}
		}
		if found && bestDist < s.DedupThreshold {
			return bestID, true, 0, nil
		}
	}

	p := MemoryPayload{ID: s.nextID, Text: text, Vector: v, Tags: opts.Tags, Source: opts.Src}
	if _, err := s.journal.Append(tenant, EventMemory, p); err != nil {
		return 0, false, 0, fmt.Errorf("memory: append: %w", err)
	}
	s.entries = append(s.entries, entry{tenant: tenant, mem: p})
	s.nextID++
	return p.ID, false, 0, nil
}

// hasEntry reports whether (tenant, id) is live in the index. Caller holds s.mu.
func (s *Store) hasEntry(tenant sentry.TenantID, id uint64) bool {
	for _, e := range s.entries {
		if e.tenant == tenant && e.mem.ID == id {
			return true
		}
	}
	return false
}

// Recall embeds query and returns the k nearest memories for tenant, ascending
// by L2 distance (closest first). An empty store yields an empty slice.
func (s *Store) Recall(tenant sentry.TenantID, query string, k int) ([]Memory, error) {
	if k <= 0 {
		return nil, nil
	}
	vecs, err := s.embed.Embed([]string{query})
	if err != nil {
		return nil, fmt.Errorf("memory: embed query: %w", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != s.embed.Dim() {
		return nil, fmt.Errorf("memory: embedder returned %d vectors / bad dim", len(vecs))
	}
	q := vecs[0]

	s.mu.Lock()
	scored := make([]Memory, 0, len(s.entries))
	for _, e := range s.entries {
		if e.tenant != tenant {
			continue
		}
		scored = append(scored, Memory{
			ID: e.mem.ID, Text: e.mem.Text, Tags: e.mem.Tags,
			Source: e.mem.Source, Score: sqL2(q, e.mem.Vector),
		})
	}
	s.mu.Unlock()

	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score < scored[j].Score
		}
		return scored[i].ID < scored[j].ID
	})
	if len(scored) > k {
		scored = scored[:k]
	}
	return scored, nil
}

// Count returns how many memories tenant has stored.
func (s *Store) Count(tenant sentry.TenantID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.entries {
		if e.tenant == tenant {
			n++
		}
	}
	return n
}

func sqL2(a, b []float32) float32 {
	var d float32
	for i := range a {
		diff := a[i] - b[i]
		d += diff * diff
	}
	return d
}
