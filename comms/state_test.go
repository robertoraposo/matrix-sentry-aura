package comms

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"matrixsentry/sentry"
)

// countClaimedStateEvents scans the journal for EventMessageState records with
// State=="claimed" for a given seq — the definitive durable single-winner check
// (losers must not append). Same package, so s.journal is reachable.
func countClaimedStateEvents(t *testing.T, s *Store, seq uint64) int {
	t.Helper()
	etype := EventMessageState
	n := 0
	if err := s.journal.Scan(sentry.Filter{Type: &etype}, func(r sentry.Record) bool {
		var sp StatePayload
		if err := sentry.UnmarshalPayload(r.Payload, &sp); err != nil {
			t.Fatalf("decode state event: %v", err)
		}
		if sp.Seq == seq && sp.State == "claimed" {
			n++
		}
		return true
	}); err != nil {
		t.Fatalf("scan state events: %v", err)
	}
	return n
}

// TestClaimSingleWinner is the concurrency contract: N goroutines with DISTINCT
// labels claim the same task at a single fixed `now`; exactly ONE wins. Distinct
// `by` (so a renewal cannot manufacture a second winner) + one shared `now` (so a
// lease-steal cannot either) + a start gate to maximize contention. The durable
// proof is that the journal holds exactly ONE claimed record (losers append none).
// Run the package under -race.
func TestClaimSingleWinner(t *testing.T) {
	s, _ := newTestStore(t)
	s.leaseTTL = 15 * time.Minute
	seq, err := s.Post(1, MessagePayload{Area: "a", From: "boss", Text: "do it", Kind: "task"})
	if err != nil {
		t.Fatal(err)
	}

	const N = 50
	now := time.Unix(1700000000, 0)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	winner := ""
	for i := 0; i < N; i++ {
		by := fmt.Sprintf("agent-%d", i)
		wg.Add(1)
		go func(by string) {
			defer wg.Done()
			<-start
			ts, ok, err := s.Claim(1, seq, by, now)
			if err != nil {
				t.Errorf("Claim(%s) err = %v", by, err)
				return
			}
			if ok {
				mu.Lock()
				wins++
				winner = ts.Holder
				mu.Unlock()
			}
		}(by)
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("exactly one claimer must win, got %d", wins)
	}
	got, ok := s.TaskOf(1, seq)
	if !ok || got.Holder != winner {
		t.Fatalf("TaskOf holder = %q ok=%v, want winner %q", got.Holder, ok, winner)
	}
	if c := countClaimedStateEvents(t, s, seq); c != 1 {
		t.Fatalf("journal must hold exactly ONE claimed record, got %d", c)
	}
}

// TestClaimLeaseSteal: A holds until t0+leaseTTL; B claims one ns past expiry and
// becomes the holder.
func TestClaimLeaseSteal(t *testing.T) {
	s, _ := newTestStore(t)
	s.leaseTTL = 15 * time.Minute
	seq, _ := s.Post(1, MessagePayload{Area: "a", From: "boss", Text: "t", Kind: "task"})

	t0 := time.Unix(1700000000, 0)
	if _, ok, _ := s.Claim(1, seq, "A", t0); !ok {
		t.Fatal("A's first claim must succeed")
	}
	stealAt := t0.Add(s.leaseTTL + 1)
	ts, ok, err := s.Claim(1, seq, "B", stealAt)
	if err != nil || !ok {
		t.Fatalf("B must steal expired lease: ok=%v err=%v", ok, err)
	}
	if ts.Holder != "B" {
		t.Fatalf("holder after steal = %q, want B", ts.Holder)
	}
}

// TestClaimRenewByHolder: same holder re-claims → still the holder, lease extended.
func TestClaimRenewByHolder(t *testing.T) {
	s, _ := newTestStore(t)
	s.leaseTTL = 15 * time.Minute
	seq, _ := s.Post(1, MessagePayload{Area: "a", From: "boss", Text: "t", Kind: "task"})

	t0 := time.Unix(1700000000, 0)
	first, ok, _ := s.Claim(1, seq, "A", t0)
	if !ok {
		t.Fatal("A's claim must succeed")
	}
	renew, ok, err := s.Claim(1, seq, "A", t0.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("holder renew must succeed: ok=%v err=%v", ok, err)
	}
	if renew.Holder != "A" {
		t.Fatalf("renew changed holder to %q, want A", renew.Holder)
	}
	if renew.LeaseUntil <= first.LeaseUntil {
		t.Fatalf("renew must extend lease: %d !> %d", renew.LeaseUntil, first.LeaseUntil)
	}
}

