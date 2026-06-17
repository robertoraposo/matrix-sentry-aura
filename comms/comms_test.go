package comms

import (
	"testing"

	"matrixsentry/sentry"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	j, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(j)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestPostReadRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	seq, err := s.Post(1, MessagePayload{Area: "proj/backend", From: "be", Text: "auth done"})
	if err != nil || seq == 0 {
		t.Fatalf("Post = (%d,%v)", seq, err)
	}
	got := s.Read(1, "proj/backend", 0)
	if len(got) != 1 || got[0].Text != "auth done" || got[0].From != "be" {
		t.Fatalf("Read = %+v", got)
	}
	if got[0].Kind != "note" {
		t.Fatalf("default kind = %q, want note", got[0].Kind)
	}
	if got[0].Seq != seq {
		t.Fatalf("msg seq %d != posted seq %d", got[0].Seq, seq)
	}
}

func TestReadSinceCursor(t *testing.T) {
	s, _ := newTestStore(t)
	s1, _ := s.Post(1, MessagePayload{Area: "a", From: "x", Text: "one"})
	s.Post(1, MessagePayload{Area: "a", From: "x", Text: "two"})
	s.Post(1, MessagePayload{Area: "a", From: "x", Text: "three"})
	got := s.Read(1, "a", s1)
	if len(got) != 2 || got[0].Text != "two" || got[1].Text != "three" {
		t.Fatalf("since-cursor read = %+v", got)
	}
}

func TestAreaFilter(t *testing.T) {
	s, _ := newTestStore(t)
	s.Post(1, MessagePayload{Area: "a", From: "x", Text: "in a"})
	s.Post(1, MessagePayload{Area: "b", From: "x", Text: "in b"})
	got := s.Read(1, "a", 0)
	if len(got) != 1 || got[0].Text != "in a" {
		t.Fatalf("area filter leaked: %+v", got)
	}
}

func TestTenantIsolation(t *testing.T) {
	s, _ := newTestStore(t)
	s.Post(2, MessagePayload{Area: "a", From: "x", Text: "tenant 2 only"})
	if got := s.Read(1, "a", 0); len(got) != 0 {
		t.Fatalf("tenant 1 saw tenant 2's messages: %+v", got)
	}
	if got := s.Read(2, "a", 0); len(got) != 1 {
		t.Fatalf("tenant 2 read = %+v, want its 1 message", got)
	}
}

func TestNoDedup(t *testing.T) {
	s, _ := newTestStore(t)
	s.Post(1, MessagePayload{Area: "a", From: "x", Text: "same text"})
	s.Post(1, MessagePayload{Area: "a", From: "y", Text: "same text"})
	if got := s.Read(1, "a", 0); len(got) != 2 {
		t.Fatalf("comms must NOT dedup; got %d, want 2", len(got))
	}
}

func TestGet(t *testing.T) {
	s, _ := newTestStore(t)
	seq, _ := s.Post(1, MessagePayload{Area: "a", From: "x", Text: "find me"})
	if m, ok := s.Get(1, "a", seq); !ok || m.Text != "find me" {
		t.Fatalf("Get = %+v ok=%v", m, ok)
	}
	if _, ok := s.Get(1, "a", 999999); ok {
		t.Fatal("Get of missing seq should be false")
	}
	if _, ok := s.Get(2, "a", seq); ok {
		t.Fatal("Get across tenant should be false")
	}
}

func TestPostRequiresFields(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.Post(1, MessagePayload{Area: "a", From: "x"}); err == nil {
		t.Fatal("empty text must error")
	}
	if _, err := s.Post(1, MessagePayload{Area: "a", Text: "t"}); err == nil {
		t.Fatal("empty from must error")
	}
	if _, err := s.Post(1, MessagePayload{From: "x", Text: "t"}); err == nil {
		t.Fatal("empty area must error")
	}
}

func TestSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	j, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := New(j)
	s.Post(1, MessagePayload{Area: "a", From: "x", Text: "persist me"})
	j.Close()

	j2, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := New(j2)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.Read(1, "a", 0)
	if len(got) != 1 || got[0].Text != "persist me" {
		t.Fatalf("after reopen read = %+v", got)
	}
}

func TestInboxFiltersByTarget(t *testing.T) {
	st, _ := newTestStore(t)
	st.Post(1, MessagePayload{Area: "x", From: "a", Text: "for me", Target: "me"})
	st.Post(1, MessagePayload{Area: "y", From: "b", Text: "also for me", Target: "me"})
	st.Post(1, MessagePayload{Area: "x", From: "a", Text: "for other", Target: "other"})
	st.Post(2, MessagePayload{Area: "x", From: "z", Text: "other tenant", Target: "me"})

	in := st.Inbox(1, "me", 0)
	if len(in) != 2 {
		t.Fatalf("want 2 inbox msgs across areas, got %d", len(in))
	}
	in2 := st.Inbox(1, "me", in[0].Seq)
	if len(in2) != 1 {
		t.Fatalf("since should return only newer, got %d", len(in2))
	}
}

func TestRecentReturnsLastN(t *testing.T) {
	st, _ := newTestStore(t)
	for i := 0; i < 5; i++ {
		st.Post(1, MessagePayload{Area: "x", From: "a", Text: "m"})
	}
	st.Post(2, MessagePayload{Area: "x", From: "z", Text: "other tenant"})
	got := st.Recent(1, 3)
	if len(got) != 3 {
		t.Fatalf("want last 3, got %d", len(got))
	}
	if !(got[0].Seq < got[1].Seq && got[1].Seq < got[2].Seq) {
		t.Fatalf("Recent should be seq-ascending: %+v", got)
	}
	for _, m := range got {
		if m.Tenant != 1 {
			t.Fatal("Recent leaked another tenant")
		}
	}
}
