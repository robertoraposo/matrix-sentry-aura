# Re-embed Corpus to bge-m3 (1024-d) · Design Spec

> Date: 2026-06-21 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved → plan → subagent-driven (build the tool) → PAUSE → run the prod migration WITH the owner.

## Problem

The personal server (8808) embeds via Ollama nomic-embed-text-v2-moe (768-d). The owner is switching to a local
GPU embedder — **Ante Crucible serving BAAI/bge-m3 (1024-d)** at `http://100.93.11.62:11435`, which is
**Ollama-API-compatible** (`POST /api/embed`, `{model,input}` → `{embeddings:[[...]]}`, batch, ignores the model
name). This kills the ~300ms Ollama round-trip and bge-m3's multilingual + long-context properties fix the
Spanish-query/English-memory ranking misses we measured.

The blocker: **the dimension changes 768→1024.** The ~227 live memories (303 EventMemory records incl.
superseded) are 768-d; `memory.New` dim-validates every persisted vector against the embedder, so simply
pointing the server at :11435 (dim 1024) makes it FAIL to start. The corpus must be re-embedded.

## Decisions

- **No new embedder client.** The bge-m3 endpoint is Ollama-compatible → the existing `memory.OllamaEmbedder`
  works pointed at :11435 with dim 1024. The only config change is `SENTRY_OLLAMA_URL` (→ :11435),
  `SENTRY_EMBED_DIM` (→ 1024), `SENTRY_EMBED_MODEL` (→ bge-m3, cosmetic — server ignores it).
- **Re-embed by rewriting the journal record-by-record (lossless).** A new `cmd/reembed` streams the OLD journal
  into a NEW one: every `EventMemory` record gets its TEXT re-embedded via bge-m3 (768→1024 vector, all other
  fields — id, tags, source, supersedes — preserved); EVERY OTHER record (Access, PathMap, Forget, Message,
  Recall) is copied VERBATIM. Records are written in original order, so journal seqs stay identical (comms `ref`
  / any seq references survive). Result: a byte-different but semantically-identical 1024-d twin — comms, access
  history, path registry, supersede/forget chains all preserved; only the memory vectors change space.
- **Re-embed ALL EventMemory records (incl. superseded), not just the 227 live ones.** `memory.New` loads and
  dim-validates every EventMemory record before applying supersede/forget drops — a leftover 768-d record would
  fail validation. So all 303 must become 1024-d.
- **Owner-coordinated prod cutover.** The tool is built + tested in isolation; running it on 8808 stops the live
  server, rewrites the corpus, and swaps journals — done WITH the owner (he asked to be notified before
  proceeding). Old journal is backed up; reversible.

## Architecture

### `cmd/reembed` (new)

Flags: `-src` (old journal dir, required), `-dst` (new journal dir, required, must be empty/new),
`-url` (embed base URL, default the bge-m3 endpoint), `-model` (default "bge-m3"), `-dim` (default 1024),
`-batch` (default 64).

Logic:
1. `sentry.Open(src)`; **Pass 1** — `Scan` all records; for each `EventMemory`, decode `memory.MemoryPayload`,
   record `seq → Text`. Batch-embed all texts via `memory.NewOllamaEmbedder(url, model, dim)` (validates dim
   1024). Build `seq → []float32`.
2. `sentry.Open(dst)` (fresh); **Pass 2** — `Scan` src again in order; for each record:
   - `EventMemory`: decode payload, set `Vector = embeds[seq]`, keep ID/Tags/Source/Supersedes, `dst.Append(rec.Tenant, memory.EventMemory, payload)`.
   - any other type: `dst.Append(rec.Tenant, rec.Type, json.RawMessage(rec.Payload))` (verbatim — `json.RawMessage` round-trips the exact bytes).
3. Verify: dst record count == src record count; dst opened with a 1024 embedder via `memory.New` succeeds (proves dim consistency + supersede/forget replay); print {records copied, memories re-embedded, dim}.

It reuses `sentry.Store` (Open/Scan/Append) + `memory.OllamaEmbedder` + `memory.MemoryPayload` — no new infra.

## Testing (TDD)

- **`cmd/reembed`** against a synthetic src journal built in the test (a small fake embed server via `httptest`
  returning fixed 4-d vectors so we don't need a real model): seed src with 2 EventMemory (one superseded), 1
  EventForget, 1 a non-memory record (e.g. AccessPayload); run the rewrite; assert dst has the SAME record count
  and SAME seqs/types/order, the EventMemory vectors are now the new dim, the non-memory record's payload is
  byte-identical, and `memory.New(dst, fakeEmbedder)` rebuilds the SAME live set (superseded/forgotten dropped).
- **No prod data in tests** — the synthetic journal + httptest embedder make it hermetic.

## Migration runbook (owner-coordinated; NOT auto-run)

1. Build `cmd/reembed` (linux), ship to server1 (it reaches :11435).
2. `systemctl stop sentrymcp` (brief downtime; clients retry).
3. `reembed -src /root/sentry-journal -dst /root/sentry-journal-1024 -url http://100.93.11.62:11435` → verify
   counts.
4. Backup: `mv /root/sentry-journal /root/sentry-journal-768.bak`; `mv /root/sentry-journal-1024 /root/sentry-journal`.
5. Edit `/root/sentrymcp.env`: `SENTRY_OLLAMA_URL=http://100.93.11.62:11435`, `SENTRY_EMBED_DIM=1024`,
   `SENTRY_EMBED_MODEL=bge-m3`.
6. `systemctl start sentrymcp`; verify: starts clean (no dim error), `stats` count unchanged, a `recall` returns
   sensible on-topic results in the new space, `analyze_recall`/comms intact.
7. Rollback if needed: restore the .768.bak journal + old env. (Old journal untouched.)

8809 (teams, mistral-1024) is separate — left as-is unless the owner wants to unify on bge-m3 later (also needs
a re-embed since mistral-1024 ≠ bge-1024 space, but those corpora are ~empty).

## Out of scope (YAGNI)

- A new embedder client (Ollama-compatible API → reuse OllamaEmbedder).
- Online/zero-downtime migration (brief stop is fine for a personal server).
- Re-embedding 8809 teams now (separate, near-empty).
- Preserving exact byte offsets (only seqs/semantics matter; offsets naturally differ).
