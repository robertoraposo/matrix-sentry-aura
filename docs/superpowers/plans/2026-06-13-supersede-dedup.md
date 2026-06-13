# Supersede-Dedup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an agent correct/replace an existing memory via an explicit `supersedes:<id>` pointer, so legitimate updates are not suppressed by the truth-blind semantic dedup gate.

**Architecture:** `memory.Store.Remember` gains a `supersedes uint64` parameter. When it names an existing same-tenant memory, Remember bypasses the dedup gate, appends a new `EventMemory` carrying a `Supersedes` pointer, and drops the old id from the in-RAM index (the append-only journal keeps full history on disk; the index holds current truth). `cmd/sentrymcp` exposes the pointer as an optional `supersedes` arg on the `remember` tool and reports the outcome; the `sentry-reflect` reflection prompt tells the agent to use it for outdated/contradicted memories.

**Tech Stack:** Pure Go, zero external deps. Builds on the shipped auto-remember feature ([[2026-06-13-auto-remember-design]]). Spec: `docs/superpowers/specs/2026-06-13-supersede-dedup-design.md`.

---

## File Structure

- **Modify** `memory/memory.go` — add `Supersedes` to `MemoryPayload`; new `Remember` arity + supersede branch; `New` rebuild drops superseded ids. The package's single responsibility (semantic memory over the journal) is unchanged.
- **Modify** `memory/memory_test.go` — update existing `Remember` call sites to the new arity; add supersede tests.
- **Modify** `cmd/sentrymcp/main.go` — `uintArg` helper; `remember` handler passes/report supersedes; tool schema gains the `supersedes` property.
- **Modify** `cmd/sentry-reflect/main.go` — one clause added to `reflectionPrompt`.
- **Modify** `cmd/sentry-reflect/main_test.go` — assert the prompt advertises the supersede affordance.
- **Deploy (Task 4):** rebuild/reinstall `~/.local/bin/sentry-reflect`; redeploy `sentrymcp` on the VM; verify live; update `HANDOFF.md` + memory.

---

## Task 1: `memory` — supersede pointer, gate bypass, rebuild drop

**Files:**
- Modify: `memory/memory.go` (`MemoryPayload` at `:28`, `New` at `:77-104`, `Remember` at `:106-149`)
- Test: `memory/memory_test.go`

- [ ] **Step 1: Write the failing tests** — append to `memory/memory_test.go`

