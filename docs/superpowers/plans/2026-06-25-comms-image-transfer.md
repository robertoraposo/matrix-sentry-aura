# Comms Image Transfer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let visual/UI agents hand images to other agents through Matrix's comms channel, delivered as base64/MCP-native image content, without bloating the in-RAM comms index or the journal.

**Architecture:** Image bytes live in a new content-addressed blob store on disk (`<dir>/blobs/<sha256>`, global dedup). The journal/comms index carry only a tiny reference (a new `EventImage` message). New MCP tools `post_image`/`get_image` decode/encode base64 at the edges; `pin_image`/`blob_gc` implement the hybrid ephemeral-with-pin lifecycle. GC is orchestrated in `main` (`blob.Sweep(comms.LiveBlobIDs())`) so `comms` and `blob` stay decoupled.

**Tech Stack:** Pure Go, zero external dependencies, one binary (build on Mac → ship linux/amd64). Hand-rolled MCP server (no SDK). Module path `matrixsentry`.

## Global Constraints

- **Zero external dependencies** — stdlib only. Image dimension decode uses `image` + `image/png|jpeg|gif` ONLY (no `golang.org/x/image`); unknown formats store `0/0`.
- **One binary, build-on-Mac→ship-linux** — `GOOS=linux GOARCH=amd64 go build -trimpath`.
- **The journal is never mutated** — retention/clear/GC only affect the in-RAM index and blob *files*; the append-only journal keeps the full audit record.
- **Event type registry** — next free types are `8` and `9` (1=Access,2=PathMap,3=Memory,4=Forget,5=Message,6=Recall,7=CommsClear). Use `EventImage = 8`, `EventBlobPin = 9`.
- **Tenant isolation** — every read/get/pin is scoped to the caller's tenant; blobs are global but reachable only via a tenant-owned reference.
- **Size/format limits** — reject images `> 15<<20` bytes or mime not `image/*`.

---

### Task 1: `blob` package — content-addressed byte store

**Files:**
- Create: `blob/blob.go`
- Test: `blob/blob_test.go`

**Interfaces:**
- Consumes: stdlib only (`crypto/sha256`, `encoding/hex`, `os`, `path/filepath`, `sync`, `errors`).
- Produces:
  - `func Open(root string) (*Store, error)` — manages `<root>/blobs`.
  - `func (s *Store) Put(data []byte) (sha string, err error)`
  - `func (s *Store) Get(sha string) ([]byte, error)` — returns `ErrNotFound` if absent/invalid.
  - `func (s *Store) Delete(sha string) error` — idempotent.
  - `func (s *Store) List() ([]string, error)`
  - `func (s *Store) Sweep(keep map[string]bool) (deleted int, err error)`
  - `var ErrNotFound error`

- [ ] **Step 1: Write the failing test**

Create `blob/blob_test.go`:

```go
package blob

import (
	"bytes"
	"testing"
)

func TestPutGetDedupAndSweep(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	data := []byte("fake png bytes")
	sha1, err := s.Put(data)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(sha1) != 64 {
		t.Fatalf("sha must be 64 hex chars, got %q", sha1)
	}

	// Dedup: same bytes → same sha, written once.
	sha2, err := s.Put(data)
	if err != nil || sha2 != sha1 {
		t.Fatalf("dedup: got %q,%v want %q", sha2, err, sha1)
	}
	if list, _ := s.List(); len(list) != 1 {
		t.Fatalf("expected 1 blob after dedup, got %d", len(list))
	}

	// Round-trip.
	got, err := s.Get(sha1)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get round-trip: got %q,%v", got, err)
	}

	// Missing / invalid sha → ErrNotFound.
	if _, err := s.Get("deadbeef"); err != ErrNotFound {
		t.Fatalf("Get(invalid) = %v, want ErrNotFound", err)
	}

	// Sweep keeps the keep-set, deletes the rest.
	other, _ := s.Put([]byte("second blob"))
	n, err := s.Sweep(map[string]bool{sha1: true})
	if err != nil || n != 1 {
		t.Fatalf("Sweep deleted %d,%v want 1", n, err)
	}
	if _, err := s.Get(other); err != ErrNotFound {
		t.Fatal("swept blob should be gone")
	}
	if _, err := s.Get(sha1); err != nil {
		t.Fatal("kept blob should survive sweep")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./blob/ -run TestPutGetDedupAndSweep -v`
Expected: FAIL — build error, `Open`/`Store`/`ErrNotFound` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `blob/blob.go`:

