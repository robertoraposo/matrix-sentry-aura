package comms

import (
	"context"
	"time"

	"matrixsentry/sentry"
)

// sweep.go implements the eager sweeper — the piece that makes leases, deadlines,
// TTL, and presence staleness real even in a QUIET channel (v1 retention only ran
// on Post, so a silent channel never expired anything).
//
// Concurrency contract (do not regress — same lock-order invariant as every
// mutator, commit c87a055):
//   - The entire read-modify-write of s.tasks/s.entries/s.presence — including the
//     appendStateLocked journal writes — happens inside ONE s.mu critical section.
//   - hub.publish MUST run AFTER s.mu is released: sweepOnce COLLECTS nudges under
//     the lock and RETURNS them; the Start goroutine publishes them post-unlock.
//     sweepOnce itself never calls hub.publish, so a caller/test can drive it
//     deterministically with an injected `now` and no goroutine.

// msgKey identifies a message by (tenant, seq) for the sweep's nudge lookup.
type msgKey struct {
	tenant sentry.TenantID
	seq    uint64
}

// sweepOnce runs one eager expiry pass at now and returns the nudges to publish
// (task-freed / overdue), collected under s.mu but published by the caller AFTER
// the lock is released. It: (1) flips a claimed task whose lease has passed back to
// pending (holder cleared); (2) flips a not-terminal task past its deadline to
// overdue, PRESERVING holder+lease so a still-live lease is not stealable; (3)
// drops TTL-expired messages from the index (journal record retained); (4) drops
// stale presence slots; (5) applies per-tenant retention. Deterministic in `now`.
func (s *Store) sweepOnce(now time.Time) []Message {
	nn := now.UnixNano()
	var nudges []Message

	s.mu.Lock()
	// Snapshot (tenant,seq)→Message so a flipped task can nudge its area/target,
	// built before the TTL drop so a task whose own message just expired still
	// resolves for one last nudge.
	idx := make(map[msgKey]Message, len(s.entries))
	for _, m := range s.entries {
		idx[msgKey{m.Tenant, m.Seq}] = m
	}

	// Task lifecycle: lease expiry → pending, then deadline → overdue. Values are
	// mutated in place (no keys added/removed), so iterating the map is safe.
	for tenant, byseq := range s.tasks {
		for seq, ts := range byseq {
			// 1. Lease expiry: a claimed task whose lease has passed frees up.
			if ts.State == "claimed" && nn > ts.LeaseUntil {
				if err := s.appendStateLocked(tenant, StatePayload{Seq: seq, State: "pending"}); err != nil {
					continue // journal error: leave state untouched, retry next tick
				}
				ts.State, ts.Holder, ts.LeaseUntil = "pending", "", 0
				if m, ok := idx[msgKey{tenant, seq}]; ok {
					nudges = append(nudges, m)
				}
			}
			// 2. Deadline: a not-terminal, not-already-overdue task past its
			// deadline is flagged overdue. Holder/LeaseUntil are PRESERVED (carried
			// on the event too) so a still-live lease keeps the task un-stealable;
			// claimability is gated on now>LeaseUntil in Claim, not on the flag.
			if ts.State != "done" && ts.State != "cancel" && ts.State != "overdue" &&
				ts.Deadline > 0 && nn > ts.Deadline {
				if err := s.appendStateLocked(tenant, StatePayload{Seq: seq, State: "overdue", By: ts.Holder, LeaseUntil: ts.LeaseUntil}); err != nil {
					continue
				}
				ts.State = "overdue"
				if m, ok := idx[msgKey{tenant, seq}]; ok {
					nudges = append(nudges, m)
				}
			}
		}
	}

	// TTL: drop expired ephemeral messages from the index (journal record kept,
	// exactly like retention — New already skips already-expired entries on load).
	out := s.entries[:0]
	for _, m := range s.entries {
		if s.isExpired(m, nn) {
			continue
		}
		out = append(out, m)
	}
	s.entries = out

	// Presence staleness + per-tenant retention.
	s.pruneStalePresenceLocked(nn)
	s.pruneAt(nn)
	s.mu.Unlock()

	return nudges
}

// Start runs the sweeper on a time.Ticker until ctx is cancelled. Each tick it
// calls sweepOnce and publishes the returned nudges AFTER s.mu has been released
// (sweepOnce already unlocked), preserving the hub.publish-outside-lock invariant.
// A non-positive interval defaults to 30s.
func (s *Store) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				for _, n := range s.sweepOnce(time.Now()) {
					s.hub.publish(n)
				}
			}
		}
	}()
}
