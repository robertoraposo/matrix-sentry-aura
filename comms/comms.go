// Package comms is Matrix Sentry's agent communication channel: an ordered,
// per-tenant message log built on the SentryLog journal — the coordination
// counterpart to the semantic memory in package memory. Unlike memory it does
// NOT embed or deduplicate: messages are a chronological stream agents poll by
// area. It mirrors memory.Store's journal-wrapping + in-RAM-index pattern so
// reads are a cheap in-RAM filter, while the journal keeps the durable record.
package comms

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"matrixsentry/sentry"
)

// EventMessage is the journal record type for a channel message.
const EventMessage sentry.EventType = 5

// EventCommsClear tombstones an area: on rebuild, messages in that area+tenant
// posted before this record are dropped from the index (the journal keeps them).
const EventCommsClear sentry.EventType = 7

// EventImage is the journal record type for an image-reference message: a comms
// message that carries a blob ref (sha + mime + dims) instead of body text. The
// bytes live in the blob store; only this small ref rides the in-RAM index.
const EventImage sentry.EventType = 8

// EventBlobPin records a pin (On=true) or unpin (On=false) of a blob sha, so a
// blob survives GC independently of whether a live message still references it.
const EventBlobPin sentry.EventType = 9

// PinPayload is the persisted pin/unpin of a blob.
type PinPayload struct {
	BlobID string `json:"blob"`
	On     bool   `json:"on"`
}

// ClearPayload names the area to sweep.
type ClearPayload struct {
	Area string `json:"area"`
}

// MessagePayload is the persisted form of one message.
type MessagePayload struct {
	Area   string `json:"area"`
	From   string `json:"from"`
	Kind   string `json:"kind,omitempty"`
	Text   string `json:"text"`
	Target string `json:"target,omitempty"`
	Ref    uint64 `json:"ref,omitempty"`
	// Image reference (set only for image messages; BlobID != "" marks an image):
	BlobID string `json:"blob,omitempty"`
	Mime   string `json:"mime,omitempty"`
	W      int    `json:"w,omitempty"`
	H      int    `json:"h,omitempty"`
	Size   int    `json:"size,omitempty"`
}

// Message is a read result: the payload plus the journal seq (its id), tenant, ts.
type Message struct {
	Seq    uint64
	Tenant sentry.TenantID
	TS     int64
	Area   string
	From   string
	Kind   string
	Text   string
	Target string
	Ref    uint64
	// Image reference (set only for image messages; BlobID != "" marks an image):
	BlobID string
	Mime   string
	W      int
	H      int
	Size   int
}

// Store is the message log: a journal for durability plus an in-RAM index.
type Store struct {
	journal   *sentry.Store
	mu        sync.Mutex
	entries   []Message
	retainN   int           // keep at most the last N messages in the index (0 = off)
	retainAge time.Duration // drop messages older than this from the index (0 = off)
	pinned    map[string]map[sentry.TenantID]bool // blob sha → set of tenants who pinned it; kept from GC while any tenant holds a pin
	hub       *hub          // in-RAM nudge pub/sub (wake-on-update); set by New before first use
}

func message(seq uint64, tenant sentry.TenantID, ts int64, p MessagePayload) Message {
	return Message{
		Seq: seq, Tenant: tenant, TS: ts,
		Area: p.Area, From: p.From, Kind: p.Kind, Text: p.Text, Target: p.Target, Ref: p.Ref,
		BlobID: p.BlobID, Mime: p.Mime, W: p.W, H: p.H, Size: p.Size,
	}
}