```go
func TestSupersedeReplacesInIndex(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	id1, _, _, err := st.Remember(1, "cat", nil, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.Remember(1, "rocket", nil, "", 0); err != nil {
		t.Fatal(err)
	}
	// "feline" supersedes "cat": cat leaves the index, feline takes its place.
	newID, dup, superseded, err := st.Remember(1, "feline", nil, "", id1)
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Fatalf("supersede must not report deduped")
	}
	if superseded != id1 {
		t.Fatalf("superseded = %d, want %d", superseded, id1)
	}
	if newID == id1 {
		t.Fatalf("supersede must mint a new id, got the old %d", newID)
	}
	if n := st.Count(1); n != 2 { // cat dropped, feline added; rocket remains
		t.Fatalf("count = %d, want 2", n)
	}
	// recall near cat's old location: must surface feline (cat is gone), not cat.
	got, _ := st.Recall(1, "kitten", 3)
	for _, m := range got {
		if m.Text == "cat" {
			t.Fatalf("superseded 'cat' still recallable")
		}
	}
	if got[0].Text != "feline" {
		t.Fatalf("nearest = %q, want feline", got[0].Text)
	}
}

func TestSupersedeBypassesDedup(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	st.DedupThreshold = 0.05 // kitten is within 0.05 of cat -> would normally dedup
	id1, _, _, _ := st.Remember(1, "cat", nil, "", 0)
	// WITH supersedes set, the near-duplicate must REPLACE, not skip.
	newID, dup, superseded, err := st.Remember(1, "kitten", nil, "", id1)
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Fatalf("supersede path must not dedup")
	}
	if superseded != id1 || newID == id1 {
		t.Fatalf("expected replace: superseded=%d newID=%d (old %d)", superseded, newID, id1)
	}
	if n := st.Count(1); n != 1 {
		t.Fatalf("count = %d, want 1 (replaced)", n)
	}
}

func TestSupersedeInvalidIDStoresAsNew(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	st.Remember(1, "cat", nil, "", 0)
	// id 999 does not exist -> graceful store-as-new, superseded=0.
	_, _, superseded, err := st.Remember(1, "rocket", nil, "", 999)
	if err != nil {
		t.Fatal(err)
	}
	if superseded != 0 {
		t.Fatalf("superseded = %d, want 0 (id not found)", superseded)
	}
	if n := st.Count(1); n != 2 {
		t.Fatalf("count = %d, want 2 (stored as new)", n)
	}
}

func TestSupersedeIsTenantScoped(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	id1, _, _, _ := st.Remember(2, "cat", nil, "", 0) // tenant 2 owns id1
	// tenant 1 cannot supersede tenant 2's memory -> stored as new, t2 untouched.
	_, _, superseded, err := st.Remember(1, "kitten", nil, "", id1)
	if err != nil {
		t.Fatal(err)
	}
	if superseded != 0 {
		t.Fatalf("superseded = %d, want 0 (cross-tenant)", superseded)
	}
	if n := st.Count(2); n != 1 {
		t.Fatalf("tenant 2 count = %d, want 1 (untouched)", n)
	}
	if n := st.Count(1); n != 1 {
		t.Fatalf("tenant 1 count = %d, want 1", n)
	}
}

func TestSupersedeSurvivesReopen(t *testing.T) {
	emb := geoEmbedder()
	dir := t.TempDir()
	j, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	st, err := New(j, emb)
	if err != nil {
		t.Fatal(err)
	}
	id1, _, _, _ := st.Remember(1, "cat", nil, "", 0)
	st.Remember(1, "feline", nil, "", id1) // supersede cat
	j.Close()

	// Reopen: the rebuild must drop the superseded id and keep current truth.
	j2, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	st2, err := New(j2, emb)
	if err != nil {
		t.Fatal(err)
	}
	if n := st2.Count(1); n != 1 {
		t.Fatalf("after reopen count = %d, want 1", n)
	}
	got, _ := st2.Recall(1, "kitten", 3)
	if len(got) != 1 || got[0].Text != "feline" {
		t.Fatalf("after reopen recall = %+v, want only feline", got)
	}
	// ids are never reused: a fresh remember must mint id 3 (1=cat,2=feline used).
	id3, _, _, _ := st2.Remember(1, "rocket", nil, "", 0)
	if id3 != 3 {
		t.Fatalf("next id = %d, want 3 (no reuse)", id3)
	}
}
```

- [ ] **Step 2: Update existing call sites** — the signature gains BOTH a new trailing parameter `supersedes uint64` AND a new return value. Every existing call must change in two ways. Run:

```bash
grep -rn '\.Remember(' --include=*.go memory/
```

For each existing `st.Remember(...)` call in `memory/memory_test.go`:
1. **Add the argument** `, 0` as the new last parameter (these are non-supersede calls). E.g. `st.Remember(1, "cat", nil, "")` → `st.Remember(1, "cat", nil, "", 0)`. This applies to bare statement calls too — they will NOT compile without it ("not enough arguments").
2. **Capture the extra return** where returns are captured. The prior feature's dedup tests use 3-return forms: `id, dup, err := st.Remember(...)` → `id, dup, _, err :=`; `_, dup, _ := ...` → `_, dup, _, _ :=`. Earlier tests `_, _, err :=` → `_, _, _, err :=`. Bare statement calls capture nothing, so only change (1) applies to them.

