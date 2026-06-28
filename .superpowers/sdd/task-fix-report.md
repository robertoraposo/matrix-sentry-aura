# Fix Report — comms-image-transfer branch review

## Fix 1: TOCTOU race — blob_gc vs in-flight post_image

**Files changed:** `cmd/sentrymcp/main.go`

### What changed

**`post_image` case (line ~1003–1035):**
- Moved `s.blobs.Put(raw)` from outside the lock to **inside** `s.mu.Lock()/Unlock()`, immediately before `s.chat.PostImage(...)`.
- The base64 decode, size/mime checks, `imageDims`, and arg extraction remain **before** the lock (no journal I/O there).
- If `Put` fails, `s.mu.Unlock()` is called before the early return to avoid a lock leak.

**`blob_gc` case (line ~1081–1090):**
- Wrapped the entire `LiveBlobIDs()` + `Sweep()` pair in `s.mu.Lock()/Unlock()`.

### Lock-order confirmation

There are three independent mutexes:

| Mutex | Owner | Acquired by |
|-------|-------|-------------|
| `server.mu` (`sync.Mutex`) | `server` struct | callers in `callTool` |
| `comms.Store.mu` (`sync.Mutex`) | `comms.Store` | methods on `comms.Store` |
| `blob.Store.mu` (`sync.Mutex`) | `blob.Store` | methods on `blob.Store` |

**post_image** under `s.mu`:
1. `s.blobs.Put(raw)` → acquires + releases `blob.mu` internally (sequential, not nested)
2. `s.chat.PostImage(...)` → acquires + releases `comms.mu` internally (sequential, not nested)

**blob_gc** under `s.mu`:
1. `s.chat.LiveBlobIDs()` → acquires + releases `comms.mu` internally
2. `s.blobs.Sweep(...)` → acquires + releases `blob.mu` internally

In both paths `blob.mu` and `comms.mu` are held for their own method call only (never nested under `s.mu` while `s.mu` is still held at the same frame). There is **no nested lock-order inversion** — deadlock is not possible.

Startup GC in `main()` is single-threaded before any listener is started; left as-is per spec.

---

## Fix 2: Cross-tenant unpin — pins are now per-(tenant, sha)

**Files changed:** `comms/comms.go`, `comms/comms_test.go`

### What changed in comms.go

**Field** (line ~88):
```go
// before
pinned map[string]bool
// after
pinned map[string]map[sentry.TenantID]bool  // outer=sha, inner=set of pinning tenants
```

**`New()` pin-replay pass** (Pass 3, line ~166):
- Initialises `s.pinned = map[string]map[sentry.TenantID]bool{}`
- On `pp.On = true`: ensures inner map exists, sets `s.pinned[pp.BlobID][r.Tenant] = true` using `r.Tenant` (the journal record's tenant, authoritative).
- On `pp.On = false`: `delete(s.pinned[pp.BlobID], r.Tenant)`; cleans up the outer key when the inner map becomes empty.

**`Pin()` method** (line ~370):
- Same per-tenant logic as replay; nil-guard initialises `map[string]map[sentry.TenantID]bool{}`.

**`LiveBlobIDs()`** (line ~392):
- `for sha, tenants := range s.pinned { if len(tenants) > 0 { keep[sha] = true } }`
- A blob is kept if any tenant has an active pin.

### Tests

`TestLiveBlobIDsAndPin` passes unchanged (single-tenant, same observable behaviour).

`TestPinIsPerTenant` (new, added to `comms/comms_test.go`):
1. Tenant 1 pins `shaX` → `shaX` in keep-set ✓
2. Tenant 2 unpins `shaX` → `shaX` **still** in keep-set (tenant 1's pin survives) ✓
3. Tenant 1 also unpins → `shaX` **leaves** keep-set ✓

```
=== RUN   TestLiveBlobIDsAndPin
--- PASS: TestLiveBlobIDsAndPin (0.02s)
=== RUN   TestPinIsPerTenant
--- PASS: TestPinIsPerTenant (0.01s)
PASS
ok  matrixsentry/comms  0.475s
```

---

## Fix 3: blob_gc owner-gate

**Files changed:** `cmd/sentrymcp/main.go`

### What changed

Added at the top of the `blob_gc` case (line ~1082):
```go
if tenant != s.tenant {
    return s.toolErr(req.ID, "blob_gc is restricted to the owner tenant")
}
```

### Owner signal confirmation

`s.tenant` is set from the `-tenant` flag (default `1`) in `main()`:
```go
s := &server{..., tenant: sentry.TenantID(*tenant), ...}
```
It represents the operator's configured tenant — the same one that owns the SENTRY_MCP_TOKEN. Multi-tenant callers are mapped by `s.tokens` (team tokens); the `-tenant` flag identity is the right "owner" notion. The tool remains registered in `toolList()` for all; the gate is enforced at dispatch.

`TestPinSurvivesBlobGC` uses `callNamed(s, "blob_gc", ...)` which passes `s.tenant` (1) as tenant — the owner — so the gate allows it. Test still passes.

---

## Fix 4: Base64 pre-check (DoS vector)

**Files changed:** `cmd/sentrymcp/main.go`

### What changed

Inserted before `base64.StdEncoding.DecodeString(dataB64)` (line ~1010):
```go
if len(dataB64) > ((15<<20)+2)/3*4+16 {
    return s.toolErr(req.ID, "image too large")
}
```

Formula: `((15<<20)+2)/3*4+16 = 20971536` bytes. A legitimately-encoded 15 MiB payload encodes to exactly 20971520 bytes; the +16 bytes of slack absorbs padding variation. An oversized base64 string is rejected before `DecodeString` allocates any heap. The existing post-decode `len(raw) > 15<<20` check is retained for defense in depth.

The test's oversize case (`make([]byte, (15<<20)+1)`) encodes to 20971524 bytes, which is below 20971536, so it correctly falls through to the post-decode check — no test change needed.

---

## Full test output

```
$ go test ./comms/ -run 'TestLiveBlobIDsAndPin|TestPinIsPerTenant' -v
=== RUN   TestLiveBlobIDsAndPin
--- PASS: TestLiveBlobIDsAndPin (0.02s)
=== RUN   TestPinIsPerTenant
--- PASS: TestPinIsPerTenant (0.01s)
PASS
ok  matrixsentry/comms  0.475s

$ go test ./cmd/sentrymcp/ -run 'TestPinSurvivesBlobGC|TestPostImageGetImageRoundTrip' -v
=== RUN   TestPostImageGetImageRoundTrip
--- PASS: TestPostImageGetImageRoundTrip (0.16s)
=== RUN   TestPinSurvivesBlobGC
--- PASS: TestPinSurvivesBlobGC (0.02s)
PASS
ok  matrixsentry/cmd/sentrymcp  0.486s

$ go test ./... && go vet ./...
ok  matrixsentry/blob          (cached)
ok  matrixsentry/cmd/sentrymcp 0.762s
ok  matrixsentry/comms         0.688s
... (all other packages cached/pass)
PASS — go vet clean
```
