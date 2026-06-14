# Dedup Force Escape-Hatch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `force` escape-hatch so an agent can store a genuinely-distinct fact that falls within the dedup radius of an unrelated-but-vocabulary-similar memory, refactoring `Remember` to an options struct to stop parameter sprawl.

**Architecture:** `memory.Store.Remember` takes a `RememberOpts{Tags, Src, Supersedes, Force}` and, when `Force` is set, bypasses the dedup gate (supersede still takes precedence). `cmd/sentrymcp` exposes a `force` boolean on the `remember` tool; the `sentry-reflect` reflection prompt tells the agent to use it when a deduped fact is actually distinct.

**Tech Stack:** Pure Go, zero deps. Spec: `docs/superpowers/specs/2026-06-13-dedup-force-design.md`. Builds on the supersede-dedup feature.

---

## File Structure

- **Modify** `memory/memory.go` — add `RememberOpts`; change `Remember` signature + add the `Force` branch.
- **Modify** `memory/memory_test.go` — migrate all 28 `Remember` call sites to `RememberOpts`; add the force test.
- **Modify** `cmd/sentrymcp/main.go` — `boolArg` helper; `force` schema property; handler builds `RememberOpts`.
- **Modify** `cmd/sentry-reflect/main.go` + `main_test.go` — reflection prompt clause + test.
- **Deploy (Task 4):** redeploy `sentrymcp` (VM), reinstall `sentry-reflect` (Mac), live-verify the real case.

---

## Task 1: `memory` — `RememberOpts` + `Force` branch

**Files:** Modify `memory/memory.go` (`Remember` at `:139`), `memory/memory_test.go` (28 call sites)

- [ ] **Step 1: Write the failing test** — append to `memory/memory_test.go`:

```go
func TestRememberForceStoresNearDuplicate(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	st.DedupThreshold = 0.05 // kitten(0.01) is within tau of cat
	st.Remember(1, "cat", RememberOpts{})
	// WITHOUT force this would dedup; WITH force it must store anyway.
	_, dup, _, err := st.Remember(1, "kitten", RememberOpts{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Fatalf("force must not dedup")
	}
	if n := st.Count(1); n != 2 {
		t.Fatalf("count = %d, want 2 (forced store)", n)
	}
}

func TestSupersedeTakesPrecedenceOverForce(t *testing.T) {
	st, _ := newTestStore(t, geoEmbedder())
	st.DedupThreshold = 0.05
	id1, _, _, _ := st.Remember(1, "cat", RememberOpts{})
	// Both Supersedes and Force set: supersede wins (replaces id1, count stays 1).
	_, _, superseded, err := st.Remember(1, "kitten", RememberOpts{Supersedes: id1, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if superseded != id1 {
		t.Fatalf("superseded = %d, want %d (supersede precedence)", superseded, id1)
	}
	if n := st.Count(1); n != 1 {
		t.Fatalf("count = %d, want 1 (replaced, not added)", n)
	}
}
```

- [ ] **Step 2: Migrate all existing call sites** so the package compiles. The signature changes from
`Remember(tenant, text, tags, src, supersedes)` to `Remember(tenant, text, opts)`. Returns are UNCHANGED
(the 4-tuple stays), so left-hand captures don't change — only the arguments. Apply these exact translations
across `memory/memory_test.go` (run `grep -n '\.Remember(' memory/memory_test.go` to find all 28):

  - `Remember(T, X, nil, "", 0)` → `Remember(T, X, RememberOpts{})`
  - `Remember(1, "cat", []string{"animal"}, "Read", 0)` → `Remember(1, "cat", RememberOpts{Tags: []string{"animal"}, Src: "Read"})`
  - `Remember(1, "feline", nil, "", id1)` → `Remember(1, "feline", RememberOpts{Supersedes: id1})`
  - `Remember(1, "rocket", nil, "", 999)` → `Remember(1, "rocket", RememberOpts{Supersedes: 999})`

  General rule: the 3rd/4th/5th args (`tags, src, supersedes`) become fields of `RememberOpts`; a `nil` tag,
  `""` src, and `0` supersedes are simply omitted (zero values). The `text` may be a variable (e.g. `text`) —
  keep it as the 2nd positional arg.

- [ ] **Step 3: Run, verify FAIL**

Run: `go test ./memory/`
Expected: FAIL — `too many arguments` / `undefined: RememberOpts`.

- [ ] **Step 4: Add `RememberOpts` and rewrite `Remember`** in `memory/memory.go`. Add the type just above
`Remember`:

```go
// RememberOpts carries the optional inputs to Remember. Zero value = a plain
// store subject to the dedup gate.
type RememberOpts struct {
	Tags       []string
	Src        string
	Supersedes uint64 // replace this existing same-tenant memory (bypasses dedup)
	Force      bool   // store even if a near-duplicate exists (bypasses dedup)
}
```

