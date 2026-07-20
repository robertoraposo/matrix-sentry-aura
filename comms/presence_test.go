package comms

import (
	"testing"
	"time"
)

// TestHeartbeatLastWins: repeated heartbeats from one agent collapse to a single
// presence slot carrying the latest status (last-status-wins per agent).
func TestHeartbeatLastWins(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Unix(1700000000, 0)
	for i, status := range []string{"boot", "warming", "STANDBY", "busy", "idle"} {
		if err := s.Heartbeat(1, "x", "ops", status, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
	}
	got := s.PresenceList(1)
	if len(got) != 1 {
		t.Fatalf("5 heartbeats from one agent must collapse to 1 slot, got %d", len(got))
	}
	if got[0].Agent != "x" || got[0].Status != "idle" || got[0].Area != "ops" {
		t.Fatalf("last-wins slot = %+v, want agent=x status=idle area=ops", got[0])
	}
}

// TestHeartbeatRequiresAgentAndStatus: an empty agent or status is rejected.
func TestHeartbeatRequiresAgentAndStatus(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Unix(1700000000, 0)
	if err := s.Heartbeat(1, "", "ops", "up", now); err == nil {
		t.Fatal("empty agent must error")
	}
	if err := s.Heartbeat(1, "x", "ops", "", now); err == nil {
		t.Fatal("empty status must error")
	}
}

// TestHeartbeatNotJournaled is the core presence contract: heartbeats are RAM-only
// and add ZERO journal records (the whole point — 161 heartbeats/day cost 0 disk).
func TestHeartbeatNotJournaled(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Unix(1700000000, 0)
	before := s.journal.ReadNextSeq()
	for i := 0; i < 10; i++ {
		if err := s.Heartbeat(1, "x", "ops", "STANDBY tick", now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
	}
	after := s.journal.ReadNextSeq()
	if after != before {
		t.Fatalf("heartbeats must add zero journal records: nextSeq %d -> %d", before, after)
	}
}

// TestPresenceTenantScoped: one tenant's presence slots are invisible to another.
func TestPresenceTenantScoped(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Unix(1700000000, 0)
	if err := s.Heartbeat(1, "x", "ops", "up", now); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if got := s.PresenceList(2); len(got) != 0 {
		t.Fatalf("tenant 2 saw tenant 1's presence: %+v", got)
	}
	if got := s.PresenceList(1); len(got) != 1 {
		t.Fatalf("tenant 1 presence = %+v, want its 1 slot", got)
	}
}

// TestPresenceListSorted: PresenceList is sorted by agent label for stable render.
func TestPresenceListSorted(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Unix(1700000000, 0)
	for _, a := range []string{"charlie", "alice", "bob"} {
		if err := s.Heartbeat(1, a, "ops", "up", now); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
	}
	got := s.PresenceList(1)
	if len(got) != 3 || got[0].Agent != "alice" || got[1].Agent != "bob" || got[2].Agent != "charlie" {
		t.Fatalf("PresenceList must be agent-sorted: %+v", got)
	}
}

// TestPresenceStaleDropped: a slot older than presenceStale is removed by
// pruneStalePresenceLocked; a fresh slot survives.
func TestPresenceStaleDropped(t *testing.T) {
	s, _ := newTestStore(t)
	s.presenceStale = 15 * time.Minute
	now := time.Unix(1700000000, 0)
	if err := s.Heartbeat(1, "stale", "ops", "up", now); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	t0 := now.UnixNano()

	// Not yet stale: exactly at the boundary the slot survives.
	s.pruneStalePresenceLocked(t0 + int64(s.presenceStale))
	if got := s.PresenceList(1); len(got) != 1 {
		t.Fatalf("slot must survive at the staleness boundary: %+v", got)
	}

	// One ns past the boundary: the slot is dropped.
	s.pruneStalePresenceLocked(t0 + int64(s.presenceStale) + 1)
	if got := s.PresenceList(1); len(got) != 0 {
		t.Fatalf("stale slot must be dropped: %+v", got)
	}
}