// New wraps a journal, rebuilding the in-RAM message index from EventMessage records.
func New(journal *sentry.Store) (*Store, error) {
	s := &Store{journal: journal}
	etype := EventMessage
	var scanErr error
	err := journal.Scan(sentry.Filter{Type: &etype}, func(r sentry.Record) bool {
		var p MessagePayload
		if err := sentry.UnmarshalPayload(r.Payload, &p); err != nil {
			scanErr = fmt.Errorf("comms: decode record seq %d: %w", r.Seq, err)
			return false
		}
		s.entries = append(s.entries, message(uint64(r.Seq), r.Tenant, r.Tstamp, p))
		return true
	})
	if err != nil {
		return nil, err
	}
	if scanErr != nil {
		return nil, scanErr
	}

	// Pass 1b: image-reference messages share the message index.
	itype := EventImage
	if err := journal.Scan(sentry.Filter{Type: &itype}, func(r sentry.Record) bool {
		var p MessagePayload
		if err := sentry.UnmarshalPayload(r.Payload, &p); err != nil {
			scanErr = fmt.Errorf("comms: decode image seq %d: %w", r.Seq, err)
			return false
		}
		s.entries = append(s.entries, message(uint64(r.Seq), r.Tenant, r.Tstamp, p))
		return true
	}); err != nil {
		return nil, err
	}
	if scanErr != nil {
		return nil, scanErr
	}
	// Messages and images interleave in the journal but are scanned separately;
	// re-sort by seq so Read/Inbox/Recent cursor logic stays correct.
	sort.Slice(s.entries, func(i, j int) bool { return s.entries[i].Seq < s.entries[j].Seq })

	// Pass 2: apply area-clear tombstones. A clear at seq S for (tenant, area)
	// drops that area+tenant's messages with seq < S; later posts survive.
	ctype := EventCommsClear
	if err := journal.Scan(sentry.Filter{Type: &ctype}, func(r sentry.Record) bool {
		var cp ClearPayload
		if err := sentry.UnmarshalPayload(r.Payload, &cp); err != nil {
			scanErr = fmt.Errorf("comms: decode clear seq %d: %w", r.Seq, err)
			return false
		}
		cut := uint64(r.Seq)
		out := s.entries[:0]
		for _, m := range s.entries {
			if m.Tenant == r.Tenant && m.Area == cp.Area && m.Seq < cut {
				continue
			}
			out = append(out, m)
		}
		s.entries = out
		return true
	}); err != nil {
		return nil, err
	}
	if scanErr != nil {
		return nil, scanErr
	}

	// Pass 3: replay blob pins (seq order → final on/off state wins).
	// Pins are per-(tenant, blobID): r.Tenant is the authoritative tenant for each pin record.
	s.pinned = map[string]map[sentry.TenantID]bool{}
	ptype := EventBlobPin
	if err := journal.Scan(sentry.Filter{Type: &ptype}, func(r sentry.Record) bool {
		var pp PinPayload
		if err := sentry.UnmarshalPayload(r.Payload, &pp); err != nil {
			scanErr = fmt.Errorf("comms: decode pin seq %d: %w", r.Seq, err)
			return false
		}
		if pp.On {
			if s.pinned[pp.BlobID] == nil {
				s.pinned[pp.BlobID] = map[sentry.TenantID]bool{}
			}
			s.pinned[pp.BlobID][r.Tenant] = true
		} else {
			delete(s.pinned[pp.BlobID], r.Tenant)
			if len(s.pinned[pp.BlobID]) == 0 {
				delete(s.pinned, pp.BlobID)
			}
		}
		return true
	}); err != nil {
		return nil, err
	}
	if scanErr != nil {
		return nil, scanErr
	}

	s.hub = newHub()
	return s, nil
}

// Post appends a message to area for tenant and returns its journal seq (its id).
// Area, From and Text are required; Kind defaults to "note".
func (s *Store) Post(tenant sentry.TenantID, p MessagePayload) (uint64, error) {
	if p.Area == "" || p.From == "" || p.Text == "" {
		return 0, fmt.Errorf("comms: area, from and text are required")
	}
	if p.Kind == "" {
		p.Kind = "note"
	}
	s.mu.Lock()
	seq, err := s.journal.Append(tenant, EventMessage, p)
	if err != nil {
		s.mu.Unlock()
		return 0, fmt.Errorf("comms: append: %w", err)
	}
	msg := message(uint64(seq), tenant, time.Now().UnixNano(), p)
	s.entries = append(s.entries, msg)
	s.pruneAt(msg.TS)
	s.mu.Unlock()
	s.hub.publish(msg)
	return uint64(seq), nil
}

// PostImage appends an image-reference message to area for tenant and returns
// its journal seq. Area, From, BlobID and Mime are required; Kind is forced to
// "image". The bytes themselves live in the blob store, keyed by BlobID.
func (s *Store) PostImage(tenant sentry.TenantID, p MessagePayload) (uint64, error) {
	if p.Area == "" || p.From == "" || p.BlobID == "" || p.Mime == "" {
		return 0, fmt.Errorf("comms: area, from, blob and mime are required for an image")
	}
	p.Kind = "image"
	s.mu.Lock()
	seq, err := s.journal.Append(tenant, EventImage, p)
	if err != nil {
		s.mu.Unlock()
		return 0, fmt.Errorf("comms: append image: %w", err)
	}
	msg := message(uint64(seq), tenant, time.Now().UnixNano(), p)
	s.entries = append(s.entries, msg)
	s.pruneAt(msg.TS)
	s.mu.Unlock()
	s.hub.publish(msg)
	return uint64(seq), nil
}

// GetBySeq returns tenant's message at seq regardless of area (image tools have
// a seq, not an area). Tenant-scoped: another tenant's seq returns false.
func (s *Store) GetBySeq(tenant sentry.TenantID, seq uint64) (Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.entries {
		if m.Tenant == tenant && m.Seq == seq {
			return m, true
		}
	}
	return Message{}, false
}