Replace the whole `Remember` method (currently at `:139`) with:

```go
// Remember embeds text and persists it as an EventMemory, returning its id.
//
//   - opts.Supersedes names an existing same-tenant memory → the dedup gate is
//     bypassed, the new record carries a Supersedes pointer, the superseded id is
//     dropped from the index, and superseded is set to it. (Takes precedence over
//     Force.)
//   - else opts.Force → store without the dedup gate (persist even within tau).
//   - else → the dedup gate applies: if dedup is enabled (DedupThreshold > 0) and
//     the nearest same-tenant memory is within tau, the text is NOT persisted and
//     the existing id is returned with deduped=true.
func (s *Store) Remember(tenant sentry.TenantID, text string, opts RememberOpts) (id uint64, deduped bool, superseded uint64, err error) {
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

	// Supersede path: explicit replace, highest precedence, bypasses dedup.
	if opts.Supersedes != 0 && s.hasEntry(tenant, opts.Supersedes) {
		p := MemoryPayload{ID: s.nextID, Text: text, Vector: v, Tags: opts.Tags, Source: opts.Src, Supersedes: opts.Supersedes}
		if _, err := s.journal.Append(tenant, EventMemory, p); err != nil {
			return 0, false, 0, fmt.Errorf("memory: append: %w", err)
		}
		s.dropEntry(tenant, opts.Supersedes)
		s.entries = append(s.entries, entry{tenant: tenant, mem: p})
		s.nextID++
		return p.ID, false, opts.Supersedes, nil
	}

	// Dedup gate, unless the caller forces the store.
	if !opts.Force && s.DedupThreshold > 0 {
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

	p := MemoryPayload{ID: s.nextID, Text: text, Vector: v, Tags: opts.Tags, Source: opts.Src}
	if _, err := s.journal.Append(tenant, EventMemory, p); err != nil {
		return 0, false, 0, fmt.Errorf("memory: append: %w", err)
	}
	s.entries = append(s.entries, entry{tenant: tenant, mem: p})
	s.nextID++
	return p.ID, false, 0, nil
}
```

(`hasEntry` and `dropEntry` already exist from the supersede feature — do not redefine them.)

- [ ] **Step 5: Run, verify PASS**

Run: `go test ./memory/ -race && go vet ./memory/`
Expected: all tests PASS (migrated existing + 2 new force tests), race + vet clean.

- [ ] **Step 6: Commit**

```bash
git add memory/
git commit -m "feat(memory): RememberOpts + Force escape-hatch (store within-tau distinct facts; supersede precedence)"
```
End the commit body with:
Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>

---

## Task 2: `cmd/sentrymcp` — expose `force` on the remember tool

**Files:** Modify `cmd/sentrymcp/main.go` (remember tool schema; handler `:397-399`; helpers near `:512`)

- [ ] **Step 1: Write the failing test** → append to the sentrymcp test file (find it with `ls cmd/sentrymcp/*_test.go`; add to the main test file):

```go
func TestBoolArg(t *testing.T) {
	if !boolArg(map[string]any{"force": true}, "force") {
		t.Fatal("true not parsed")
	}
	if boolArg(map[string]any{"force": false}, "force") {
		t.Fatal("false not parsed")
	}
	if boolArg(map[string]any{}, "force") {
		t.Fatal("missing should be false")
	}
	if boolArg(map[string]any{"force": "yes"}, "force") {
		t.Fatal("non-bool should be false")
	}
}
```

- [ ] **Step 2: Run, verify FAIL** — `go test ./cmd/sentrymcp/ -run TestBoolArg` → `undefined: boolArg`.

- [ ] **Step 3: Add `boolArg`** next to `strArg`/`uintArg` (around `cmd/sentrymcp/main.go:512`):

```go
// boolArg reads a boolean arg; missing or non-bool yields false.
func boolArg(args map[string]any, key string) bool {
	b, _ := args[key].(bool)
	return b
}
```

- [ ] **Step 4: Add the `force` schema property** to the remember tool's `inputSchema.properties` (next to
`supersedes`):

```go
					"force": map[string]any{"type": "boolean", "description": "store even if a near-duplicate already exists; use only when your fact is genuinely distinct from what recall/remember reports, not a restatement"},
```

- [ ] **Step 5: Update the handler** (`cmd/sentrymcp/main.go:397-399`). It currently reads `supersedes` and
calls the old positional `Remember`. Change to build `RememberOpts`:

```go
		supersedes := uintArg(p.Args, "supersedes")
		force := boolArg(p.Args, "force")
		s.mu.Lock()
		id, deduped, superseded, err := s.mem.Remember(s.tenant, text, memory.RememberOpts{Tags: tags, Src: src, Supersedes: supersedes, Force: force})
		s.mu.Unlock()
```

(The `tags`/`src` locals are already read just above; keep them. The moko log + the response switch on
`superseded`/`deduped`/`supersedes` are unchanged. `memory` is already imported.)

- [ ] **Step 6: Build, vet, test, module green**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all green (no other caller uses the old signature outside memory/sentrymcp).

- [ ] **Step 7: Commit**

```bash
git add cmd/sentrymcp/
git commit -m "feat(sentrymcp): remember accepts force:true to store within-tau distinct facts"
```
End the commit body with:
Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>

---

## Task 3: `cmd/sentry-reflect` — teach the prompt to force-store distinct facts

**Files:** Modify `cmd/sentry-reflect/main.go` (`reflectionPrompt`), `cmd/sentry-reflect/main_test.go`

- [ ] **Step 1: Write the failing test** — append to `cmd/sentry-reflect/main_test.go`:

```go
func TestReflectionPromptAdvertisesForce(t *testing.T) {
	if !strings.Contains(reflectionPrompt, "force") {
		t.Fatal("reflectionPrompt should tell the agent to force-store genuinely-distinct deduped facts")
	}
}
```

- [ ] **Step 2: Run, verify FAIL** — `go test ./cmd/sentry-reflect/ -run Force` → fails (no "force").

- [ ] **Step 3: Add the clause** to `reflectionPrompt` in `cmd/sentry-reflect/main.go`. The constant currently
ends with the supersede clause then "Do NOT store transient state…". Insert the force clause right after the
supersede clause. The relevant span currently reads:

```go
	"with supersedes set to that memory's id to replace it instead of storing a contradicting duplicate. " +
	"Do NOT store transient state, file contents, task progress, or anything already in the code or git. " +
```

Replace it with:

```go
	"with supersedes set to that memory's id to replace it instead of storing a contradicting duplicate. " +
	"If remember reports your fact was deduped but it is genuinely distinct (not a restatement of the named " +
	"memory), call remember again with force set to true to store it anyway. " +
	"Do NOT store transient state, file contents, task progress, or anything already in the code or git. " +
```

- [ ] **Step 4: Run, verify PASS** — `go test ./cmd/sentry-reflect/ && go vet ./cmd/sentry-reflect/` (all PASS).

- [ ] **Step 5: Commit**

```bash
git add cmd/sentry-reflect/
git commit -m "feat(sentry-reflect): prompt instructs force:true for genuinely-distinct deduped facts"
```
End the commit body with:
Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>

---

## Task 4: Deploy + live verification (controller-executed)

- [ ] **Step 1: Full green gate** — `go build ./... && go test ./...` (all green).
- [ ] **Step 2: Reinstall the hook (Mac)** — `go build -o ~/.local/bin/sentry-reflect ./cmd/sentry-reflect && echo INSTALLED`.
- [ ] **Step 3: Redeploy sentrymcp (VM)** —
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/sentrymcp ./cmd/sentrymcp
scp /tmp/sentrymcp matrix-sentry:/root/sentrymcp.new
ssh matrix-sentry 'cp /root/sentrymcp /root/sentrymcp.bak && mv /root/sentrymcp.new /root/sentrymcp && chmod +x /root/sentrymcp && systemctl restart sentrymcp && sleep 2 && systemctl is-active sentrymcp'
```
Expected: `active`.
- [ ] **Step 4: Live-verify the real case** — re-`remember` the GP-feasibility finding (the fact that deduped
against #22 last session) WITH `force:true`, through the live endpoint (token+URL from `~/.matrix-sentry.env`),
and confirm the response is `remembered as memory #N` (a new id, NOT "already known as #22"). Then `recall` a
GP-related query and confirm both #22 (engine config) and the new GP fact are retrievable — proving the
distinct fact now coexists with its vocabulary-similar neighbor.
- [ ] **Step 5: Update HANDOFF + memory** with the force escape-hatch (the within-τ triad is now complete:
skip / supersede / force) and that the GP finding is now in the live corpus. Commit.

---

## Notes for the implementer

- **Returns are unchanged** (4-tuple `id, deduped, superseded, err`) — only the input changed to `RememberOpts`. Force adds no return; a forced store is `deduped=false, superseded=0`.
- **Precedence:** supersede > force > dedup. The code checks supersede first, then `!Force && DedupThreshold>0`.
- **Migration is mechanical** but touches 28 call sites — translate args to struct fields, leave captures alone. Run the tests to catch any missed site.
- `hasEntry`/`dropEntry` already exist (supersede feature) — reuse, don't redefine.