```go
// Package blob is a content-addressed byte store on disk: each blob is keyed by
// the hex sha256 of its contents, written once (dedup), under <root>/blobs. It
// has no knowledge of tenants or the journal — callers gate access via a
// tenant-scoped reference. Used by Matrix Sentry to hold agent-shared images
// out of the in-RAM comms index and out of the 16 MiB-capped journal records.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// ErrNotFound is returned by Get when a blob is absent or the sha is malformed.
var ErrNotFound = errors.New("blob: not found")

type Store struct {
	dir string // <root>/blobs
	mu  sync.Mutex
}

// Open prepares <root>/blobs (creating it if needed) and returns the store.
func Open(root string) (*Store, error) {
	dir := filepath.Join(root, "blobs")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func validSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Put stores data and returns its hex sha256. If a blob with that sha already
// exists it is not rewritten (write-once dedup). Atomic via temp-file + rename.
func (s *Store) Put(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	path := filepath.Join(s.dir, sha)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(path); err == nil {
		return sha, nil // already stored
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return sha, nil
}

// Get returns the bytes for sha, or ErrNotFound if absent/malformed.
func (s *Store) Get(sha string) ([]byte, error) {
	if !validSHA(sha) {
		return nil, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(s.dir, sha))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

// Delete removes sha if present; absent/malformed is a no-op (idempotent).
func (s *Store) Delete(sha string) error {
	if !validSHA(sha) {
		return nil
	}
	err := os.Remove(filepath.Join(s.dir, sha))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// List returns every stored blob sha (skips in-progress temp files).
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if validSHA(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// Sweep deletes every stored blob whose sha is NOT in keep, returning the count
// deleted. This is the GC primitive; the keep-set comes from comms.LiveBlobIDs.
func (s *Store) Sweep(keep map[string]bool) (int, error) {
	shas, err := s.List()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, sha := range shas {
		if !keep[sha] {
			if err := s.Delete(sha); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./blob/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add blob/blob.go blob/blob_test.go
git commit -m "feat(blob): content-addressed byte store (dedup, sweep) for image transfer"
```

---

### Task 2: comms — image-reference messages

**Files:**
- Modify: `comms/comms.go` (constants, `MessagePayload`, `Message`, `message()`, `New()`, add `PostImage`, `GetBySeq`)
- Test: `comms/comms_test.go` (append)

**Interfaces:**
- Consumes: `sentry.Store`, `sentry.EventType`, existing `EventMessage`/`EventCommsClear`.
- Produces:
  - `const EventImage sentry.EventType = 8`
  - `MessagePayload`/`Message` gain `BlobID, Mime string; W, H, Size int`.
  - `func (s *Store) PostImage(tenant sentry.TenantID, p MessagePayload) (uint64, error)`
  - `func (s *Store) GetBySeq(tenant sentry.TenantID, seq uint64) (Message, bool)`

- [ ] **Step 1: Write the failing test**

Append to `comms/comms_test.go`:

```go
func TestPostImageAndGetBySeq(t *testing.T) {
	st := newTestStore(t) // existing helper that wraps a temp journal in comms.New
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
```

> If `newTestStore` does not exist, check `comms_test.go` for the existing setup helper (the retention/clear tests build a `*Store`); reuse that exact constructor instead.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./comms/ -run TestPostImageAndGetBySeq -v`
Expected: FAIL — `PostImage`/`GetBySeq` undefined; `MessagePayload` has no `BlobID`.

- [ ] **Step 3: Write minimal implementation**

In `comms/comms.go`, add the constant next to `EventCommsClear`:

```go
// EventImage is the journal record type for an image-reference message: a comms
// message that carries a blob ref (sha + mime + dims) instead of body text. The
// bytes live in the blob store; only this small ref rides the in-RAM index.
const EventImage sentry.EventType = 8
```

Extend `MessagePayload` and `Message` with the image fields (add to both structs):

```go
	// Image reference (set only for image messages; BlobID != "" marks an image):
	BlobID string `json:"blob,omitempty"`
	Mime   string `json:"mime,omitempty"`
	W      int    `json:"w,omitempty"`
	H      int    `json:"h,omitempty"`
	Size   int    `json:"size,omitempty"`
