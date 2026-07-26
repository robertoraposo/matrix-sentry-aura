package comms

import (
	"fmt"
	"sort"
	"time"

	"matrixsentry/sentry"
)

// presence.go implements the ephemeral heartbeat slot — the fix for the 55%
// heartbeat-noise problem. Presence is the one deliberately-RAM-only citizen:
// last-status-wins per agent, NEVER journaled (so 161 heartbeats/day cost 0
// journal records) and empty after a restart until agents repopulate it. There
// is no hub.publish and no journal.Append here, so the lock-order invariant is
// preserved trivially.

// Heartbeat overwrites tenant's presence slot for agent with the latest status
// (last-status-wins). agent and status are required; area is optional. It never
// appends to the journal and never publishes a nudge. The slot goes stale after
// presenceStale with no update and is dropped by the sweeper.
func (s *Store) Heartbeat(tenant sentry.TenantID, agent, area, status string, now time.Time) error {
	if agent == "" || status == "" {
		return fmt.Errorf("comms: agent and status are required to heartbeat")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.presence[tenant] == nil {
		s.presence[tenant] = map[string]Presence{}
	}
	s.presence[tenant][agent] = Presence{Agent: agent, Area: area, Status: status, TS: now.UnixNano()}
	return nil
}

// PresenceList returns tenant's live presence slots by value, sorted by agent
// label for a stable render. Goes through s.mu so it never races the sweeper's
// stale-slot pruning. Returns nil when the tenant has no live slots.
func (s *Store) PresenceList(tenant sentry.TenantID) []Presence {
	s.mu.Lock()
	defer s.mu.Unlock()
	slots := s.presence[tenant]
	if len(slots) == 0 {
		return nil
	}
	out := make([]Presence, 0, len(slots))
	for _, p := range slots {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}

// SetPresenceStale sets how long a presence slot survives without a fresh
// heartbeat before the sweeper drops it (wired from SENTRY_COMMS_PRESENCE_STALE_SEC
// at startup). A non-positive d leaves the current value; a zero presenceStale
// disables staleness entirely (pruneStalePresenceLocked never evicts).
func (s *Store) SetPresenceStale(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.presenceStale = d
}

// pruneStalePresenceLocked deletes every presence slot last updated more than
// presenceStale before now (unix nanos). Caller holds s.mu (invoked by the
// sweeper each tick). A non-positive presenceStale disables the knob (no slot is
// ever considered stale) so a misconfigured 0 does not evict every heartbeat.
func (s *Store) pruneStalePresenceLocked(now int64) {
	if s.presenceStale <= 0 {
		return
	}
	max := int64(s.presenceStale)
	for tenant, slots := range s.presence {
		for agent, p := range slots {
			if now-p.TS > max {
				delete(slots, agent)
			}
		}
		if len(slots) == 0 {
			delete(s.presence, tenant)
		}
	}
}