(The new supersede tests above already use the final `(text, ..., supersedes)` arity and 4-return form.)

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./memory/`
Expected: FAIL — `Remember` returns 3 values / not enough arguments / `MemoryPayload` has no `Supersedes`.

- [ ] **Step 4: Add the `Supersedes` field** to `MemoryPayload` (`memory/memory.go:28-34`). The struct currently is:

```go
type MemoryPayload struct {
	ID     uint64    `json:"id"`
	Text   string    `json:"text"`
	Vector []float32 `json:"vec"`
	Tags   []string  `json:"tags,omitempty"`
	Source string    `json:"src,omitempty"`
}
```

Add one field at the end:

```go
type MemoryPayload struct {
	ID     uint64    `json:"id"`
	Text   string    `json:"text"`
	Vector []float32 `json:"vec"`
	Tags   []string  `json:"tags,omitempty"`
	Source string    `json:"src,omitempty"`
	// Supersedes, when non-zero, is the id of an earlier memory this record
	// replaces. On rebuild the superseded id is dropped from the in-RAM index;
	// the original record remains on disk (append-only journal = full history).
	Supersedes uint64 `json:"sup,omitempty"`
}
```

- [ ] **Step 5: Make `New` drop superseded ids on rebuild** (`memory/memory.go:77-104`). Inside the `journal.Scan` callback, after the existing `s.entries = append(...)` and `nextID` update, add the drop. Replace the callback body so it reads:

```go
	err := journal.Scan(sentry.Filter{Type: &etype}, func(r sentry.Record) bool {
		var p MemoryPayload
		if err := sentry.UnmarshalPayload(r.Payload, &p); err != nil {
			scanErr = fmt.Errorf("memory: decode record seq %d: %w", r.Seq, err)
			return false
		}
		if len(p.Vector) != embed.Dim() {
			scanErr = fmt.Errorf("memory: persisted vector dim %d != embedder dim %d (id %d)", len(p.Vector), embed.Dim(), p.ID)
			return false
		}
		s.entries = append(s.entries, entry{tenant: r.Tenant, mem: p})
		if p.ID >= s.nextID {
			s.nextID = p.ID + 1
		}
		if p.Supersedes != 0 {
			s.dropEntry(r.Tenant, p.Supersedes)
		}
		return true
	})
