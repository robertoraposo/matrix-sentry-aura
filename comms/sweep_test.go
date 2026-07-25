package comms

import (
	"context"
	"testing"
	"time"
)

// TestSweepExpiresLease: a claimed task whose lease has passed is flipped back to
// pending (holder cleared) by an eager sweep — even in a quiet channel where no
// Post/Claim runs. This is the lever that makes leases real without wall-clock.
func TestSweepExpiresLease(t *testing.T) {
	s, _ := newTestStore(t)
	s.leaseTTL = 15 * time.Minute
	seq, _ := s.Post(1, MessagePayload{Area: "a", From: "boss", Text: "t", Kind: "task"})

	t0 := time.Unix(1700000000, 0)
	if _, ok, _ := s.Claim(1, seq, "A", t0); !ok {
		t.Fatal("claim must succeed")
	}
	s.sweepOnce(t0.Add(s.leaseTTL + 1))

	ts, ok := s.TaskOf(1, seq)
	if !ok || ts.State != "pending" || ts.Holder != "" || ts.LeaseUntil != 0 {
		t.Fatalf("expired lease must flip to pending w/ cleared holder: %+v ok=%v", ts, ok)
	}
}

// TestSweepMarksOverdue: a task past its deadline flips to overdue; a task with a
// future deadline is untouched; a resolved (done) task is NEVER marked overdue.
func TestSweepMarksOverdue(t *testing.T) {
	s, _ := newTestStore(t)
	s.leaseTTL = 15 * time.Minute
	t0 := time.Unix(1700000000, 0)

	// Past deadline → overdue.
	pastSeq, _ := s.Post(1, MessagePayload{Area: "a", From: "boss", Text: "late", Kind: "task", Deadline: t0.UnixNano()})
	// Future deadline → untouched.
	futSeq, _ := s.Post(1, MessagePayload{Area: "a", From: "boss", Text: "later", Kind: "task", Deadline: t0.Add(time.Hour).UnixNano()})
	// Done before deadline → never marked.
	doneSeq, _ := s.Post(1, MessagePayload{Area: "a", From: "boss", Text: "finished", Kind: "task", Deadline: t0.UnixNano()})
	if _, ok, _ := s.Claim(1, doneSeq, "w", t0); !ok {
		t.Fatal("claim done-task must succeed")
	}
	if err := s.Resolve(1, doneSeq, "w", "done", t0); err != nil {
		t.Fatal(err)
	}

	s.sweepOnce(t0.Add(time.Nanosecond))

	if ts, _ := s.TaskOf(1, pastSeq); ts.State != "overdue" {
		t.Fatalf("past-deadline task state = %q, want overdue", ts.State)
	}
	if ts, _ := s.TaskOf(1, futSeq); ts.State != "pending" {
		t.Fatalf("future-deadline task state = %q, want pending (untouched)", ts.State)
	}
	if ts, _ := s.TaskOf(1, doneSeq); ts.State != "done" {
		t.Fatalf("done task must never be marked overdue, got %q", ts.State)
	}
}

// TestSweepOverduePreservesLiveLease is the double-holder blocker guard: a task
// claimed with a STILL-LIVE lease that crosses its deadline flips to overdue but
// must NOT become stealable — Holder and LeaseUntil are preserved so a second
// agent's Claim is DENIED and the original holder can still Resolve.
func TestSweepOverduePreservesLiveLease(t *testing.T) {
	s, _ := newTestStore(t)
	s.leaseTTL = 15 * time.Minute
	t0 := time.Unix(1700000000, 0)
	seq, _ := s.Post(1, MessagePayload{Area: "a", From: "boss", Text: "t", Kind: "task", Deadline: t0.UnixNano()})

	if _, ok, _ := s.Claim(1, seq, "A", t0); !ok {
		t.Fatal("A's claim must succeed")
	}
	// Deadline has passed but A's lease (t0+15m) is still live.
	s.sweepOnce(t0.Add(time.Nanosecond))

	ts, _ := s.TaskOf(1, seq)
	if ts.State != "overdue" {
		t.Fatalf("state = %q, want overdue", ts.State)
	}
	if ts.Holder != "A" || ts.LeaseUntil != t0.UnixNano()+int64(s.leaseTTL) {
		t.Fatalf("overdue must preserve holder+lease, got holder=%q lease=%d", ts.Holder, ts.LeaseUntil)
	}
	// A different agent must NOT be able to steal the live (though overdue) lease.
	if _, ok, _ := s.Claim(1, seq, "B", t0.Add(2*time.Nanosecond)); ok {
		t.Fatal("overdue task with a live lease must reject a second claimer")
	}
	// The original holder can still resolve it.
	if err := s.Resolve(1, seq, "A", "done", t0.Add(3*time.Nanosecond)); err != nil {
		t.Fatalf("holder must still be able to resolve an overdue-but-held task: %v", err)
	}
}

