# Comms Wake + Deploy Hooks (cross-harness: Claude Code + Codex) · Design Spec

> Date: 2026-07-23 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved → plan → subagent-driven. Turn the H0–H5 "operational hooks" (today only a memory + posts,
> so they never fire) into REAL lifecycle hooks that the harness executes: a wake-on-reply watcher, deploy
> evidence, and block/close signalling — installable in BOTH Claude Code and Codex.

## Problem

An agent (Codex) "defined" operational hooks by storing a MatrixSentry memory (#2537) + posting a protocol
description, then reported them active. But `/hooks` shows **0 installed / 0 active**: nothing fires. Those
"hooks" only happen if the agent voluntarily follows the protocol each turn — the unreliable "LLM housekeeping"
failure mode. And the "watcher" is not a daemon: MatrixSentry is MCP (request/response), so a poll loop lives
only while a session runs and dies at turn/session end.

Root cause: conflating an **operational hook** (agent discipline) with a **lifecycle hook** (config the harness
runs). Only the second fires automatically.

## Grounding (researched, cited 2026-07-23)

- **Both harnesses support the same hook model.** Codex CLI has native lifecycle hooks (PR #19882) with a
  Claude-Code-compatible schema and the SAME wake mechanism. Only the config **file** differs:
  Claude Code → `~/.claude/settings.json`; Codex → `~/.codex/hooks.json` (or `[hooks]` in `~/.codex/config.toml`,
  gated by `[features] hooks = true`). Both use `{event: [{matcher?, hooks:[{type:"command", command, timeout?,
  statusMessage?}]}]}`.
- **Wake-on-reply is real (both).** A Stop hook that exits 0 and prints `{"decision":"block","reason":"<text>"}`
  does NOT end the turn — the harness creates a continuation prompt using `reason` as its text. `stop_hook_active`
  on stdin is the loop guard. → one binary can drive the loop in either harness; no daemon needed.
- **Repo template exists.** `cmd/sentry-reflect` (Stop), `cmd/sentry-record` (PostToolUse), `cmd/sentry-recall`
  (SessionStart): single Go binaries, config from env or `~/.matrix-sentry.env` (`SENTRY_MCP_URL`/`SENTRY_MCP_TOKEN`),
  server call = stateless JSON-RPC `POST {url}` `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name",
  "arguments"}}` with `Authorization: Bearer <token>`, **best-effort exit 0 on every path**, no-op if URL unset.
  Per-session state lives under `os.UserCacheDir()/matrix-sentry/<hook>/<safeName(session_id)>`, never `~/.claude`.

## Decision (key fork, resolved)

`sentry-reflect` already owns the **Stop** event, and two Stop hooks both emitting `decision:"block"` is
undocumented/racy. → **Merge into a single Stop hook, `sentry-wake`,** that does all turn-end jobs in priority
order (wake > reflect > close). The installer replaces the `sentry-reflect` Stop entry with `sentry-wake`.
`sentry-reflect` remains in the tree (its reflection prompt/cadence is reused by `sentry-wake`).

## Goals / Non-goals

**Goals.** (1) `sentry-wake`: a real Stop hook that at turn-end fetches the agent's MatrixSentry inbox and, when
new directed messages exist, **wakes the turn** (`decision:block`) so the intra-session loop survives across
turns; else runs the reflection cadence; else posts a close confirmation. (2) `sentry-deploy`: a PostToolUse
hook that detects deploy/merge commands and posts evidence to comms. (3) block/loop signalling folded into
`sentry-wake`. (4) One installer that targets Claude Code OR Codex.

**Non-goals.** A background daemon (impossible over MCP; the Stop-hook continuation IS the loop). Changing the
server. Auto-detecting "blocked" perfectly (best-effort heuristic only). Touching the deployed `sentry-record`/
`sentry-recall`/`sentry-reflect` binaries (only the Stop *registration* is swapped by the installer).

## Components

### 1. `cmd/sentry-wake` — the single Stop hook (watcher + reflect + close)

**Stdin (Stop):** `{session_id, transcript_path, stop_hook_active bool, cwd}` (same as sentry-reflect).
**Config:** `SENTRY_MCP_URL`, `SENTRY_MCP_TOKEN` (env → `~/.matrix-sentry.env`); agent identity from
`SENTRY_COMMS_AGENT` (label used as inbox `target` and post `from`) and `SENTRY_COMMS_AREA` (default post area),
each falling back to `filepath.Base(cwd)` for area and `"agent"`/host for label. No-op (exit 0) if URL unset.
**Per-session state** (`os.UserCacheDir()/matrix-sentry/wake/<safeName(session_id)>`, JSON): `{cursor uint64,
reflectCount int}`.

**Decision (priority order), all paths exit 0:**
1. **Wake.** If `!stop_hook_active`: call `inbox(target=<agent>, since=cursor)` (JSON-RPC, ~5s). Parse the
   returned messages + trailing `(cursor: #N)`. If there are messages with seq > cursor → write `cursor=maxSeq`
   (advancing the cursor IS the primary loop guard — the same messages never wake twice) and print
   `{"decision":"block","reason":"<compact list of the new directed messages>. Process them; reply with
   post(area,kind=answer,ref,target)."}`. **WAKE — loop continues.**
2. **Reflect.** Elif `!stop_hook_active` and `CountToolUses(transcript) - reflectCount >= 40`: write
   `reflectCount=current` and print `{"decision":"block","reason":<reflectionPrompt, reused from sentry-reflect>}`.
3. **Close.** Else: best-effort fire-and-forget `post(area, from, kind="note", text="cierre: <session> — inbox
   caught up @cursor #N")` (~700ms), then exit 0 with **no** decision (turn ends).

**Block/loop signal:** if `stop_hook_active` AND `inbox` still returns messages newer than cursor (we won't wake
again this continuation), best-effort `post(area="antelisp"/<area>, from=<agent>, target="forge-orchestrator",
kind="info", text="wake guard: holding, N undelivered @#cursor")` and also target `backend`. Then exit 0 (allow
stop). This is the automatic "bloqueo/bucle" alert.

### 2. `cmd/sentry-deploy` — PostToolUse hook (deploy evidence)

**Stdin (PostToolUse):** `{tool_name, tool_input{command}, tool_response, cwd}`. Only acts when
`tool_name=="Bash"` and `tool_input.command` matches a deploy/merge pattern:
`systemctl (restart|reload) sentrymcp`, `gh pr merge`, `git merge`, `scp .*sentrymcp`, `git push .*(main|deploy)`.
On match → best-effort fire-and-forget `post(area, from, kind="info", text="deploy: <first line of command> @ <cwd>")`.
Exit 0 always; no-op if URL unset or no match. Passive → `async:true`, no injected output.

### 3. Installers

`scripts/install-comms-hooks.sh [--target claude|codex] [--build | --wake-bin PATH --deploy-bin PATH] [--url URL]`
mirrors `install-reflect-hook.sh`:
- Build/place `sentry-wake` and `sentry-deploy` into `$HOME/.local/bin/`.
- **Claude target** (`~/.claude/settings.json`, jq idempotent-by-command merge): set `.hooks.Stop` to
  `sentry-wake` **removing any existing `sentry-reflect` Stop entry** (the merge supersedes it); add
  `sentry-deploy` under `.hooks.PostToolUse` with `matcher:"Bash"`, `async:true`.
- **Codex target** (`~/.codex/hooks.json`, jq merge, same shape): `.hooks.Stop` → sentry-wake; `.hooks.PostToolUse`
  matcher `"Bash"` → sentry-deploy. Print a reminder to set `[features] hooks = true` in `~/.codex/config.toml`.
- Idempotent by command path; atomic write via mktemp + jq-validate + mv; warn if `SENTRY_MCP_URL` unset
  (hooks are no-ops); verify `printf '{}' | <bin>` exits 0.

## Testing (TDD; `go test ./cmd/sentry-wake/... ./cmd/sentry-deploy/...` green)

`sentry-wake` (pure decision logic isolated from I/O so it is unit-testable):
- `decide()` given (inbox messages, cursor, stop_hook_active, toolDelta) returns the right action
  (Wake/Reflect/Close/BlockAlert) with the right new cursor/reflectCount.
- Wake emits `decision:block` with the messages; cursor advances to maxSeq; a second call with the same inbox
  (no newer seq) does NOT wake (loop guard).
- `stop_hook_active=true` never wakes/reflects; with undelivered messages it selects BlockAlert.
- Reflect fires only at delta>=40 and only when not waking.
- Inbox JSON-RPC reply parsing (messages + `(cursor: #N)`); malformed reply → no wake, exit 0.
- Config load + `safeName` path-traversal guard (mirror sentry-reflect); state round-trip.

`sentry-deploy`:
- Command matcher: table of deploy commands → match, benign commands → no-op.
- Builds the correct `post` JSON-RPC envelope; no-op when URL unset; always exit 0 (including on HTTP error,
  via a stubbed transport).

Installer: a smoke test asserting the jq merge produces VALID JSON for both an empty and a pre-populated
settings file, and that re-running is idempotent (no duplicate command entries), for both `--target` values.

## Acceptance criteria

1. `go build ./...` and `go test ./cmd/sentry-wake/... ./cmd/sentry-deploy/...` pass; `go vet` clean.
2. Fed a Stop stdin with a fresh inbox message, `sentry-wake` prints `{"decision":"block","reason":...}`
   containing that message; fed the same message again (cursor advanced) it does NOT wake.
3. `stop_hook_active=true` never produces a wake/reflect; every path exits 0.
4. `sentry-deploy` posts on a deploy command, no-ops on an ordinary command, always exits 0.
5. Both binaries are silent no-ops when `SENTRY_MCP_URL` is unset (safe to install before configuring).
6. `install-comms-hooks.sh --target claude` and `--target codex` each produce VALID, idempotent config with the
   Stop→sentry-wake swap and PostToolUse→sentry-deploy entry.
