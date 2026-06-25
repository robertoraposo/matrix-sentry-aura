# Comms Image Transfer · Design Spec

> Date: 2026-06-24 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved → plan → subagent-driven → deploy. Let visual/UI agents hand images to other agents
> through Matrix in a base64/object format, without bloating the in-RAM comms index or the journal.

## Problem

Agents can only exchange text through comms today. A visual or UI agent (screenshot, mockup, rendered chart)
has no way to pass the actual pixels to a vision-capable agent — it can only describe them. We want an
agent→agent image handoff in a "fast object/base64" format, reachable by BOTH local agents (Claude Code over
the LAN, `:8808`) and remote agents (claude.ai / Cloudflare), so the delivery must work without server
filesystem access.

Two hard constraints from the code shape the design:

- **Journal payload is capped at 16 MiB per record** (`sentry/store.go` `recover()`, `maxPayload = 16<<20`).
  A ≤15 MB image base64-encoded is ~20 MB > cap — so image **bytes cannot live inline in a journal record**.
- **`comms.Store` keeps every message in RAM** (`entries []Message`) and **replays the whole journal on
  `New()`**. Inlining image bytes in a message would grow RAM and restart time with image volume — exactly
  what the retention work (2000/14d) was built to prevent.

## Decisions

- **Delivery is MCP-native `image` content (base64).** `post_image` accepts base64 in, `get_image` returns
  base64 out as MCP `image` content (universal: works for local AND remote agents). base64 exists only at the
  edges; in the middle a ~200-byte reference travels.
- **Bytes live in a content-addressed blob store on disk; only a reference lives in the journal/RAM.** This
  sidesteps the 16 MiB cap (the reference is tiny), keeps the comms in-RAM index light (ref-sized, like a text
  message), and dedups identical images by sha256.
- **Blobs are global, content-addressed: `<dir>/blobs/<sha256>`.** Dedup is cross-tenant (same image = one
  file). Access control lives in the **tenant-scoped reference**: `get_image(seq)` only returns bytes when the
  caller's tenant owns a message referencing that blob — a leaked sha alone reads nothing.
- **Hybrid lifecycle: ephemeral by default, pin to preserve.** Image references ride the existing comms
  machinery, so retention (N ∩ days) and `comms_clear` age them out of the live index for free. A blob file is
  GC-deletable when **no live (retained, un-cleared) image message in ANY tenant references it AND it is not
  pinned**. The journal is never touched — GC only deletes orphan blob *files*.
- **Limits: ≤ 15 MB per image, any image mime.** Over-size or non-image mime is rejected with a clear error.

## Architecture

### Component 1 — `blob` package (new): content-addressed byte store

Pure storage, no journal knowledge. Lives at `blob/blob.go`.

```go
type Store struct { dir string; mu sync.Mutex }
func Open(dir string) (*Store, error)                      // mkdir <dir>/blobs
func (s *Store) Put(data []byte) (sha string, err error)  // sha256 hex; write-once (skip if exists)
func (s *Store) Get(sha string) ([]byte, error)           // ErrNotFound if missing
func (s *Store) Delete(sha string) error                  // idempotent
func (s *Store) List() ([]string, error)                  // all sha on disk (for GC)
```

- `Put`: `sha = hex(sha256(data))`; path `<dir>/blobs/<sha>`; if the file exists, return the sha without
  rewriting (dedup, write-once). Write via temp-file + rename for atomicity.
- Global/content-addressed: no tenant in the path. Tenant gating is the caller's job (Component 2/3).

### Component 2 — image references in `comms` (`comms/comms.go`)

- New event types (registry next = 8): `const EventImage sentry.EventType = 8`,
  `const EventBlobPin sentry.EventType = 9`.
- `MessagePayload` gains optional image fields: `BlobID string`, `Mime string`, `W int`, `H int`,
  `Size int` (all `json:",omitempty"`). When `BlobID != ""` the message is an image (`Kind:"image"`).
- `Message` gains the same fields so reads carry the ref (never the bytes).
- New `func (s *Store) PostImage(tenant, p MessagePayload) (seq uint64, err error)`: like `Post` but requires
  `BlobID` + `Mime` (Text/caption optional), appends an `EventImage` record, adds to the in-RAM index, prunes.
  (Text-only `Post` is unchanged; its `Text` required-check stays.)
- New `func (s *Store) GetBySeq(tenant sentry.TenantID, seq uint64) (Message, bool)`: returns the tenant's
  message at `seq` regardless of area (image tools have a seq, not an area). Tenant-scoped — another tenant's
  seq returns `false`. This is what `get_image`/`pin_image` resolve through.
- `New()` rebuild gains the image + pin passes alongside the existing message + clear passes, merged in seq
  order: scan `EventMessage` **and** `EventImage` into `entries` (sort/merge by seq), then apply
  `EventCommsClear` tombstones (already drops image messages too, since they share Area/Seq), then replay
  `EventBlobPin` into a `pinned map[string]bool`, then `prune()`.
- Pin: `const ... EventBlobPin` payload `{ BlobID string; On bool }`.
  `func (s *Store) Pin(tenant, blobID string, on bool) error` appends the record and updates `s.pinned`.
  Pins are global (a blob is shared); `s.pinned` is a set of sha. (Tenant recorded for audit only.)