```

Update the `message()` helper to copy them:

```go
func message(seq uint64, tenant sentry.TenantID, ts int64, p MessagePayload) Message {
	return Message{
		Seq: seq, Tenant: tenant, TS: ts,
		Area: p.Area, From: p.From, Kind: p.Kind, Text: p.Text, Target: p.Target, Ref: p.Ref,
		BlobID: p.BlobID, Mime: p.Mime, W: p.W, H: p.H, Size: p.Size,
	}
}
```

In `New()`, after the existing `EventMessage` scan (pass 1) add an `EventImage` scan, then sort `entries` by seq so reads stay seq-ascending. Add `"sort"` to the imports. Insert this immediately after pass 1 populates `s.entries` and before the clear pass:

```go
	// Pass 1b: image-reference messages share the message index.
	itype := EventImage
	if err := journal.Scan(sentry.Filter{Type: &itype}, func(r sentry.Record) bool {
		var p MessagePayload
		if err := sentry.UnmarshalPayload(r.Payload, &p); err != nil {
			scanErr = fmt.Errorf("comms: decode image seq %d: %w", r.Seq, err)
			return false
		}
		s.entries = append(s.entries, message(uint64(r.Seq), r.Tenant, r.Tstamp, p))
		return true
	}); err != nil {
		return nil, err
	}
	if scanErr != nil {
		return nil, scanErr
	}
	// Messages and images interleave in the journal but are scanned separately;
	// re-sort by seq so Read/Inbox/Recent cursor logic stays correct.
	sort.Slice(s.entries, func(i, j int) bool { return s.entries[i].Seq < s.entries[j].Seq })
```

Add `PostImage` and `GetBySeq` (place after `Post`):

```go
// PostImage appends an image-reference message to area for tenant and returns
// its journal seq. Area, From, BlobID and Mime are required; Kind is forced to
// "image". The bytes themselves live in the blob store, keyed by BlobID.
func (s *Store) PostImage(tenant sentry.TenantID, p MessagePayload) (uint64, error) {
	if p.Area == "" || p.From == "" || p.BlobID == "" || p.Mime == "" {
		return 0, fmt.Errorf("comms: area, from, blob and mime are required for an image")
	}
	p.Kind = "image"
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, err := s.journal.Append(tenant, EventImage, p)
	if err != nil {
		return 0, fmt.Errorf("comms: append image: %w", err)
	}
	s.entries = append(s.entries, message(uint64(seq), tenant, time.Now().UnixNano(), p))
	s.pruneAt(time.Now().UnixNano())
	return uint64(seq), nil
}