// Clear removes a tenant's messages in area from the live index by appending an
// EventCommsClear tombstone (the journal record is retained). Messages posted to
// the area AFTER the clear survive (the clear's own seq is the cutoff). Returns
// how many index entries were dropped.
func (s *Store) Clear(tenant sentry.TenantID, area string) (int, error) {
	if area == "" {
		return 0, fmt.Errorf("comms: area required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, err := s.journal.Append(tenant, EventCommsClear, ClearPayload{Area: area})
	if err != nil {
		return 0, fmt.Errorf("comms: append clear: %w", err)
	}
	cut := uint64(seq)
	out := s.entries[:0]
	dropped := 0
	for _, m := range s.entries {
		if m.Tenant == tenant && m.Area == area && m.Seq < cut {
			dropped++
			continue
		}
		out = append(out, m)
	}
	s.entries = out
	return dropped, nil
}

// Read returns tenant's messages in area with Seq > since, in seq order.
func (s *Store) Read(tenant sentry.TenantID, area string, since uint64) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Message
	for _, m := range s.entries {
		if m.Tenant == tenant && m.Area == area && m.Seq > since {
			out = append(out, m)
		}
	}
	return out
}

// Inbox returns messages directed at target (Target==target) across ALL areas
// for tenant, with Seq > since, in seq order. Lets an agent fetch everything
// addressed to it in one call instead of guessing areas. In-RAM; no journal scan.
func (s *Store) Inbox(tenant sentry.TenantID, target string, since uint64) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Message
	for _, m := range s.entries {
		if m.Tenant == tenant && m.Target == target && m.Seq > since {
			out = append(out, m)
		}
	}
	return out
}

// Recent returns the last `limit` messages for tenant across all areas, in seq
// order (oldest→newest). In-RAM; serves the dashboard without a journal scan.
func (s *Store) Recent(tenant sentry.TenantID, limit int) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Message
	for _, m := range s.entries {
		if m.Tenant == tenant {
			out = append(out, m)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// SetRetention bounds the in-RAM index: keep only messages BOTH within the last n
// (0 = off) AND newer than age (0 = off). The journal is untouched (audit).
// Applied here and after every Post. Global across tenants (RAM bound).
func (s *Store) SetRetention(n int, age time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retainN = n
	s.retainAge = age
	s.pruneAt(time.Now().UnixNano())
}

// pruneAt drops index entries failing either retention knob. Caller holds s.mu.
func (s *Store) pruneAt(now int64) {
	if s.retainN <= 0 && s.retainAge <= 0 {
		return
	}
	start := 0
	if s.retainN > 0 && len(s.entries) > s.retainN {
		start = len(s.entries) - s.retainN
	}
	var cutoff int64
	if s.retainAge > 0 {
		cutoff = now - int64(s.retainAge)
	}
	out := make([]Message, 0, len(s.entries)-start)
	for _, m := range s.entries[start:] {
		if cutoff > 0 && m.TS < cutoff {
			continue
		}
		out = append(out, m)
	}
	s.entries = out
}

// Get returns the message at seq in area for tenant.
func (s *Store) Get(tenant sentry.TenantID, area string, seq uint64) (Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.entries {
		if m.Tenant == tenant && m.Area == area && m.Seq == seq {
			return m, true
		}
	}
	return Message{}, false
}

// Pin records a pin (on=true) or unpin (on=false) for a blob sha for this tenant
// and updates the in-RAM pinned set. Pins are per-(tenant, blobID): a blob is
// kept from GC if ANY tenant holds a pin on it. Unpinning only removes the
// calling tenant's pin; other tenants' pins remain in effect.
func (s *Store) Pin(tenant sentry.TenantID, blobID string, on bool) error {
	if blobID == "" {
		return fmt.Errorf("comms: blob id required to pin")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.journal.Append(tenant, EventBlobPin, PinPayload{BlobID: blobID, On: on}); err != nil {
		return fmt.Errorf("comms: append pin: %w", err)
	}
	if s.pinned == nil {
		s.pinned = map[string]map[sentry.TenantID]bool{}
	}
	if on {
		if s.pinned[blobID] == nil {
			s.pinned[blobID] = map[sentry.TenantID]bool{}
		}
		s.pinned[blobID][tenant] = true
	} else {
		delete(s.pinned[blobID], tenant)
		if len(s.pinned[blobID]) == 0 {
			delete(s.pinned, blobID)
		}
	}
	return nil
}

// LiveBlobIDs returns the GC keep-set: every blob referenced by a live image
// message (across ALL tenants — blobs are shared) plus every blob that has at
// least one tenant's pin still active.
func (s *Store) LiveBlobIDs() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := map[string]bool{}
	for _, m := range s.entries {
		if m.BlobID != "" {
			keep[m.BlobID] = true
		}
	}
	for sha, tenants := range s.pinned {
		if len(tenants) > 0 {
			keep[sha] = true
		}
	}
	return keep
}
