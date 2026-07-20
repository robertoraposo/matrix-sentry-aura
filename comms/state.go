package comms

import (
	"fmt"
	"time"

	"matrixsentry/sentry"
)

// state.go implements the lifecycle-v2 task state machine: an atomic claim/lease
// compare-and-swap, holder/owner resolve, and the event-sourced rebuild replay.
//
// Concurrency contract (do not regress):
//   - The ENTIRE claim read-modify-write — claimability check, journal append,
//     and map mutation — stays inside ONE s.mu critical section. A single-process
//     mutex makes the CAS trivially correct: only one goroutine is ever inside.
//   - Order is journal-append-then-commit (like Post, comms.go): appendStateLocked
//     writes the durable event FIRST; the in-RAM tasks map is mutated ONLY on a
//     successful append. An append error aborts with no mutation, so RAM never
//     runs ahead of the journal (a restart can't drop a "claim" that was never
//     durably recorded).
//   - Claim/Resolve add no hub nudge, so nothing is published while holding s.mu;
//     the lock-order invariant (publish after unlock) is preserved trivially.
//   - Claim/Resolve/TaskOf return TaskState BY VALUE (a copy under the lock);
//     the *TaskState in the map is never handed out, so callers cannot race the
//     sweeper's in-place mutation.

// Claim performs the atomic task-claim compare-and-swap. It returns the resulting
// (or current, when denied) TaskState by value, ok=true iff the claim succeeded,
// and an error only for a bad request (empty `by`) or a non-task seq.
//
// Claimable iff the task is pending, OR it is claimed/overdue AND its lease has
// expired (now > LeaseUntil) OR the caller is already its holder (renewal, which
// doubles as a task-level "still working" heartbeat and extends the lease). A
// terminal done/cancel task is claimable by no one — the parentheses below are
// load-bearing (Go's && binds tighter than ||): dropping them would let any task
// whose stored Holder equals `by`, including a resolved one, be re-claimed.
func (s *Store) Claim(tenant sentry.TenantID, seq uint64, by string, now time.Time) (TaskState, bool, error) {
	if by == "" {
		return TaskState{}, false, fmt.Errorf("comms: 'by' required to claim")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.tasks[tenant][seq] // safe on a nil inner map: returns (nil, false)
	if !ok {
		return TaskState{}, false, fmt.Errorf("comms: #%d is not a task for this tenant", seq)
	}
	nn := now.UnixNano()
	claimable := ts.State == "pending" ||
		((ts.State == "claimed" || ts.State == "overdue") && (nn > ts.LeaseUntil || ts.Holder == by))
	if !claimable {
		return *ts, false, nil
	}
	lease := nn + int64(s.leaseTTL)
	// Journal FIRST; mutate the map only on success (append-then-commit).
	if err := s.appendStateLocked(tenant, StatePayload{Seq: seq, State: "claimed", By: by, LeaseUntil: lease}); err != nil {
		return *ts, false, err
	}
	ts.State, ts.Holder, ts.LeaseUntil = "claimed", by, lease
	return *ts, true, nil
}

// Resolve closes a task terminally as state ∈ {done, cancel} (empty defaults to
// "done"). Allowed iff `by` is the current holder or the configured owner label
// (owner override for stuck tasks). Journals the state change first, then mutates
// the map. A resolved task is terminal — no further Claim succeeds.
func (s *Store) Resolve(tenant sentry.TenantID, seq uint64, by, state string, now time.Time) error {
	if by == "" {
		return fmt.Errorf("comms: 'by' required to resolve")
	}
	if state == "" {
		state = "done"
	}
	if state != "done" && state != "cancel" {
		return fmt.Errorf("comms: resolve state must be done or cancel, got %q", state)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.tasks[tenant][seq]
	if !ok {
		return fmt.Errorf("comms: #%d is not a task for this tenant", seq)
	}
	isOwner := s.ownerLabel != "" && by == s.ownerLabel
	if by != ts.Holder && !isOwner {
		return fmt.Errorf("comms: #%d can only be resolved by its holder (%q) or the owner", seq, ts.Holder)
	}
	if err := s.appendStateLocked(tenant, StatePayload{Seq: seq, State: state, By: by}); err != nil {
		return err
	}
	ts.State, ts.Holder, ts.LeaseUntil = state, by, 0
	return nil
}

// TaskOf returns tenant's live task state for seq by value, ok=false if seq is not
// a task for this tenant. Goes through s.mu so it never races the sweeper.
func (s *Store) TaskOf(tenant sentry.TenantID, seq uint64) (TaskState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.tasks[tenant][seq]
	if !ok {
		return TaskState{}, false
	}
	return *ts, true
}

// SetOwnerLabel sets the agent label allowed to override Resolve on any task
// (default "" = only the holder may resolve).
func (s *Store) SetOwnerLabel(label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ownerLabel = label
}

// appendStateLocked journals one EventMessageState change. The caller MUST hold
// s.mu and MUST mutate the in-RAM tasks map only after this returns nil, so RAM
// never runs ahead of the durable journal. (comms.mu → journal.mu is the same
// lock order Post already establishes; no inversion.)
func (s *Store) appendStateLocked(tenant sentry.TenantID, sp StatePayload) error {
	if _, err := s.journal.Append(tenant, EventMessageState, sp); err != nil {
		return fmt.Errorf("comms: append state: %w", err)
	}
	return nil
}

// seedTaskLocked initializes tasks[tenant][seq] = pending for a kind=task message
// (no state event — absence of a task-state entry means pending). Caller holds
// s.mu. Lazily inits the inner map so the first task for a tenant does not panic
// on a nil-map write, and never clobbers an already-present entry (so replay's
// last-wins state survives a later seed of the same seq).
func (s *Store) seedTaskLocked(tenant sentry.TenantID, seq uint64, deadline int64) {
	if s.tasks[tenant] == nil {
		s.tasks[tenant] = map[uint64]*TaskState{}
	}
	if _, ok := s.tasks[tenant][seq]; ok {
		return
	}
	s.tasks[tenant][seq] = &TaskState{State: "pending", Deadline: deadline}
}

// replayStateLocked rebuilds durable task state by scanning EventMessageState in
// seq order, last-wins per (tenant, seq). Caller holds s.mu (or, as in New, runs
// single-threaded before the Store is shared). Runs AFTER every kind=task message
// has been seeded pending, so Deadline is already known and preserved — a state
// event only overwrites State/Holder/LeaseUntil, never Deadline.
func (s *Store) replayStateLocked() error {
	etype := EventMessageState
	var scanErr error
	if err := s.journal.Scan(sentry.Filter{Type: &etype}, func(r sentry.Record) bool {
		var sp StatePayload
		if err := sentry.UnmarshalPayload(r.Payload, &sp); err != nil {
			scanErr = fmt.Errorf("comms: decode state seq %d: %w", r.Seq, err)
			return false
		}
		if s.tasks[r.Tenant] == nil {
			s.tasks[r.Tenant] = map[uint64]*TaskState{}
		}
		ts := s.tasks[r.Tenant][sp.Seq]
		if ts == nil {
			ts = &TaskState{}
			s.tasks[r.Tenant][sp.Seq] = ts
		}
		ts.State, ts.Holder, ts.LeaseUntil = sp.State, sp.By, sp.LeaseUntil
		return true
	}); err != nil {
		return err
	}
	return scanErr
}