// TestResolveHolderOnly: only the holder (or owner) may resolve; a resolved task
// is terminal and rejects any further claim — including a re-claim by its former
// holder, guarding the &&/|| precedence trap in the claimability expression.
func TestResolveHolderOnly(t *testing.T) {
	s, _ := newTestStore(t)
	s.leaseTTL = 15 * time.Minute
	now := time.Unix(1700000000, 0)
	seq, _ := s.Post(1, MessagePayload{Area: "a", From: "boss", Text: "t", Kind: "task"})

	if _, ok, _ := s.Claim(1, seq, "worker", now); !ok {
		t.Fatal("claim must succeed")
	}
	if err := s.Resolve(1, seq, "other", "done", now); err == nil {
		t.Fatal("non-holder resolve must error")
	}
	if err := s.Resolve(1, seq, "worker", "done", now); err != nil {
		t.Fatalf("holder resolve must succeed: %v", err)
	}
	if _, ok, _ := s.Claim(1, seq, "worker2", now); ok {
		t.Fatal("resolved (terminal) task must reject a new claimer")
	}
	if _, ok, _ := s.Claim(1, seq, "worker", now); ok {
		t.Fatal("resolved (terminal) task must reject re-claim by its former holder")
	}
}

// TestResolveOwnerOverride: the owner label may resolve a task held by someone else.
func TestResolveOwnerOverride(t *testing.T) {
	s, _ := newTestStore(t)
	s.leaseTTL = 15 * time.Minute
	s.SetOwnerLabel("alvin")
	now := time.Unix(1700000000, 0)
	seq, _ := s.Post(1, MessagePayload{Area: "a", From: "boss", Text: "t", Kind: "task"})

	if _, ok, _ := s.Claim(1, seq, "worker", now); !ok {
		t.Fatal("claim must succeed")
	}
	if err := s.Resolve(1, seq, "alvin", "cancel", now); err != nil {
		t.Fatalf("owner override resolve must succeed: %v", err)
	}
	ts, _ := s.TaskOf(1, seq)
	if ts.State != "cancel" {
		t.Fatalf("state after owner override = %q, want cancel", ts.State)
	}
}

// TestStateRebuild: post→claim→resolve, reopen the journal → terminal state is
// reconstructed by last-wins EventMessageState replay.
func TestStateRebuild(t *testing.T) {
	dir := t.TempDir()
	j, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(j)
	if err != nil {
		t.Fatal(err)
	}
	s.leaseTTL = 15 * time.Minute
	now := time.Unix(1700000000, 0)
	seq, _ := s.Post(1, MessagePayload{Area: "a", From: "boss", Text: "t", Kind: "task"})
	if _, ok, _ := s.Claim(1, seq, "worker", now); !ok {
		t.Fatal("claim must succeed")
	}
	if err := s.Resolve(1, seq, "worker", "done", now); err != nil {
		t.Fatal(err)
	}
	j.Close()

	j2, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := New(j2)
	if err != nil {
		t.Fatal(err)
	}
	ts, ok := s2.TaskOf(1, seq)
	if !ok || ts.State != "done" {
		t.Fatalf("rebuilt task state = %+v ok=%v, want done", ts, ok)
	}
}

// TestRebuildUnactionedTaskClaimable is the never-claimed-task blocker guard: a
// kind=task posted and NEVER claimed emits no EventMessageState, so absence must
// mean pending after a restart. New must re-seed pending for every kind=task
// entry; otherwise the task is permanently unclaimable after the first reopen.
func TestRebuildUnactionedTaskClaimable(t *testing.T) {
	dir := t.TempDir()
	j, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(j)
	if err != nil {
		t.Fatal(err)
	}
	s.leaseTTL = 15 * time.Minute
	seq, _ := s.Post(1, MessagePayload{Area: "a", From: "boss", Text: "unclaimed", Kind: "task"})
	j.Close()

	j2, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := New(j2)
	if err != nil {
		t.Fatal(err)
	}
	s2.leaseTTL = 15 * time.Minute
	ts, ok := s2.TaskOf(1, seq)
	if !ok || ts.State != "pending" {
		t.Fatalf("un-actioned task must rebuild as pending: %+v ok=%v", ts, ok)
	}
	_, cok, err := s2.Claim(1, seq, "worker", time.Now())
	if err != nil || !cok {
		t.Fatalf("un-actioned task must be claimable after restart: ok=%v err=%v", cok, err)
	}
}

// TestClaimNotATask: claiming a plain (non-task) message errors.
func TestClaimNotATask(t *testing.T) {
	s, _ := newTestStore(t)
	seq, _ := s.Post(1, MessagePayload{Area: "a", From: "x", Text: "just a note"})
	if _, ok, err := s.Claim(1, seq, "worker", time.Now()); ok || err == nil {
		t.Fatalf("claiming a non-task must error: ok=%v err=%v", ok, err)
	}
	// Cross-tenant: tenant 2 cannot claim tenant 1's task.
	tseq, _ := s.Post(1, MessagePayload{Area: "a", From: "x", Text: "t", Kind: "task"})
	if _, ok, err := s.Claim(2, tseq, "worker", time.Now()); ok || err == nil {
		t.Fatalf("cross-tenant claim must error: ok=%v err=%v", ok, err)
	}
}