// TestSweepDropsTTL: an expired ephemeral message is dropped from the index by the
// sweeper (Read no longer returns it) while the journal record is retained (the
// sweeper adds zero journal records).
func TestSweepDropsTTL(t *testing.T) {
	s, _ := newTestStore(t)
	t0 := time.Unix(1700000000, 0)
	if _, err := s.Post(1, MessagePayload{Area: "a", From: "x", Text: "ttl", ExpiresAt: t0.UnixNano()}); err != nil {
		t.Fatal(err)
	}
	if len(s.Read(1, "a", 0)) != 1 {
		t.Fatal("message must be present before it expires")
	}
	before := s.journal.ReadNextSeq()

	s.sweepOnce(t0.Add(time.Nanosecond))

	if got := s.Read(1, "a", 0); len(got) != 0 {
		t.Fatalf("expired message must be dropped from the index: %+v", got)
	}
	if after := s.journal.ReadNextSeq(); after != before {
		t.Fatalf("sweeper must retain the journal record: nextSeq %d -> %d", before, after)
	}
}

// TestSweepDropsStalePresence: a presence slot older than presenceStale is dropped
// by the sweep; a fresh one survives.
func TestSweepDropsStalePresence(t *testing.T) {
	s, _ := newTestStore(t)
	s.presenceStale = 15 * time.Minute
	t0 := time.Unix(1700000000, 0)
	if err := s.Heartbeat(1, "stale", "ops", "up", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Heartbeat(1, "fresh", "ops", "up", t0.Add(14*time.Minute)); err != nil {
		t.Fatal(err)
	}

	s.sweepOnce(t0.Add(s.presenceStale + time.Second))

	got := s.PresenceList(1)
	if len(got) != 1 || got[0].Agent != "fresh" {
		t.Fatalf("sweep must drop only the stale slot, keep 'fresh': %+v", got)
	}
}

// TestSweepPublishesOutsideLock is the lock-order guard: the sweeper collects
// nudges under s.mu and publishes them AFTER unlocking. A subscriber receives the
// lease-freed nudge without deadlock (bounded timeout). Run under -race.
func TestSweepPublishesOutsideLock(t *testing.T) {
	s, _ := newTestStore(t)
	s.leaseTTL = time.Minute
	seq, _ := s.Post(1, MessagePayload{Area: "a", From: "boss", Text: "t", Kind: "task"})
	// Claim far in the past so the lease is already expired at real now → the next
	// sweeper tick flips it to pending and nudges area "a".
	if _, ok, _ := s.Claim(1, seq, "A", time.Unix(1, 0)); !ok {
		t.Fatal("claim must succeed")
	}
	ch, cancelSub := s.Subscribe(Filter{Tenant: 1, Areas: []string{"a"}})
	defer cancelSub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx, 2*time.Millisecond)

	select {
	case n := <-ch:
		if n.Seq != seq {
			t.Fatalf("nudge seq = %d, want %d", n.Seq, seq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sweeper did not publish a nudge (deadlock or lock-order regression?)")
	}
}
