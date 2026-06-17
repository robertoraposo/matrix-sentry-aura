# Agent Inbox + Bounded Admin Scans · Design Spec

> Date: 2026-06-16 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved → plan → subagent-driven → deploy. Two complementary fixes: stop the admin/analytics
> endpoints from re-reading the whole journal (perf #3), and give agents an `inbox` tool so directed messages
> aren't lost.

## Problem

1. **Full-journal-scan perf (#3).** `analyze_recall`, `analyze_access`, `/admin/journal`, `/admin/comms` call
   `sentry.Store.Scan`, which reads EVERY record seq 1..n. Measured at 30,749 events: ~347ms per request, and
   it grows linearly with the journal. The hot path (recall/remember, served from in-RAM indexes) is sub-ms; it
   is only these recency/analytics endpoints that re-read everything.
2. **Agents lose comms.** A recipient must guess which area to `read`, and `read` is per-area, so messages
   directed at an agent across different areas are easy to miss. There is no "what's addressed to me?" query.

## Decisions

### Part A — bound the scans

- **Add `sentry.Store.ScanReverse(filter, fn)`** — iterate seq n→1 via the existing `keydir`; `fn` returns
  `false` to stop early. Trivial (mirror of `Scan`). Lets recency endpoints stop after collecting the last N
  matches → O(records-until-N-matches) instead of O(n).
- **`/admin/journal`** and **`analyze_recall`**: reverse-scan, stop after the last N matches (journal: the
  `limit`, default 60; analyze_recall: a fixed window, e.g. 500 recent recalls — coverage over recent recalls
  is the meaningful signal). Reverse the collected slice back to chronological for display.
- **`/admin/comms`**: serve from the `comms.Store` in-RAM index (`s.chat`) — it already holds all messages;
  add `comms.Store.Recent(tenant, limit)` (last N tenant messages, in-RAM). No journal read at all.
- **`analyze_access`** stays a full scan — its Markov metric genuinely needs every access event; it is an
  occasional analytic, not a per-request hot path. Documented as the one intentional O(n) path.

### Part B — `inbox` tool

- **`comms.Store.Inbox(tenant, target, since)`** — in-RAM filter over `s.chat.entries`: `Tenant==tenant &&
  Target==target && Seq>since`, across ALL areas, in seq order (mirrors `Read` but filters by `Target`). Fast,
  no scan.
- **MCP `inbox(target, since?)` tool** — returns the messages addressed to `target` since the cursor, rendered
  like `read` (`#seq [kind] from→target: text`) plus the trailing max-seq cursor. Tenant-scoped. The tool
  description encodes the convention: *poll your inbox with your own label so you never miss directed messages;
  reply with `post(kind=answer, ref=<seq>, target=<sender>)`.*

This is the `inbox` piece of the paused comms-wake design; the `sentry-comms` Stop hook + the /loop Monitor
("wake on reply") remain deferred — this spec ships only the tool, which is what stops messages being lost.

## Architecture (no storage-format changes)

- `sentry/store.go`: `ScanReverse(filter Filter, fn func(Record) bool) error` (RLock for `n`, loop n→1, same
  filter logic as `Scan`, `Read(Seq(i))`).
- `comms/comms.go`: `Inbox(tenant, target, since)` and `Recent(tenant, limit)` — both in-RAM over `entries`
  under `mu`.
- `cmd/sentrymcp/main.go`: `/admin/journal` + `analyze_recall` → `ScanReverse` bounded; `/admin/comms` →
  `s.chat.Recent`; new `inbox` tool def + handler (uses `s.chat.Inbox`, tenant from `resolveTenant`/dispatch).

## Testing (TDD)

- **`sentry.ScanReverse`**: append records of mixed type/tenant; ScanReverse visits them newest-first; filter
  by type/tenant works; returning `false` from `fn` stops early (assert it did NOT read the whole journal — e.g.
  count fn invocations).
- **`comms.Inbox`**: messages with `Target=="me"` across two areas are returned (other targets/tenants
  excluded); `since` cursor returns only newer; empty when none.
- **`comms.Recent`**: returns the last N tenant messages in seq order; excludes other tenants.
- **`cmd/sentrymcp`**: `inbox` tool round-trips (post directed to "me" in two areas → `inbox(target=me)` returns
  both with a cursor; `since` filters); `/admin/journal` still returns the right recent events after the
  reverse-scan rewrite (existing journal test stays green); `analyze_recall` still reports coverage (bounded).
- **Perf sanity** (manual, at deploy): `analyze_recall` and `/admin/journal` latency drops from ~350ms toward
  the few-ms range now that they stop after N instead of reading 30k.

## Deployment

Rebuild sentrymcp; redeploy 8808 + 8809. Verify: (a) `inbox(target=<an agent label>)` returns that agent's
directed messages; (b) `analyze_recall` and `/admin/journal` latency is now a few ms (vs ~350ms); (c) the
dashboard comms/journal still render. Add the `inbox` convention to the comms protocol memory.

## Out of scope (YAGNI / follow-ups)

- The `sentry-comms` Stop hook + /loop Monitor "wake on reply" (deferred; this ships the tool only).
- Soft-validation of `post` (the question-without-target warning) — fold into the comms-protocol work later.
- Optimizing `analyze_access` (intentional full scan).
- A general secondary index on the journal (reverse-scan + the existing in-RAM stores cover the current needs).
