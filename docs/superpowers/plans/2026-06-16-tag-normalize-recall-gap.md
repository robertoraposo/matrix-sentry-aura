# Tag Normalization + Recall Gap-Truncation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Normalize memory tags (lowercase/trim/dedupe) at the index boundary, and truncate recall results at the first relevance cliff (ratio-based gap), to fix tag fragmentation and fixed-k padding.

**Architecture:** Pure `memory` package changes (`normalizeTags`, `Store.RecallGap`) + a one-line env wiring in `cmd/sentrymcp/main.go`. Storage layer untouched; on-disk journal stays verbatim, the live index normalizes on rebuild.

**Tech Stack:** Go (strings, sort), the existing `memory.Store`.

---

### Task 1: `normalizeTags` + apply on write and rebuild

**Files:** Modify `memory/memory.go`, `memory/memory_test.go`.

CONTEXT: `Remember` (memory/memory.go ~176) builds `MemoryPayload{... Tags: opts.Tags ...}` in TWO places — the supersede branch (~190) and the normal branch (~218). `New` (~91) rebuilds the index, appending `entry{tenant, mem: p}` at ~105 after decoding each `MemoryPayload p`. `MemoryPayload.Tags` is `[]string`.

- [ ] **Step 1: failing tests (append to memory/memory_test.go)**

```go
func TestNormalizeTags(t *testing.T) {
	got := normalizeTags([]string{"ASHLEY", "ashley", " Ashley ", "", "Bug"})
	want := []string{"ashley", "bug"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	if normalizeTags(nil) != nil {
		t.Fatal("nil tags → nil")
	}
}

func TestRememberStoresNormalizedTags(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder()) // match the real helper signature
	id, _, _, err := st.Remember(1, "alpha", RememberOpts{Tags: []string{"ASHLEY", " Bug"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range st.List(1) {
		if m.ID == id {
			if len(m.Tags) != 2 || m.Tags[0] != "ashley" || m.Tags[1] != "bug" {
				t.Fatalf("tags not normalized: %v", m.Tags)
			}
		}
	}
}

func TestRebuildNormalizesExistingTags(t *testing.T) {
	st, dir := newTestStore(t, geoEmbedder())
	st.Remember(1, "beta", RememberOpts{Tags: []string{"ASHLEY"}})
	// reopen the journal + store; rebuild must lowercase the persisted tag
	jr, err := sentry.Open(dir, sentry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer jr.Close()
	re, err := New(jr, geoEmbedder())
	if err != nil {
		t.Fatal(err)
	}
	got := re.List(1)
	if len(got) == 0 || len(got[0].Tags) == 0 || got[0].Tags[0] != "ashley" {
		t.Fatalf("rebuild did not normalize tags: %+v", got)
	}
}
```
ADAPT to the real test helpers: read `memory/memory_test.go` for `newTestStore`'s exact signature/return (the prior task found `newTestStore(t, emb) (*Store, string)` returning the dir; and `geoEmbedder()` or similar). Confirm `sentry.Open(dir, sentry.Options{})` is how a journal is opened in existing tests (match the existing reopen pattern — some tests may use a helper). Confirm the `sentry` import in the test file.

Run `go test ./memory/ -run 'TestNormalizeTags|TestRememberStoresNormalized|TestRebuildNormalizes' -v` → FAIL (normalizeTags undefined).

- [ ] **Step 2: implement**

Add to memory/memory.go (ensure `strings` is imported):
```go
// normalizeTags lowercases, trims, and de-duplicates tags (order-preserving) so
// case/whitespace variants ("ASHLEY", " ashley ") collapse to one canonical tag.
// Returns nil for no usable tags (keeps json omitempty behavior).
func normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		n := strings.ToLower(strings.TrimSpace(t))
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
```
In `Remember`, normalize once at the top of the function body (after the embed, before the branches), e.g. add `tags := normalizeTags(opts.Tags)` and use `Tags: tags` in BOTH `MemoryPayload` literals (supersede branch + normal branch) instead of `opts.Tags`.
In `New`, inside the EventMemory scan callback, before `s.entries = append(...)`, add `p.Tags = normalizeTags(p.Tags)`.

Run the tests → PASS. Then `go test ./memory/ -race`.

- [ ] **Step 3: commit**
```bash
git add memory/memory.go memory/memory_test.go
git commit -m "feat(memory): normalize tags (lowercase/trim/dedupe) on write + rebuild"
```

---

### Task 2: `Store.RecallGap` truncation

**Files:** Modify `memory/memory.go`, `memory/memory_test.go`.

CONTEXT: `Recall` (memory/memory.go ~257) embeds the query, scores all tenant entries by `sqL2`, sorts ascending by Score (ties by ID), caps `if len(scored) > k { scored = scored[:k] }`, returns. `Store` has `DedupThreshold float32`; add `RecallGap` next to it.

- [ ] **Step 1: failing test (append to memory/memory_test.go)**