// GetBySeq returns tenant's message at seq regardless of area (image tools have
// a seq, not an area). Tenant-scoped: another tenant's seq returns false.
func (s *Store) GetBySeq(tenant sentry.TenantID, seq uint64) (Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.entries {
		if m.Tenant == tenant && m.Seq == seq {
			return m, true
		}
	}
	return Message{}, false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./comms/ -run TestPostImageAndGetBySeq -v && go test ./comms/`
Expected: PASS (new test) and the full comms suite still green.

- [ ] **Step 5: Commit**

```bash
git add comms/comms.go comms/comms_test.go
git commit -m "feat(comms): image-reference messages (EventImage) + GetBySeq"
```

---

### Task 3: comms — pin + GC keep-set

**Files:**
- Modify: `comms/comms.go` (`Store.pinned`, `EventBlobPin`, `PinPayload`, `Pin`, `New()` pin pass, `LiveBlobIDs`)
- Test: `comms/comms_test.go` (append)

**Interfaces:**
- Consumes: Task 2's image messages, `sentry.Store`.
- Produces:
  - `const EventBlobPin sentry.EventType = 9`
  - `type PinPayload struct { BlobID string; On bool }`
  - `func (s *Store) Pin(tenant sentry.TenantID, blobID string, on bool) error`
  - `func (s *Store) LiveBlobIDs() map[string]bool` — union of live image refs + pinned sha.

- [ ] **Step 1: Write the failing test**

Append to `comms/comms_test.go`:

```go
func TestLiveBlobIDsAndPin(t *testing.T) {
	st := newTestStore(t)
	st.PostImage(1, MessagePayload{Area: "ui", From: "d", Mime: "image/png", BlobID: "live1"})

	keep := st.LiveBlobIDs()
	if !keep["live1"] {
		t.Fatal("a referenced blob must be in the keep-set")
	}

	// A pinned blob with no live message must still be kept.
	if err := st.Pin(1, "pinned1", true); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if !st.LiveBlobIDs()["pinned1"] {
		t.Fatal("a pinned blob must be in the keep-set")
	}

	// Unpin removes it from the keep-set.
	if err := st.Pin(1, "pinned1", false); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	if st.LiveBlobIDs()["pinned1"] {
		t.Fatal("an unpinned, unreferenced blob must NOT be kept")
	}

	// comms_clear drops the ref → blob leaves the keep-set.
	st.Clear(1, "ui")
	if st.LiveBlobIDs()["live1"] {
		t.Fatal("cleared image ref must leave the keep-set")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./comms/ -run TestLiveBlobIDsAndPin -v`
Expected: FAIL — `Pin`/`LiveBlobIDs` undefined.

- [ ] **Step 3: Write minimal implementation**

Add the `pinned` field to `Store`:

```go
	pinned map[string]bool // blob sha kept from GC regardless of message retention
```

Add the constant + payload next to `EventImage`:

```go
// EventBlobPin records a pin (On=true) or unpin (On=false) of a blob sha, so a
// blob survives GC independently of whether a live message still references it.
const EventBlobPin sentry.EventType = 9

// PinPayload is the persisted pin/unpin of a blob.
type PinPayload struct {
	BlobID string `json:"blob"`
	On     bool   `json:"on"`
}
```

In `New()`, after the clear pass and before the final `return s, nil` (note: `New()` does NOT call prune — retention is applied later via `SetRetention` in `main`), replay pins:

```go
	// Pass 3: replay blob pins (seq order → final on/off state wins).
	s.pinned = map[string]bool{}
	ptype := EventBlobPin
	if err := journal.Scan(sentry.Filter{Type: &ptype}, func(r sentry.Record) bool {
		var pp PinPayload
		if err := sentry.UnmarshalPayload(r.Payload, &pp); err != nil {
			scanErr = fmt.Errorf("comms: decode pin seq %d: %w", r.Seq, err)
			return false
		}
		if pp.On {
			s.pinned[pp.BlobID] = true
		} else {
			delete(s.pinned, pp.BlobID)
		}
		return true
	}); err != nil {
		return nil, err
	}
	if scanErr != nil {
		return nil, scanErr
	}
```

> Note: if `New()` returns before any `prune()` call, ensure `s.pinned` is initialized before the existing `return s, nil`. The block above does that.

Add `Pin` and `LiveBlobIDs`:

```go
// Pin records a pin (on=true) or unpin (on=false) for a blob sha and updates the
// in-RAM pinned set. Pins are global (a blob is content-addressed and shared);
// tenant is recorded for audit only.
func (s *Store) Pin(tenant sentry.TenantID, blobID string, on bool) error {
	if blobID == "" {
		return fmt.Errorf("comms: blob id required to pin")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.journal.Append(tenant, EventBlobPin, PinPayload{BlobID: blobID, On: on}); err != nil {
		return fmt.Errorf("comms: append pin: %w", err)
	}
	if s.pinned == nil {
		s.pinned = map[string]bool{}
	}
	if on {
		s.pinned[blobID] = true
	} else {
		delete(s.pinned, blobID)
	}
	return nil
}

// LiveBlobIDs returns the GC keep-set: every blob referenced by a live image
// message (across ALL tenants — blobs are shared) plus every pinned sha.
func (s *Store) LiveBlobIDs() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := map[string]bool{}
	for _, m := range s.entries {
		if m.BlobID != "" {
			keep[m.BlobID] = true
		}
	}
	for sha := range s.pinned {
		keep[sha] = true
	}
	return keep
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./comms/ -run TestLiveBlobIDsAndPin -v && go test ./comms/`
Expected: PASS and full comms suite green.

- [ ] **Step 5: Commit**

```bash
git add comms/comms.go comms/comms_test.go
git commit -m "feat(comms): blob pin (EventBlobPin) + LiveBlobIDs GC keep-set"
```

---

### Task 4: sentrymcp — blob wiring + `post_image`/`get_image` tools

**Files:**
- Modify: `cmd/sentrymcp/main.go` (imports, `server.blobs` field, `main` wiring + startup GC, tool registry entries, dispatch cases, `toolImage` + `imageDims` helpers)
- Test: `cmd/sentrymcp/main_test.go` (append)

**Interfaces:**
- Consumes: Task 1 `blob.Open/Put/Get/Sweep`, Task 2 `comms.PostImage/GetBySeq`, Task 3 `comms.LiveBlobIDs`; existing `strArg`/`uintArg`/`s.ok`/`s.toolText`/`s.toolErr`, dispatch `switch p.Name` (main.go:642), tool registry list (main.go:563+).
- Produces: tools `post_image`, `get_image`; `func (s *server) toolImage(id json.RawMessage, b64, mime, caption string) rpcResp`; `func imageDims(data []byte) (w, h int)`; `s.blobs *blob.Store`.

- [ ] **Step 1: Write the failing test**

Append to `cmd/sentrymcp/main_test.go` (mirror the existing tool-dispatch test setup in that file — find how other tests build a `*server` with a temp journal + `s.chat`, and add `s.blobs` via `blob.Open(tmpDir)`):

```go
func TestPostImageGetImageRoundTrip(t *testing.T) {
	s := newTestServer(t) // existing helper; ensure it sets s.blobs = blob.Open(dir)
	raw := []byte("\x89PNG\r\n\x1a\n fake")
	b64 := base64.StdEncoding.EncodeToString(raw)

	// post_image
	post := s.dispatchTool(t, "post_image", map[string]any{
		"area": "ui", "from": "designer", "mime": "image/png",
		"data": b64, "caption": "mock",
	})
	seq := extractSeq(t, post) // helper: parse "#N" out of the text content

	// get_image returns MCP image content with identical bytes.
	got := s.dispatchTool(t, "get_image", map[string]any{"seq": seq})
	img := firstImageContent(t, got) // helper: find {type:"image"} in content
	if img["mimeType"] != "image/png" {
		t.Fatalf("mime = %v", img["mimeType"])
	}
	decoded, _ := base64.StdEncoding.DecodeString(img["data"].(string))
	if !bytes.Equal(decoded, raw) {
		t.Fatal("round-trip bytes differ")
	}

	// Oversize rejected.
	big := base64.StdEncoding.EncodeToString(make([]byte, (15<<20)+1))
	if !isToolError(s.dispatchTool(t, "post_image", map[string]any{
		"area": "ui", "from": "d", "mime": "image/png", "data": big,
	})) {
		t.Fatal("oversize image must be rejected")
	}

	// Non-image mime rejected.
	if !isToolError(s.dispatchTool(t, "post_image", map[string]any{
		"area": "ui", "from": "d", "mime": "application/pdf", "data": b64,
	})) {
		t.Fatal("non-image mime must be rejected")
	}
}
```

> Adapt `dispatchTool`/`extractSeq`/`firstImageContent`/`isToolError` to the test helpers that already exist in `main_test.go` (the comms_clear / post tests show how a tool call is invoked and its `rpcResp` inspected). If no such helper exists, call the same dispatch entrypoint those tests use and assert on the returned `rpcResp.Result` map shape.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/sentrymcp/ -run TestPostImageGetImageRoundTrip -v`
Expected: FAIL — `post_image` not a known tool / `s.blobs` nil.

- [ ] **Step 3: Write minimal implementation**

Add imports to `main.go` (with the existing import block):

```go
	"bytes"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"matrixsentry/blob"
```

Add the field to `server` (main.go:76 area):

```go
	blobs     *blob.Store      // content-addressed image bytes (comms image transfer)
```

Wire the blob store in `main`, right after `reg` is built and before/after `s` is constructed — open it on `*dir` and assign to `s.blobs`. Insert immediately after `s.chat.SetRetention(...)` (main.go:122):

```go
	s.blobs, err = blob.Open(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sentrymcp: init blob store: %v\n", err)
		os.Exit(1)
	}
	// Startup GC: drop orphan blob files no live message references and that
	// aren't pinned. The journal is untouched.
	if n, err := s.blobs.Sweep(s.chat.LiveBlobIDs()); err != nil {
		moko.Warn("blob gc at startup failed", map[string]string{"err": err.Error()})
	} else if n > 0 {
		moko.Info("blob gc at startup", map[string]string{"deleted": fmt.Sprint(n)})
	}
```

> If `mokoblinks.Client` has no `Warn`, use `moko.Info` with an `"err"` field, matching how errors are surfaced elsewhere in `main.go`.

Add the two tool registry entries inside the tools list (after the `inbox` entry, main.go:612):

```go
		{
			"name":        "post_image",
			"description": "Share an image with other agents on a channel ('area'). Pass the image as base64 in 'data' with its 'mime' (image/*). The bytes are stored server-side; other agents call get_image with the returned # to fetch it. Use 'caption' for a text description, 'target' to direct it at one agent. Max 15 MB.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"area":    map[string]any{"type": "string", "description": "channel name"},
					"from":    map[string]any{"type": "string", "description": "your agent label"},
					"data":    map[string]any{"type": "string", "description": "the image bytes, base64-encoded"},
					"mime":    map[string]any{"type": "string", "description": "image mime type, e.g. image/png, image/jpeg, image/webp"},
					"caption": map[string]any{"type": "string", "description": "optional text describing the image"},
					"target":  map[string]any{"type": "string", "description": "optional agent label to direct this at; empty = broadcast"},
					"w":       map[string]any{"type": "integer", "description": "optional width in px (auto-detected for png/jpeg/gif if omitted)"},
					"h":       map[string]any{"type": "integer", "description": "optional height in px (auto-detected for png/jpeg/gif if omitted)"},
				},
				"required": []any{"area", "from", "data", "mime"},
			},
		},
		{
			"name":        "get_image",
			"description": "Fetch an image posted to a channel by its message # (as shown by read/inbox/post_image). Returns the image itself so you can view it. Tenant-scoped.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"seq": map[string]any{"type": "integer", "description": "the image message #"}},
				"required":   []any{"seq"},
			},
		},
```

Add the dispatch cases inside `switch p.Name` (after the `inbox` case, main.go:912 area). The `tenant` variable is already in scope in this switch (as used by `post`):

```go
	case "post_image":
		area, _ := strArg(p.Args, "area")
		from, _ := strArg(p.Args, "from")
		mime, _ := strArg(p.Args, "mime")
		dataB64, _ := strArg(p.Args, "data")
		if area == "" || from == "" || mime == "" || dataB64 == "" {
			return s.toolErr(req.ID, "provide 'area', 'from', 'mime' and base64 'data'")
		}
		if !strings.HasPrefix(mime, "image/") {
			return s.toolErr(req.ID, "mime must be image/* (got "+mime+")")
		}
		raw, err := base64.StdEncoding.DecodeString(dataB64)
		if err != nil {
			return s.toolErr(req.ID, "data is not valid base64: "+err.Error())
		}
		if len(raw) > 15<<20 {
			return s.toolErr(req.ID, fmt.Sprintf("image too large: %d bytes (max %d)", len(raw), 15<<20))
		}
		sha, err := s.blobs.Put(raw)
		if err != nil {
			return s.toolErr(req.ID, "blob store failed: "+err.Error())
		}
		w, h := int(uintArg(p.Args, "w")), int(uintArg(p.Args, "h"))
		if w == 0 && h == 0 {
			w, h = imageDims(raw)
		}
		caption, _ := strArg(p.Args, "caption")
		target, _ := strArg(p.Args, "target")
		s.mu.Lock()
		seq, err := s.chat.PostImage(tenant, comms.MessagePayload{
			Area: area, From: from, Mime: mime, BlobID: sha,
			W: w, H: h, Size: len(raw), Text: caption, Target: target,
		})
		s.mu.Unlock()
		if err != nil {
			return s.toolErr(req.ID, "post_image failed: "+err.Error())
		}
		s.moko.Info("post_image", map[string]string{"tenant": fmt.Sprint(tenant), "area": area, "seq": fmt.Sprint(seq), "bytes": fmt.Sprint(len(raw))})
		return s.toolText(req.ID, fmt.Sprintf("posted image #%d in %s (%s, %dx%d, %d bytes) — fetch with get_image(%d)", seq, area, mime, w, h, len(raw), seq))
	case "get_image":
		seq := uintArg(p.Args, "seq")
		if seq == 0 {
			return s.toolErr(req.ID, "provide the 'seq' of the image message")
		}
		m, ok := s.chat.GetBySeq(tenant, seq)
		if !ok {
			return s.toolErr(req.ID, fmt.Sprintf("message #%d not found for this tenant", seq))
		}
		if m.BlobID == "" {
			return s.toolErr(req.ID, fmt.Sprintf("message #%d is not an image", seq))
		}
		raw, err := s.blobs.Get(m.BlobID)
		if err != nil {
			return s.toolErr(req.ID, "image bytes unavailable (blob "+m.BlobID+"): "+err.Error())
		}
		return s.toolImage(req.ID, base64.StdEncoding.EncodeToString(raw), m.Mime, m.Text)
```

Add the helpers near `toolText`/`toolErr` (main.go:1093 area):

```go
// toolImage returns an MCP image content block (base64 + mimeType), optionally
// preceded by a text caption — the protocol-native way to hand an image to any
// MCP client, local or remote.
func (s *server) toolImage(id json.RawMessage, b64, mime, caption string) rpcResp {
	content := []map[string]any{}
	if caption != "" {
		content = append(content, map[string]any{"type": "text", "text": caption})
	}
	content = append(content, map[string]any{"type": "image", "data": b64, "mimeType": mime})
	return s.ok(id, map[string]any{"content": content})
}

// imageDims best-effort decodes width/height using only stdlib decoders
// (png/jpeg/gif). Unknown formats (e.g. webp) return 0,0 — no extra deps.
func imageDims(data []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/sentrymcp/ -run TestPostImageGetImageRoundTrip -v && go vet ./cmd/sentrymcp/ && go test ./cmd/sentrymcp/`
Expected: PASS, vet clean, full suite green.

- [ ] **Step 5: Commit**

```bash
git add cmd/sentrymcp/main.go cmd/sentrymcp/main_test.go
git commit -m "feat(sentrymcp): post_image/get_image tools + blob store wiring + startup GC"
```

---

### Task 5: sentrymcp — `pin_image`/`unpin_image`/`blob_gc` + image markers in read/inbox

**Files:**
- Modify: `cmd/sentrymcp/main.go` (3 tool registry entries, 3 dispatch cases, image-aware lines in `read`/`inbox` output)
- Test: `cmd/sentrymcp/main_test.go` (append)

**Interfaces:**
- Consumes: Task 3 `comms.Pin/LiveBlobIDs`, Task 4 `s.blobs`, `comms.GetBySeq`.
- Produces: tools `pin_image`, `unpin_image`, `blob_gc`.

- [ ] **Step 1: Write the failing test**

Append to `cmd/sentrymcp/main_test.go`:

```go
func TestPinSurvivesBlobGC(t *testing.T) {
	s := newTestServer(t)
	raw := []byte("\x89PNG\r\n\x1a\n keep me")
	b64 := base64.StdEncoding.EncodeToString(raw)
	post := s.dispatchTool(t, "post_image", map[string]any{
		"area": "ui", "from": "d", "mime": "image/png", "data": b64,
	})
	seq := extractSeq(t, post)

	// Pin, then clear the area (drops the live ref), then GC.
	s.dispatchTool(t, "pin_image", map[string]any{"seq": seq})
	s.dispatchTool(t, "comms_clear", map[string]any{"area": "ui"})
	gc := s.dispatchTool(t, "blob_gc", map[string]any{})
	if isToolError(gc) {
		t.Fatalf("blob_gc errored: %v", gc)
	}

	// Pinned blob must still be fetchable even though no live message refs it.
	// (get_image needs a live message; assert the blob file survived via a fresh
	// LiveBlobIDs/Sweep check instead.)
	if !s.chat.LiveBlobIDs()[blobOf(t, s, raw)] {
		t.Fatal("pinned blob must remain in keep-set after clear")
	}

	// Unpin → now GC removes it.
	s.dispatchTool(t, "unpin_image", map[string]any{"seq": seq}) // seq still resolvable? see note
}
```

> Resolution note: after `comms_clear`, `GetBySeq(seq)` returns false (the ref left the index), so `unpin_image` by seq can no longer resolve the blob. That is acceptable — pin/unpin operate while the message is live. For the test, assert the keep-set behavior via `s.chat.LiveBlobIDs()` directly (helper `blobOf` recomputes `sha256(raw)` hex). Drop the final unpin-by-seq line if it cannot resolve; instead assert `blob_gc` deleted 0 while pinned. Keep the test focused on: **pin keeps the blob through clear+GC**.

Simplified assertion to implement:

```go
	// After pin + clear, blob_gc must delete 0 (pin protects it).
	if got := gcDeleted(t, gc); got != 0 {
		t.Fatalf("blob_gc deleted %d, want 0 (pinned)", got)
	}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/sentrymcp/ -run TestPinSurvivesBlobGC -v`
Expected: FAIL — `pin_image`/`blob_gc` unknown tools.

- [ ] **Step 3: Write minimal implementation**

Add three registry entries (after `get_image`):

```go
		{
			"name":        "pin_image",
			"description": "Pin an image so it survives channel retention and comms_clear (the blob is never GC'd while pinned). Pass the image message #.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"seq": map[string]any{"type": "integer", "description": "the image message # to pin"}},
				"required":   []any{"seq"},
			},
		},
		{
			"name":        "unpin_image",
			"description": "Remove a pin from an image so it can be garbage-collected once no live message references it. Pass the image message #.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"seq": map[string]any{"type": "integer", "description": "the image message # to unpin"}},
				"required":   []any{"seq"},
			},
		},
		{
			"name":        "blob_gc",
			"description": "Delete orphan image blobs no live message references and that aren't pinned (the journal is retained). Owner/orchestrator housekeeping.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
```

Add dispatch cases (after `get_image`):

```go
	case "pin_image", "unpin_image":
		seq := uintArg(p.Args, "seq")
		if seq == 0 {
			return s.toolErr(req.ID, "provide the 'seq' of the image")
		}
		m, ok := s.chat.GetBySeq(tenant, seq)
		if !ok || m.BlobID == "" {
			return s.toolErr(req.ID, fmt.Sprintf("image #%d not found for this tenant", seq))
		}
		on := p.Name == "pin_image"
		if err := s.chat.Pin(tenant, m.BlobID, on); err != nil {
			return s.toolErr(req.ID, "pin failed: "+err.Error())
		}
		verb := "pinned"
		if !on {
			verb = "unpinned"
		}
		return s.toolText(req.ID, fmt.Sprintf("%s image #%d", verb, seq))
	case "blob_gc":
		n, err := s.blobs.Sweep(s.chat.LiveBlobIDs())
		if err != nil {
			return s.toolErr(req.ID, "blob_gc failed: "+err.Error())
		}
		s.moko.Info("blob_gc", map[string]string{"tenant": fmt.Sprint(tenant), "deleted": fmt.Sprint(n)})
		return s.toolText(req.ID, fmt.Sprintf("deleted %d orphan blob(s) (journal retained)", n))
```

Make `read` and `inbox` show image messages clearly. In the `read` loop (main.go:863) replace the single `fmt.Fprintf` with:

```go
			if m.BlobID != "" {
				fmt.Fprintf(&b, "#%d [image] %s→%s: %s [%s %dx%d %dB · get_image(%d)]\n", m.Seq, m.From, to, m.Text, m.Mime, m.W, m.H, m.Size, m.Seq)
			} else {
				fmt.Fprintf(&b, "#%d [%s] %s→%s: %s\n", m.Seq, m.Kind, m.From, to, m.Text)
			}
```

In the `inbox` loop (main.go:905) replace its `fmt.Fprintf` with:

```go
				if m.BlobID != "" {
					fmt.Fprintf(&b, "#%d [image] %s→%s @%s: %s [%s %dx%d · get_image(%d)]\n", m.Seq, m.From, tgt, m.Area, m.Text, m.Mime, m.W, m.H, m.Seq)
				} else {
					fmt.Fprintf(&b, "#%d [%s] %s→%s @%s: %s\n", m.Seq, m.Kind, m.From, tgt, m.Area, m.Text)
				}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/sentrymcp/ -run TestPinSurvivesBlobGC -v && go test ./... && go vet ./...`
Expected: PASS; whole-module tests green; vet clean.

- [ ] **Step 5: Commit**

```bash
git add cmd/sentrymcp/main.go cmd/sentrymcp/main_test.go
git commit -m "feat(sentrymcp): pin_image/unpin_image/blob_gc + image markers in read/inbox"
```

---

### Task 6: Build, deploy to 8808, verify (ops — no TDD)

**Files:** none (deployment).

- [ ] **Step 1: Cross-compile for linux/amd64**

```bash
GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/sentrymcp.linux-amd64 ./cmd/sentrymcp
file /tmp/sentrymcp.linux-amd64   # expect: ELF 64-bit ... x86-64
```

- [ ] **Step 2: Ship + checksum-verify to the VM**

```bash
scp /tmp/sentrymcp.linux-amd64 matrix-sentry:/root/sentrymcp.new
ssh matrix-sentry 'sha256sum /root/sentrymcp.new'
shasum -a 256 /tmp/sentrymcp.linux-amd64   # must match
```

- [ ] **Step 3: Swap binary + restart (keep backup)**

```bash
ssh matrix-sentry '
  cp -f /root/sentrymcp /root/sentrymcp.prev &&
  mv /root/sentrymcp.new /root/sentrymcp && chmod +x /root/sentrymcp &&
  systemctl restart sentrymcp.service && sleep 1 &&
  echo "active=$(systemctl is-active sentrymcp.service) pid=$(systemctl show -p MainPID --value sentrymcp.service)"'
```
Expected: `active=active pid=<new>`.

- [ ] **Step 4: Verify the new tools are advertised**

```bash
ssh matrix-sentry 'curl -s -X POST http://127.0.0.1:8808/mcp \
  -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" \
  -H "Authorization: Bearer $(grep ^SENTRY_MCP_TOKEN= /root/sentrymcp.env | cut -d= -f2-)" \
  -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}" | grep -o "post_image\|get_image\|pin_image\|blob_gc"'
```
Expected: the four tool names appear.

- [ ] **Step 5: End-to-end round-trip via the MCP tool**

From this session, call `post_image` (a small base64 PNG to a test area) then `get_image` with the returned seq and confirm an image content block returns. Then `blob_gc` and confirm it does not delete the just-posted (live) blob. Finally `comms_clear` the test area + `blob_gc` to confirm the orphan is swept.

- [ ] **Step 6: Note** — the teams server (`matrix.blazesphere.net` / 8809, separate host) is NOT covered here. Deploy there separately if image transfer is wanted for team tenants.