- GC support: `func (s *Store) LiveBlobIDs() map[string]bool` returns the union of (a) `BlobID` of every live
  image message across all tenants and (b) every pinned sha. This is the keep-set for the sweep.

### Component 3 — blob GC (`comms/comms.go` + caller)

- `func (s *Store) GC(b *blob.Store) (deleted int, err error)`: `keep := s.LiveBlobIDs()`; for each sha in
  `b.List()`, if `!keep[sha]` then `b.Delete(sha)`; return count. Holds `s.mu` while reading the keep-set,
  releases before disk deletes.
- Invoked: once on startup (after `comms.New`), after each `comms_clear`, and via a manual `blob_gc` admin
  path. (No background timer in v1 — YAGNI; these triggers cover the ephemeral story.)

### Component 4 — MCP wiring (`cmd/sentrymcp/main.go`)

Open the blob store next to the journal dir; pass it to the image tools. New tools (all tenant-scoped via the
existing per-request dispatch):

- `post_image{ area(req), from(req), data(req, base64), mime(req), target?, caption?, w?, h? }` → decode
  base64 → reject if `len > 15<<20` or mime not `image/*` → `blob.Put` → `comms.PostImage{...}` → return the
  seq. `w`/`h` are optional caller-supplied dimensions; if omitted, fill best-effort via stdlib
  `image.DecodeConfig` (png/jpeg/gif only — **no `golang.org/x/image`, to preserve zero-dep**); unknown
  formats (e.g. webp) store `0/0`.
- `get_image{ seq(req) }` → `comms.Get(tenant, seq)`; if not found / not the tenant's / not an image → error;
  else `blob.Get(BlobID)` → return MCP **image content** `{ type:"image", data: base64, mimeType: Mime }`.
- `pin_image{ seq }` / `unpin_image{ seq }` → resolve seq→BlobID for the tenant → `comms.Pin(tenant, blob, on)`.
- `blob_gc{}` (owner/admin) → `comms.GC(blob)` → "deleted N orphan blobs".
- Existing `read` / `inbox` / `recent` already surface image messages (with the ref + caption, no bytes); add
  the new fields to their rendered output so an agent sees "[image seq=N mime=… 1024x768] caption" and then
  calls `get_image(N)`.

## Data flow

```
post_image(area, from, data=b64, mime, target?, caption?)
  → decode → size/mime check → blob.Put(bytes)=sha (dedup)
  → comms.PostImage{Kind:"image", BlobID:sha, Mime, W, H, Size, Text:caption, Area, Target} → seq

read / inbox / recent → image message with ref (NO bytes) — cheap, in-RAM
get_image(seq)        → comms.Get(tenant,seq) → blob.Get(ref) → MCP image content (base64)
pin_image(seq)        → EventBlobPin{On:true}   (preserve from GC)
comms_clear(area) / retention → drops ref from live index → blob becomes GC-eligible (if unpinned, unreferenced)
blob_gc               → delete orphan blob files (journal untouched)
```

## Testing (TDD)

- **`blob`**: Put returns stable sha; same bytes twice = one file (dedup, write-once); Get round-trips; Get
  missing = ErrNotFound; Delete idempotent; List returns all sha.
- **`comms`**: `PostImage` → `Read`/`Inbox`/`Recent` return the message with ref fields and no bytes; missing
  BlobID/Mime → error; retention prunes an image ref; `comms_clear` sweeps an image ref; `New()` rebuild
  replays image + clear + pin correctly (image survives unless cleared; pin set restored); seq ordering correct
  when text and image messages interleave.
- **GC**: orphan blob deleted; pinned blob preserved even with no live ref; blob referenced by a live message
  (any tenant) preserved; blob shared by two tenants preserved while either has a live ref.
- **`cmd/sentrymcp`**: `post_image`→`get_image` round-trip returns identical bytes + mime; >15 MB rejected;
  non-image mime rejected; `get_image` of another tenant's seq → error (isolation); `pin_image` then `blob_gc`
  keeps the blob; `read` output shows the image marker.

## Deployment

Rebuild sentrymcp; redeploy 8808 (personal) + 8809 (teams). Additive: new tools + two new event types
(8, 9) + a `blobs/` subdir under each journal dir; existing journals replay unchanged (no records of type 8/9
yet). Verify: `post_image`→`get_image` round-trip over the LAN and over the public MCP; `read` shows image
markers; `blob_gc` deletes nothing while refs are live; pin survives a `comms_clear` + `blob_gc`.

## Out of scope (YAGNI)

- Image transcoding / thumbnail generation / re-compression (store/serve as-is; the agent sizes its own image).
- Background GC timer (startup + post-clear + manual `blob_gc` triggers suffice).
- Per-tenant blob dedup or quotas (global content-addressing chosen; disk is cheap).
- Embedding/semantic search over images (this is a comms handoff, not a memory; CLIP-style recall is a
  separate front).
- Width/height auto-detection beyond what the caller supplies (decode dims best-effort; 0/0 allowed).
