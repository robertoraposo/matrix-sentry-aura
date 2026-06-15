# Agent Comms Channel · Design Spec (v1)

> Date: 2026-06-15 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved → plan → subagent-driven implementation → deploy.

## Problem

Matrix Sentry is a shared *memory* (pull: recall durable knowledge on demand). The next capability is
real-time *coordination*: several agents working different parts of ONE project need to talk — post a
question, answer one, request info from a specific agent — in a shared "area", and see each other's messages
quickly. This is a chronological communication channel, a different thing from semantic memory.

## Decisions (settled in brainstorming)

- **Delivery v1 = PULL, ready-for-push.** Agents poll an area for new messages since their cursor. Works
  across all 5 heterogeneous clients today (plain request/response MCP). The data model is designed so a v2
  SSE `subscribe` can stream from the same log with zero redesign. (v2 push is gated on "is poll latency
  actually a problem?" once v1 is in use.)
- **A SEPARATE comms log, NOT memory.** Messages are an ordered, chronological, **non-deduped, non-embedded**
  stream — the opposite of semantic memory (which dedups, embeds, has no order). Building chat on `remember`
  would lose ordering, pay embedding per message, and let dedup delete distinct messages. So: a new
  `EventMessage` journal type + a `comms` package + `post`/`read` tools, separate from `remember`/`recall`.
- **Hybrid: promote-to-memory.** A `promote` action turns a worthwhile message (a decision, an answer worth
  keeping) into a durable memory via `memory.Remember`. Chat is transient/ordered; promotion is the bridge to
  durable/semantic.
- **Tenant-scoped.** Channels live inside a tenant (the per-request tenant from the multi-tenant routing), so
  a team's agents share their channels and other tenants can't see them — reusing the isolation already built.

## Architecture

### Component 1 — `comms` package (new; mirrors `memory.Store`'s journal-wrapping pattern)

- `const EventMessage sentry.EventType = 5` (1 access, 2 pathmap, 3 memory, 4 forget are taken).
- `type MessagePayload struct { Area, From, Kind, Text, Target string; Ref uint64 \`json:"...,omitempty"\` }`
  persisted form. `Kind` ∈ {`question`,`answer`,`info`,`note`} (free-form string allowed; default `note`).
  `Target` = an agent label this message is directed at (empty = broadcast). `Ref` = the journal seq of a
  message this one replies to (0 = none; lightweight Q→A threading).
- `type Message struct { Seq uint64; Tenant sentry.TenantID; TS int64; Area, From, Kind, Text, Target string; Ref uint64 }`
  — the read result. **The message id IS its journal seq** (monotonic, unique) — no separate id counter.
- `type Store struct { journal *sentry.Store; mu sync.Mutex; entries []Message }` — keeps messages in RAM
  (rebuilt from `EventMessage` records on `New`, like `memory.Store`), so `Read` is an in-RAM filter, not a
  journal scan per poll (5 agents polling every few seconds must be cheap).
  - `New(journal *sentry.Store) (*Store, error)`: scans `Filter{Type:&EventMessage}`, decodes each payload,
    appends a `Message{Seq: r.Seq, Tenant: r.Tenant, TS: r.Tstamp, …}`. (No embedding, no dedup.)
  - `Post(tenant sentry.TenantID, p MessagePayload) (seq uint64, err error)`: `journal.Append(tenant,
    EventMessage, p)`; append the resulting `Message` to `entries`; return its seq.
  - `Read(tenant sentry.TenantID, area string, since uint64) ([]Message, error)`: the entries with
    `tenant==tenant && Area==area && Seq>since`, in seq order (entries are appended in order). Returns `[]`
    when nothing new. (Optional `target` filtering is done at the MCP layer, not here.)

### Component 2 — MCP tools (`cmd/sentrymcp`), all tenant-scoped via the per-request tenant

- `post` — args `{area (req), text (req), kind?, target?, ref?}` → `posted message #<seq> in <area>`. Reads
  `from` from a required `from` arg (the agent's self-declared label). `kind` defaults to `note`. Appends via
  `comms.Post`. Logs to MokoBlinks.
- `read` — args `{area (req), since? (int cursor), target? (label filter)}` → the new messages, each rendered
  `#<seq> [<kind>] <from>→<target|all>: <text>` (and a trailing line with the max seq so the agent can use it
  as its next `since`). If `target` is given, return only messages whose `Target==target` OR `Target==""`
  (broadcast). Caps the count (e.g. last/next 100) to bound payloads.
- `promote` — args `{area (req), seq (req), tags?}` → reads the message at `seq` in `area`; stores its text as
  a durable memory via `memory.Remember` (tagged, `src="promote"`, prefixed/linked to the source like
  `[from area#seq] text`); returns the memory id. Requires the embedder (errors like `remember` when `mem==nil`).
- All three resolve the tenant from the request (Component already built: `resolveTenant`), so channels are
  isolated per tenant/team.

### Component 3 — server wiring (`main.go`)

`server` gains `chat *comms.Store`, built in `main()` over the same journal (always available — comms needs
no embedder). The `post`/`read` tools work whenever the server runs; `promote` additionally needs `mem`.

## Delivery / real-time

Agents poll: `read(area, since=<lastSeq>)` on their existing loop/hook cadence (seconds). The returned max-seq
becomes the next cursor. "Real-time" at seconds granularity, universal across all clients. **v2 (deferred):**
an SSE `subscribe(area)` endpoint streaming new `EventMessage`s from the same log — no data-model change.

## Testing (TDD)

- `comms`: `Post`→`Read` returns it; `Read(since)` returns only newer; **area filter** (other areas excluded);
  **tenant isolation** (tenant B's `Read` never sees tenant A's messages); **no dedup** (two near-identical
  posts both returned); **ordering** by seq; **survives reopen** (a fresh `New` rebuilds the RAM index from
  the journal, same messages).
- `cmd/sentrymcp`: `post` then `read` round-trip over the tool layer; `read` `target` filter (broadcast +
  @me only); `promote` stores a memory and returns its id; **channel tenant isolation at the tool layer**
  (two tenants' `read` don't cross); missing-`area`/`from` → tool error.

## Out of scope (YAGNI)

- Push/SSE (`subscribe`) — v2, gated on poll latency being insufficient.
- Presence / typing indicators / read receipts.
- Editing or deleting messages (a message-`forget` could come later; v1 is append-only chat).
- Semantic search over messages (that's what `promote`→`recall` is for).
- Intra-tenant ACLs (agents in a tenant cooperate; isolation is at the tenant boundary).
- A separate message-id counter (journal seq is the id).
