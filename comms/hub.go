package comms

import (
	"sync"

	"matrixsentry/sentry"
)

// Nudge is an ephemeral wake signal: "activity matching your filter, up to Seq".
// It is NOT journaled — it is derived from a Post/PostImage and fanned out in RAM.
type Nudge struct {
	Seq    uint64 `json:"seq"`
	Area   string `json:"area,omitempty"`
	Target string `json:"target,omitempty"`
	Kind   string `json:"kind,omitempty"`
}

// Filter is a subscriber's interest: same tenant AND (Target match OR Area match).
type Filter struct {
	Tenant sentry.TenantID
	Target string
	Areas  []string
}

// matches reports whether message m should wake this filter's subscriber.
func (f Filter) matches(m Message) bool {
	if m.Tenant != f.Tenant {
		return false
	}
	if f.Target != "" && m.Target == f.Target {
		return true
	}
	for _, a := range f.Areas {
		if m.Area == a {
			return true
		}
	}
	return false
}

type subscription struct {
	filter Filter
	ch     chan Nudge
}

// hub is an in-RAM pub/sub of comms nudges. Its mutex is independent of Store.mu;
// the only lock order is Store.mu → hub.mu (publish is called from Post under
// Store.mu; Subscribe/cancel take only hub.mu), so there is no inversion.
type hub struct {
	mu   sync.Mutex
	next int
	subs map[int]subscription
}

func newHub() *hub { return &hub{subs: map[int]subscription{}} }

// subscribe registers a filter and returns a receive channel (buffered 1) plus a
// cancel func that unregisters and closes the channel.
func (h *hub) subscribe(f Filter) (<-chan Nudge, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.next
	h.next++
	ch := make(chan Nudge, 1)
	h.subs[id] = subscription{filter: f, ch: ch}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if s, ok := h.subs[id]; ok {
			delete(h.subs, id)
			// Drain buffered nudges before closing so a receive after cancel
			// immediately returns (open=false) rather than replaying stale nudges.
			for len(s.ch) > 0 {
				<-s.ch
			}
			close(s.ch)
		}
	}
}

// publish fans m out to matching subscribers with a NON-BLOCKING send. A full
// buffer means a nudge is already pending; coalescing is safe because a nudge
// only says "there is something ≥ Seq" and the agent reads everything since its
// cursor. Never blocks the caller (Post).
func (h *hub) publish(m Message) {
	n := Nudge{Seq: m.Seq, Area: m.Area, Target: m.Target, Kind: m.Kind}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.subs {
		if !s.filter.matches(m) {
			continue
		}
		select {
		case s.ch <- n:
		default: // buffer full → pending nudge already covers this; drop.
		}
	}
}

// Subscribe registers interest (by target and/or areas, scoped to tenant) and
// returns a nudge channel + cancel func. Wake-on-update for agents.
func (s *Store) Subscribe(f Filter) (<-chan Nudge, func()) {
	return s.hub.subscribe(f)
}

// MatchingSince returns the highest seq of a live message matching f with
// Seq > since, or 0 if none — used for the SSE catch-up nudge on (re)connect.
func (s *Store) MatchingSince(f Filter, since uint64) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var max uint64
	for _, m := range s.entries {
		if m.Seq > since && f.matches(m) && m.Seq > max {
			max = m.Seq
		}
	}
	return max
}
