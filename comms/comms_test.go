package comms

import (
	"testing"
	"time"

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

func TestRetentionCountAndTime(t *testing.T) {
	st, _ := newTestStore(t)
	for i := 0; i < 5; i++ {
		st.Post(1, MessagePayload{Area: "x", From: "a", Text: "m"})
	}
	st.SetRetention(3, 0) // count only
	if got := len(st.Recent(1, 100)); got != 3 {
		t.Fatalf("count retention: want 3, got %d", got)
	}

	now := time.Now().UnixNano()
	st2, _ := newTestStore(t)
	st2.SetRetention(0, time.Hour) // time only
	st2.entries = []Message{
		{Seq: 1, Tenant: 1, TS: now - 2*int64(time.Hour), Area: "x", Text: "old"},
		{Seq: 2, Tenant: 1, TS: now - 10*int64(time.Minute), Area: "x", Text: "fresh"},
	}
	st2.pruneAt(now)
	if g := st2.Recent(1, 100); len(g) != 1 || g[0].Text != "fresh" {
		t.Fatalf("time retention: want only 'fresh', got %+v", g)
	}

	st3, _ := newTestStore(t)
	st3.SetRetention(1, time.Hour) // both
	st3.entries = []Message{
		{Seq: 1, Tenant: 1, TS: now - 10*int64(time.Minute), Area: "x", Text: "older-fresh"},
		{Seq: 2, Tenant: 1, TS: now - 5*int64(time.Minute), Area: "x", Text: "newest"},
	}
	st3.pruneAt(now)
	if g := st3.Recent(1, 100); len(g) != 1 || g[0].Text != "newest" {
		t.Fatalf("count∩time: want only 'newest', got %+v", g)
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

func TestPostImageAndGetBySeq(t *testing.T) {
	st, _ := newTestStore(t) // existing helper that wraps a temp journal in comms.New
	seq, err := st.PostImage(1, MessagePayload{
		Area: "ui", From: "designer", Mime: "image/png",
		BlobID: "abc123", W: 800, H: 600, Size: 4096, Text: "login mock",
	})
	if err != nil {
		t.Fatalf("PostImage: %v", err)
	}

	// Read surfaces the ref (no bytes), Kind forced to "image".
	got := st.Read(1, "ui", 0)
	if len(got) != 1 || got[0].BlobID != "abc123" || got[0].Kind != "image" || got[0].Mime != "image/png" {
		t.Fatalf("Read image msg = %+v", got)
	}

	// GetBySeq returns the tenant's message regardless of area.
	m, ok := st.GetBySeq(1, seq)
	if !ok || m.BlobID != "abc123" || m.W != 800 {
		t.Fatalf("GetBySeq = %+v ok=%v", m, ok)
	}
	// Tenant isolation: another tenant cannot read it.
	if _, ok := st.GetBySeq(2, seq); ok {
		t.Fatal("GetBySeq must be tenant-scoped")
	}

	// Missing blob/mime is rejected.
	if _, err := st.PostImage(1, MessagePayload{Area: "ui", From: "x"}); err == nil {
		t.Fatal("PostImage without BlobID/Mime must error")
	}
}

func TestClearAreaTombstoneSurvivesReopen(t *testing.T) {
	st, dir := newTestStore(t)
	st.Post(1, MessagePayload{Area: "X", From: "a", Text: "x1"})
	st.Post(1, MessagePayload{Area: "X", From: "a", Text: "x2"})
	st.Post(1, MessagePayload{Area: "Y", From: "a", Text: "y1"})
	st.Post(2, MessagePayload{Area: "X", From: "z", Text: "other-tenant"})

	cleared, err := st.Clear(1, "X")
	if err != nil {
		t.Fatal(err)
	}
	if cleared != 2 {
		t.Fatalf("want 2 cleared, got %d", cleared)
	}
	if len(st.Read(1, "X", 0)) != 0 {
		t.Fatal("X not cleared in index")
	}
	if len(st.Read(1, "Y", 0)) != 1 {
		t.Fatal("Y must be untouched")
	}
	if len(st.Read(2, "X", 0)) != 1 {
		t.Fatal("tenant 2's X must be untouched")
	}

	st.Post(1, MessagePayload{Area: "X", From: "a", Text: "x3-after"})
	if g := st.Read(1, "X", 0); len(g) != 1 || g[0].Text != "x3-after" {
		t.Fatalf("post-clear message should survive: %+v", g)
	}

	jr, err := sentry.Open(dir, sentry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer jr.Close()
	re, err := New(jr)
	if err != nil {
		t.Fatal(err)
	}
	if g := re.Read(1, "X", 0); len(g) != 1 || g[0].Text != "x3-after" {
		t.Fatalf("after reopen, X should have only x3-after: %+v", g)
	}
	if len(re.Read(1, "Y", 0)) != 1 {
		t.Fatal("Y lost on reopen")
	}
}
