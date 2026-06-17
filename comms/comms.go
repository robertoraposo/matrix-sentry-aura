// Package comms is Matrix Sentry's agent communication channel: an ordered,
// per-tenant message log built on the SentryLog journal — the coordination
// counterpart to the semantic memory in package memory. Unlike memory it does
// NOT embed or deduplicate: messages are a chronological stream agents poll by
// area. It mirrors memory.Store's journal-wrapping + in-RAM-index pattern so
// reads are a cheap in-RAM filter, while the journal keeps the durable record.
package comms

import (
	"fmt"
	"sync"
	"time"

	"matrixsentry/sentry"
)

// EventMessage is the journal record type for a channel message.
const EventMessage sentry.EventType = 5

// MessagePayload is the persisted form of one message.
type MessagePayload struct {
	Area   string `json:"area"`
	From   string `json:"from"`
	Kind   string `json:"kind,omitempty"`
	Text   string `json:"text"`
	Target string `json:"target,omitempty"`
	Ref    uint64 `json:"ref,omitempty"`
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
}

// Store is the message log: a journal for durability plus an in-RAM index.
type Store struct {
	journal *sentry.Store
	mu      sync.Mutex
	entries []Message
}

func message(seq uint64, tenant sentry.TenantID, ts int64, p MessagePayload) Message {
	return Message{
		Seq: seq, Tenant: tenant, TS: ts,
		Area: p.Area, From: p.From, Kind: p.Kind, Text: p.Text, Target: p.Target, Ref: p.Ref,
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
	defer s.mu.Unlock()
	seq, err := s.journal.Append(tenant, EventMessage, p)
	if err != nil {
		return 0, fmt.Errorf("comms: append: %w", err)
	}
	s.entries = append(s.entries, message(uint64(seq), tenant, time.Now().UnixNano(), p))
	return uint64(seq), nil
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