```go
func TestRecallGapTruncatesPadding(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	// geoEmbedder maps known phrases to fixed points; pick texts so ONE is close
	// to the query and the rest are far past a 1.25 ratio cliff. Use the same
	// phrases the existing recall tests rely on (read them) — e.g. a near phrase
	// and far phrases. Assert: with RecallGap=1.25, Recall returns fewer than k
	// (the close one only / the pre-cliff group); with RecallGap=0, returns k.
	near := "indentation style" // adjust to geoEmbedder's actual near/far phrases
	for _, txt := range []string{"prefer tabs over spaces", "indentation style", "deploy on fridays"} {
		st.Remember(1, txt, RememberOpts{})
	}
	st.RecallGap = 0
	all, _ := st.Recall(1, near, 5)
	st.RecallGap = 1.25
	cut, _ := st.Recall(1, near, 5)
	if len(cut) < 1 {
		t.Fatal("must keep at least the top hit")
	}
	if !(len(cut) <= len(all)) {
		t.Fatalf("gap should not increase results: cut=%d all=%d", len(cut), len(all))
	}
	if len(all) > 1 && len(cut) >= len(all) {
		t.Fatalf("gap=1.25 should truncate padding past the cliff: cut=%d all=%d (dists %v)", len(cut), len(all), dists(all))
	}
}
```
IMPORTANT: read the existing `testEmbedder`/`geoEmbedder` in `memory/memory_test.go` (and `cmd/sentrymcp/main_test.go`'s `testEmbedder` maps "prefer tabs over spaces"→{0,0}, "indentation style"→{0.1,0}, "deploy on fridays"→{9,9}, unknown→{5,5}). Use whatever the memory package's test embedder actually maps so that, for the chosen query, the sorted squared-L2 distances have a real >1.25× cliff after the first (or first few) results. If the memory test embedder differs, ADAPT the phrases/query and the assertion so the test deterministically exercises a cliff. Add a tiny `dists` helper or inline the distances in the failure message. The assertion must prove: gap=1.25 returns strictly fewer than gap=0 when a cliff exists, and never fewer than 1.

Run `go test ./memory/ -run TestRecallGap -v` → FAIL.

- [ ] **Step 2: implement**

Add the field to the `Store` struct next to `DedupThreshold`:
```go
	// RecallGap, when > 1, truncates Recall results at the first relevance cliff:
	// the first position whose distance exceeds the previous one's by this ratio.
	// The top hit is always kept. 0/≤1 disables (plain top-k). Ratio-based so it
	// is embedder-agnostic (works for any vector dimension/scale).
	RecallGap float32
```
In `Recall`, after the `if len(scored) > k { scored = scored[:k] }` block and before `return scored, nil`, add:
```go
	if s.RecallGap > 1 && len(scored) > 1 {
		for i := 1; i < len(scored); i++ {
			if scored[i].Score > scored[i-1].Score*s.RecallGap {
				scored = scored[:i]
				break
			}
		}
	}
```

Run `go test ./memory/ -run TestRecallGap -v` → PASS. Then `go test ./memory/ -race` (all memory tests green — existing recall tests use the zero-value RecallGap=0 → unaffected).

- [ ] **Step 3: commit**
```bash
git add memory/memory.go memory/memory_test.go
git commit -m "feat(memory): RecallGap — truncate recall at the first relevance cliff (ratio, embedder-agnostic)"
```

---

### Task 3: wire `SENTRY_RECALL_GAP` + deploy

**Files:** Modify `cmd/sentrymcp/main.go`; then operational.

- [ ] **Step 1: add the flag + set it**

In `main()` next to the `dedupTau` flag (~line 81):
```go
	recallGap := flag.Float64("recall-gap", envFloat("SENTRY_RECALL_GAP", 1.25), "truncate recall at the first distance cliff of this ratio (0 = off, plain top-k)")
```
Where `s.mem.DedupThreshold = float32(*dedupTau)` is set (~line 142), add right after:
```go
		s.mem.RecallGap = float32(*recallGap)
```

- [ ] **Step 2: build + full suite**

Run `go build ./... && go test ./... -race && go vet ./cmd/sentrymcp/`. All green.

- [ ] **Step 3: commit**
```bash
git add cmd/sentrymcp/main.go
git commit -m "feat(sentrymcp): wire SENTRY_RECALL_GAP (default 1.25) into recall"
```

- [ ] **Step 4: deploy 8808 + 8809 (additive; rebuild auto-normalizes existing tags in the live index)**
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/sentrymcp ./cmd/sentrymcp
scp -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes /tmp/sentrymcp root@10.10.10.96:/root/sentrymcp.new
ssh matrix-sentry 'mv /root/sentrymcp /root/sentrymcp.old && mv /root/sentrymcp.new /root/sentrymcp && chmod +x /root/sentrymcp && systemctl restart sentrymcp && sleep 2 && systemctl is-active sentrymcp'
scp -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes /tmp/sentrymcp root@10.10.10.175:/root/sentrymcp.new
ssh matrix-sentry2 'mv /root/sentrymcp /root/sentrymcp.old && mv /root/sentrymcp.new /root/sentrymcp && chmod +x /root/sentrymcp && systemctl restart sentrymcp-mt && sleep 2 && systemctl is-active sentrymcp-mt'
```

- [ ] **Step 5: verify on real data** — on 8808 with the token: (a) `/admin/corpus` tag histogram shows NO uppercase `ASHLEY` (all `ashley`); (b) the dedup-τ recall probe (`recall "cómo se calibró el umbral de dedup tau"`) returns just the exact hit (#65) — the 1.42 padding trimmed by the gap. Compare count before/after.

- [ ] **Step 6: update HANDOFF + commit**
Add a short "EFFECTIVENESS FIXES (tag normalize + recall gap)" note. Commit.
```bash
git add HANDOFF.md && git commit -m "docs: deployed tag normalization + recall gap-truncation"
```
