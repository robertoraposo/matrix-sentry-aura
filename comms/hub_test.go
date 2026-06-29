package comms

import (
	"testing"
	"time"
)

func TestHubDeliversOnMatchAndCoalesces(t *testing.T) {
	st, _ := newTestStore(t)

	// Subscribe by target.
	chT, cancelT := st.Subscribe(Filter{Tenant: 1, Target: "worker-a"})
	defer cancelT()
	// Subscribe by area.
	chA, cancelA := st.Subscribe(Filter{Tenant: 1, Areas: []string{"proj/x"}})
	defer cancelA()

	// A message directed at worker-a in some area → target subscriber gets it.
	seq, _ := st.Post(1, MessagePayload{Area: "anything", From: "o", Text: "do it", Target: "worker-a"})
	select {
	case n := <-chT:
		if n.Seq != seq {
			t.Fatalf("target nudge seq=%d want %d", n.Seq, seq)
		}
	case <-time.After(time.Second):
		t.Fatal("target subscriber got no nudge")
	}

	// A message in proj/x not directed at worker-a → area subscriber gets it, target does not.
	seq2, _ := st.Post(1, MessagePayload{Area: "proj/x", From: "o", Text: "fyi"})
	select {
	case n := <-chA:
		if n.Seq != seq2 {
			t.Fatalf("area nudge seq=%d want %d", n.Seq, seq2)
		}
	case <-time.After(time.Second):
		t.Fatal("area subscriber got no nudge")
	}

	// Tenant isolation: a different tenant's message must not deliver.
	st.Post(2, MessagePayload{Area: "proj/x", From: "o", Text: "other tenant", Target: "worker-a"})
	select {
	case n := <-chT:
		t.Fatalf("tenant isolation broken: got nudge %+v", n)
	case <-time.After(100 * time.Millisecond):
	}

	// Coalescing / non-blocking: with a full buffer, many posts must not block Post.
	for i := 0; i < 50; i++ {
		st.Post(1, MessagePayload{Area: "flood", From: "o", Text: "x", Target: "worker-a"})
	}

	// MatchingSince finds the latest matching seq > since.
	if got := st.MatchingSince(Filter{Tenant: 1, Target: "worker-a"}, 0); got == 0 {
		t.Fatal("MatchingSince should find matching messages")
	}
	if got := st.MatchingSince(Filter{Tenant: 1, Target: "nobody"}, 0); got != 0 {
		t.Fatalf("MatchingSince for non-matching target = %d, want 0", got)
	}

	// Cancel stops delivery.
	cancelT()
	st.Post(1, MessagePayload{Area: "z", From: "o", Text: "after cancel", Target: "worker-a"})
	select {
	case _, open := <-chT:
		if open {
			t.Fatal("delivery continued after cancel")
		}
	case <-time.After(100 * time.Millisecond):
	}
}