```

And add this helper method anywhere after `New` (e.g. just below it):

```go
// dropEntry removes the in-RAM entry for (tenant, id) if present. The journal
// record is untouched — only the live index (current truth) drops the id.
func (s *Store) dropEntry(tenant sentry.TenantID, id uint64) {
	for i := range s.entries {
		if s.entries[i].tenant == tenant && s.entries[i].mem.ID == id {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			return
		}
	}
}
```

(`dropEntry` does NOT take the lock — `New` runs before the store is shared, and `Remember` calls it while already holding `s.mu`.)

- [ ] **Step 6: Rewrite `Remember`** with the supersede branch (`memory/memory.go:106-149`). Replace the whole method:

```go
// Remember embeds text and persists it as an EventMemory, returning its id.
//
//   - supersedes == 0: normal path. If dedup is enabled (DedupThreshold > 0) and
//     the nearest same-tenant memory is within that squared-L2 radius, the text
//     is NOT persisted — the existing id is returned with deduped=true.
//   - supersedes names an existing same-tenant memory: the dedup gate is bypassed,
//     the new record carries a Supersedes pointer, the superseded id is dropped
//     from the in-RAM index, and superseded is set to it. The journal keeps the
//     old record (history on disk; current truth in the index).
//   - supersedes names a missing or foreign id: it is ignored and the call falls
//     back to the normal path (superseded stays 0) — never an error.
func (s *Store) Remember(tenant sentry.TenantID, text string, tags []string, src string, supersedes uint64) (id uint64, deduped bool, superseded uint64, err error) {
	vecs, err := s.embed.Embed([]string{text})
	if err != nil {
		return 0, false, 0, fmt.Errorf("memory: embed: %w", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != s.embed.Dim() {
		return 0, false, 0, fmt.Errorf("memory: embedder returned %d vectors / bad dim", len(vecs))
	}
	v := vecs[0]

	s.mu.Lock()
	defer s.mu.Unlock()

	// Supersede path: explicit intent replaces a known memory, bypassing dedup.
	if supersedes != 0 && s.hasEntry(tenant, supersedes) {
		p := MemoryPayload{ID: s.nextID, Text: text, Vector: v, Tags: tags, Source: src, Supersedes: supersedes}
		if _, err := s.journal.Append(tenant, EventMemory, p); err != nil {
			return 0, false, 0, fmt.Errorf("memory: append: %w", err)
		}
		s.dropEntry(tenant, supersedes)
		s.entries = append(s.entries, entry{tenant: tenant, mem: p})
		s.nextID++
		return p.ID, false, supersedes, nil
	}

	// Normal path: dedup gate, then store. (Reached when supersedes is 0 or names
	// a missing/foreign id.)
	if s.DedupThreshold > 0 {
		var bestID uint64
		var bestDist float32
		found := false
		for _, e := range s.entries {
			if e.tenant != tenant {
				continue
			}
			d := sqL2(v, e.mem.Vector)
			if !found || d < bestDist {
				found, bestDist, bestID = true, d, e.mem.ID
			}
		}
		if found && bestDist < s.DedupThreshold {
			return bestID, true, 0, nil
		}
	}

	p := MemoryPayload{ID: s.nextID, Text: text, Vector: v, Tags: tags, Source: src}
	if _, err := s.journal.Append(tenant, EventMemory, p); err != nil {
		return 0, false, 0, fmt.Errorf("memory: append: %w", err)
	}
	s.entries = append(s.entries, entry{tenant: tenant, mem: p})
	s.nextID++
	return p.ID, false, 0, nil
}

// hasEntry reports whether (tenant, id) is live in the index. Caller holds s.mu.
func (s *Store) hasEntry(tenant sentry.TenantID, id uint64) bool {
	for _, e := range s.entries {
		if e.tenant == tenant && e.mem.ID == id {
			return true
		}
	}
	return false
}
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `go test ./memory/`
Expected: PASS — all existing tests + the 5 new supersede tests. `go vet ./memory/` clean.

- [ ] **Step 8: Commit**

```bash
git add memory/
git commit -m "feat(memory): supersede via explicit supersedes pointer — replace in index, history on disk"
```
End the commit body with:
Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>

---

## Task 2: `cmd/sentrymcp` — expose `supersedes` on the remember tool

**Files:**
- Modify: `cmd/sentrymcp/main.go` (remember tool schema `:298-310`; handler `:386-406`; arg helpers near `:504`)

- [ ] **Step 1: Add a `uintArg` helper** near `strArg` (`cmd/sentrymcp/main.go:504`). JSON numbers decode to `float64` in `map[string]any`; handle that (and `json.Number` defensively). Non-positive / missing → 0.

```go
// uintArg reads a non-negative integer arg. JSON numbers arrive as float64 in a
// map[string]any; a json.Number is handled defensively. Missing/invalid -> 0.
func uintArg(args map[string]any, key string) uint64 {
	switch v := args[key].(type) {
	case float64:
		if v > 0 {
			return uint64(v)
		}
	case json.Number:
		if n, err := v.Int64(); err == nil && n > 0 {
			return uint64(n)
		}
	}
	return 0
}
```

(`encoding/json` is already imported in this file.)

- [ ] **Step 2: Add the `supersedes` property** to the remember tool schema (`cmd/sentrymcp/main.go:303-307`). The `properties` map currently lists `text`, `tags`, `src`. Add one entry:

```go
				"properties": map[string]any{
					"text":       map[string]any{"type": "string", "description": "the memory to store (a fact, decision, or piece of context)"},
					"tags":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "optional labels for grouping/filtering"},
					"src":        map[string]any{"type": "string", "description": "optional originating tool or context"},
					"supersedes": map[string]any{"type": "integer", "description": "optional id of an existing memory this fact updates or corrects; replaces it instead of storing a contradicting duplicate"},
				},
```

- [ ] **Step 3: Update the remember handler** (`cmd/sentrymcp/main.go:394-406`). It currently reads `src`/`tags`, calls the 3-return `Remember`, and branches on `deduped`. Replace from the `src` line through the final return with:

```go
		src, _ := strArg(p.Args, "src")
		tags := stringsArg(p.Args, "tags")
		supersedes := uintArg(p.Args, "supersedes")
		s.mu.Lock()
		id, deduped, superseded, err := s.mem.Remember(s.tenant, text, tags, src, supersedes)
		s.mu.Unlock()
		if err != nil {
			return s.toolErr(req.ID, "remember failed: "+err.Error())
		}
		s.moko.Info("remember", map[string]string{"tenant": fmt.Sprint(s.tenant), "id": fmt.Sprint(id), "tags": fmt.Sprint(tags), "len": fmt.Sprint(len(text)), "deduped": fmt.Sprint(deduped), "superseded": fmt.Sprint(superseded)})
		switch {
		case superseded != 0:
			return s.toolText(req.ID, fmt.Sprintf("remembered as memory #%d, superseding #%d", id, superseded))
		case deduped:
			return s.toolText(req.ID, fmt.Sprintf("already known as memory #%d (deduped, not stored again)", id))
		case supersedes != 0:
			return s.toolText(req.ID, fmt.Sprintf("superseded id #%d not found for this tenant; remembered as memory #%d", supersedes, id))
		default:
			return s.toolText(req.ID, fmt.Sprintf("remembered as memory #%d", id))
		}
```

- [ ] **Step 4: Build, vet, test the whole module**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: BUILD OK; vet clean; all packages green (no sentrymcp test calls `Remember` directly, so none need edits — if one does, update it to the 4-return arity and report it).

- [ ] **Step 5: Commit**

```bash
git add cmd/sentrymcp/
git commit -m "feat(sentrymcp): remember accepts supersedes:<id> and reports superseding outcome"
```
End the commit body with:
Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>

---

## Task 3: `cmd/sentry-reflect` — teach the reflection prompt to supersede

**Files:**
- Modify: `cmd/sentry-reflect/main.go` (`reflectionPrompt` const)
- Test: `cmd/sentry-reflect/main_test.go`

- [ ] **Step 1: Write the failing test** — append to `cmd/sentry-reflect/main_test.go`

```go
func TestReflectionPromptAdvertisesSupersede(t *testing.T) {
	// The prompt must tell the agent to update stale memories via supersedes,
	// not just store new ones — this is what closes the truth-blind-dedup gap.
	for _, want := range []string{"recall", "remember", "supersedes"} {
		if !strings.Contains(reflectionPrompt, want) {
			t.Fatalf("reflectionPrompt missing %q", want)
		}
	}
}
```

(`strings` is already imported in this test file.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/sentry-reflect/ -run TestReflectionPromptAdvertisesSupersede`
Expected: FAIL — prompt missing "supersedes".

- [ ] **Step 3: Add the supersede clause** to `reflectionPrompt` in `cmd/sentry-reflect/main.go`. The constant currently ends:

```go
	"self-contained and concise. Do NOT store transient state, file contents, task progress, or anything " +
	"already in the code or git. If nothing durable was learned, store nothing. Be terse — do not narrate " +
	"this to the user. Then finish."
```

Insert one sentence after the "remember once per genuinely-new fact …" clause and before the "Do NOT store…" clause. The full updated constant is:

```go
const reflectionPrompt = "Pause before finishing. Reflect on the work since your last memory checkpoint. " +
	"If — and only if — you learned durable knowledge (a decision made, a convention adopted, a gotcha " +
	"discovered) that a future session would benefit from, persist it: first call the recall tool to avoid " +
	"duplicating what is already stored, then call the remember tool once per genuinely-new fact, each fact " +
	"self-contained and concise. If recall surfaces a memory that is now outdated or wrong, call remember " +
	"with supersedes set to that memory's id to replace it instead of storing a contradicting duplicate. " +
	"Do NOT store transient state, file contents, task progress, or anything already in the code or git. " +
	"If nothing durable was learned, store nothing. Be terse — do not narrate this to the user. Then finish."
```

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/sentry-reflect/` (all PASS, including the new one), `go vet ./cmd/sentry-reflect/` (clean).

- [ ] **Step 5: Commit**

```bash
git add cmd/sentry-reflect/
git commit -m "feat(sentry-reflect): reflection prompt instructs supersedes for outdated memories"
```
End the commit body with:
Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>

---

## Task 4: Deploy + live verification

**Files:**
- Rebuild/install `~/.local/bin/sentry-reflect` (Mac); redeploy `sentrymcp` (VM); update `HANDOFF.md`.

- [ ] **Step 1: Full green gate**

Run: `go build ./... && go test ./...`
Expected: BUILD OK; all packages green.

- [ ] **Step 2: Rebuild + reinstall the hook (Mac)**

```bash
go build -o ~/.local/bin/sentry-reflect ./cmd/sentry-reflect && echo INSTALLED
```
Expected: `INSTALLED`. (No `settings.json` change — the Stop hook is already registered; only the binary's prompt changed.)

- [ ] **Step 3: Redeploy sentrymcp (VM)**

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/sentrymcp ./cmd/sentrymcp
scp /tmp/sentrymcp matrix-sentry:/root/sentrymcp.new
ssh matrix-sentry 'cp /root/sentrymcp /root/sentrymcp.bak && mv /root/sentrymcp.new /root/sentrymcp && chmod +x /root/sentrymcp && systemctl restart sentrymcp && sleep 2 && systemctl is-active sentrymcp'
```
Expected: `active`. (`SENTRY_DEDUP_TAU=0.85` already in the VM env from the prior deploy — unchanged.)

- [ ] **Step 4: Live verification — the #23 case that started this**

Using the live endpoint (token + URL from `~/.matrix-sentry.env`), confirm the supersede path works end-to-end and finally corrects the stale roadmap memory #23. Call `remember` with the corrected auto-remember-status text AND `supersedes: 23`:

```bash
cd /Users/alvinnunez
TOKEN=$(grep SENTRY_MCP_TOKEN ~/.matrix-sentry.env | head -1 | cut -d= -f2- | tr -d '"'"'"' ')
URL=$(grep SENTRY_MCP_URL ~/.matrix-sentry.env | head -1 | cut -d= -f2- | tr -d '"'"'"' ')
python3 - "$URL" "$TOKEN" <<'PY'
import sys,json,urllib.request
url,token=sys.argv[1],sys.argv[2]
def call(name,args):
    body=json.dumps({"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":name,"arguments":args}}).encode()
    req=urllib.request.Request(url,data=body,headers={"Content-Type":"application/json","Authorization":"Bearer "+token})
    with urllib.request.urlopen(req,timeout=30) as r: return json.load(r)
text=("Matrix Sentry auto-remember is LIVE (2026-06-13): the memory cycle is fully automatic. A global "
      "Stop hook (sentry-reflect, K=40) makes the agent self-report durable knowledge; the server dedups "
      "within squared-L2 tau=0.85 and now supports supersedes:<id> so corrections replace stale facts "
      "instead of being suppressed. All three legs (record_access, recall, remember) run with no manual step.")
r=call("remember",{"text":text,"tags":["matrix-sentry","status","auto-remember"],"src":"deploy-verify","supersedes":23})
print(r["result"]["content"][0]["text"])
PY
```
Expected: `remembered as memory #N, superseding #23`. Then a `recall` for "auto-remember status" should return the new memory and NOT the stale "remember is still manual" #23. Record the actual output.

- [ ] **Step 5: Update HANDOFF + memory**

Append a short "supersede-dedup LIVE" note to the auto-remember section of `HANDOFF.md` (the `supersedes:<id>` affordance + that #23 was corrected) and refresh the local memory file. Commit:

```bash
git add HANDOFF.md
git commit -m "docs: supersede-dedup live — supersedes:<id> corrects stale memories (e.g. #23)"
```
End the commit body with:
Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>

---

## Notes for the implementer

- **Append-only is the safety net:** supersede never deletes a journal record. `dropEntry` only mutates the in-RAM index. A wrong supersede is recoverable from disk.
- **Lock discipline:** `Remember` holds `s.mu` for the whole supersede branch (lookup + append + dropEntry + add). `hasEntry`/`dropEntry` assume the caller holds the lock (or, in `New`, that the store is not yet shared). Do not add locking inside them.
- **Rebuild ordering is load-bearing:** a superseding record always follows its target in the journal, so by the time `New` sees `Supersedes=X`, X is already in the index and `dropEntry` finds it. Don't reorder the Scan logic.
- **Ids never reuse:** `nextID` advances past superseded ids; the reopen test pins this.
- **K and τ are unchanged** by this lever; do not touch the calibration.
```
