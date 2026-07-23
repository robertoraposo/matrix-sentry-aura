# Comms Wake + Deploy Hooks Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps use checkbox syntax.

**Goal:** Build `cmd/sentry-wake` (single Stop hook: inbox wake-on-reply > reflect > close) and `cmd/sentry-deploy`
(PostToolUse deploy-evidence hook), plus `scripts/install-comms-hooks.sh` targeting Claude Code or Codex — real
lifecycle hooks that fire, replacing the memory-only "operational hooks".

**Architecture:** Mirror the existing hook binaries EXACTLY (`cmd/sentry-reflect`, `cmd/sentry-record`): single Go
binary, stdlib only, config from env or `~/.matrix-sentry.env`, JSON-RPC `tools/call` to `POST {SENTRY_MCP_URL}`
with Bearer auth, best-effort **exit 0 on every path**, no-op when URL unset. Spec:
`docs/superpowers/specs/2026-07-23-comms-wake-hooks-design.md`.

## Global Constraints

- Pure Go, stdlib only, one binary per hook. Every code path exits 0 (hooks must never break the harness).
- No-op silently when `SENTRY_MCP_URL` is unset (safe to install before configuring).
- Reuse the proven template verbatim: `loadConfig` (env → `~/.matrix-sentry.env`), `safeName` path-traversal guard,
  per-session state under `os.UserCacheDir()/matrix-sentry/<hook>/`, JSON-RPC envelope + Bearer header.
- Decision logic MUST be a pure function (no I/O) so it is unit-testable without a server.
- Do NOT modify `cmd/sentry-reflect`/`sentry-record`/`sentry-recall` binaries; only the installer swaps the Stop
  registration. Wake reuses sentry-reflect's reflection prompt text + `reflectEvery=40` cadence + `CountToolUses`.

## Template references (read these first)

`cmd/sentry-reflect/main.go` (Stop stdin, stateDir/safeName, readCount/writeCount, reflectEvery=40, blockOutput
`{"decision":"block","reason":...}`, decide, loadConfig, `~/.matrix-sentry.env` parse) · `cmd/sentry-record/main.go`
(PostToolUse stdin `{tool_name,tool_input,tool_response,cwd}`, buildCallBody, post fire-and-forget) ·
`cmd/sentry-recall/main.go` (JSON-RPC recall + parse reply) · `internal/transcript` (CountToolUses) ·
`scripts/install-reflect-hook.sh` (jq idempotent merge into settings.json).

---

## Task 1: `cmd/sentry-deploy` (PostToolUse deploy-evidence)

**Files:** Create `cmd/sentry-deploy/main.go`, `cmd/sentry-deploy/main_test.go`. Reuse the `loadConfig`/`post`
pattern from `cmd/sentry-record`.

**Interfaces (produce):** `isDeployCommand(cmd string) bool` (pure); `buildPostBody(area, from, text string)
[]byte` (JSON-RPC `post` envelope); `main` reads PostToolUse stdin, acts only on `tool_name=="Bash"` +
`isDeployCommand(tool_input.command)`, best-effort `post`, exit 0 always.

- [ ] Step 1: Failing tests — `TestIsDeployCommand` (table: `systemctl restart sentrymcp`, `gh pr merge 5`,
  `git merge feat/x`, `scp bin matrix-sentry:/root/sentrymcp` → true; `ls`, `go test ./...`, `git status` →
  false). `TestBuildPostBody` asserts a valid `tools/call`/`post` envelope with area/from/text.
  `TestNoopWhenURLUnset` (main-level: empty config → no panic, exit 0, no request via a stub transport).
- [ ] Step 2: Run → FAIL.
- [ ] Step 3: Implement mirroring `cmd/sentry-record` (loadConfig, 700ms client, ignore body/err, exit 0).
- [ ] Step 4: Run → PASS; `go build ./cmd/sentry-deploy`.
- [ ] Step 5: Commit — `feat(hooks) sentry-deploy PostToolUse: post deploy evidence to comms`.

## Task 2: `cmd/sentry-wake` — pure decision logic + state

**Files:** Create `cmd/sentry-wake/main.go` (types + `decide`), `cmd/sentry-wake/main_test.go`.

**Interfaces (produce):**
- `type action int` = `actWake | actReflect | actClose | actBlockAlert`.
- `type state struct { Cursor uint64; ReflectCount int }` (JSON, per-session file).
- `type inboxMsg struct { Seq uint64; From, Kind, Text, Target string }`.
- `func decide(msgs []inboxMsg, st state, stopActive bool, toolDelta int) (action, state)` — PURE. Priority:
  Wake if `!stopActive && any msg.Seq > st.Cursor` (new state Cursor=maxSeq); else Reflect if
  `!stopActive && toolDelta >= 40` (new ReflectCount handled by caller); else BlockAlert if
  `stopActive && any msg.Seq > st.Cursor`; else Close.
- `func safeName(id string) string` and state read/write (copy from sentry-reflect).

- [ ] Step 1: Failing tests — `TestDecideWake` (new msg → actWake, Cursor advances to maxSeq); `TestDecideNoReWake`
  (same msgs, Cursor already at maxSeq → NOT actWake); `TestDecideStopActiveNeverWakes` (stopActive=true + new
  msgs → actBlockAlert, never actWake); `TestDecideReflectAtThreshold` (no new msgs, toolDelta 39→Close, 40→Reflect);
  `TestDecideCloseWhenCaughtUp`; `TestSafeNameTraversal` (`../../etc` → single safe basename); state round-trip.
- [ ] Step 2: Run → FAIL.
- [ ] Step 3: Implement `decide` + state helpers (pure; mirror sentry-reflect's stateDir/safeName/read/write).
- [ ] Step 4: Run → PASS.
- [ ] Step 5: Commit — `feat(hooks) sentry-wake decision core (wake>reflect>close>block-alert) + session state`.

## Task 3: `cmd/sentry-wake` — I/O wiring (inbox fetch, reflection reuse, main)

**Files:** Modify `cmd/sentry-wake/main.go`, `cmd/sentry-wake/main_test.go`.

**Interfaces (produce):** `func parseInbox(rpcReplyBody []byte) ([]inboxMsg, uint64)` (parse `tools/call` reply
`result.content[0].text`: the `#N [kind] from→target @area: text` lines + trailing `(cursor: #N)`); `buildInboxBody
(target string, since uint64) []byte`; `buildPostBody(...)`; `reflectionPrompt()` + `reflectEvery=40`
(copied from sentry-reflect); `main` (read stdin, loadConfig, no-op if URL unset, fetch inbox ~5s, `decide`,
persist state, emit `{"decision":"block","reason":...}` for Wake/Reflect, best-effort `post` for Close/BlockAlert,
exit 0 on every path).

- [ ] Step 1: Failing tests — `TestParseInbox` (a real inbox reply string → msgs + cursor; empty/`inbox empty` →
  none); `TestBuildInboxBody`; `TestWakeReasonContainsMessages`; `TestMainNoopWhenURLUnset` (exit 0, no output).
- [ ] Step 2: Run → FAIL.
- [ ] Step 3: Implement parsing + main (JSON-RPC via stubbable transport; 5s inbox client; 700ms post client).
- [ ] Step 4: Run → PASS; `go build ./cmd/sentry-wake`; `go vet ./cmd/sentry-wake/...`.
- [ ] Step 5: Commit — `feat(hooks) sentry-wake I/O: inbox fetch/parse, wake-on-reply block output, main`.

## Task 4: `scripts/install-comms-hooks.sh` (Claude + Codex)

**Files:** Create `scripts/install-comms-hooks.sh` (mirror `install-reflect-hook.sh`).

- [ ] Step 1: Write the script: flags `--target claude|codex` (default claude), `--build|--wake-bin|--deploy-bin`,
  `--url`. Build/place both binaries to `$HOME/.local/bin/`. jq idempotent merge:
  - Claude → `~/.claude/settings.json`: `.hooks.Stop` set to a group whose command is `sentry-wake`, **removing any
    existing entry whose command ends in `sentry-reflect`**; `.hooks.PostToolUse` += a `{matcher:"Bash",
    hooks:[{command:<sentry-deploy>, async:true}]}` group (idempotent by command).
  - Codex → `~/.codex/hooks.json`: same `{hooks:{Stop:[...],PostToolUse:[...]}}` shape; then echo the reminder to
    set `[features] hooks = true` in `~/.codex/config.toml`.
  - Optional `--url` → write `SENTRY_MCP_URL` to `~/.matrix-sentry.env` if absent. Verify `printf '{}' | <bin>`
    exits 0 for both. Atomic mktemp + jq-validate + mv; abort on invalid JSON.
- [ ] Step 2: Smoke-test manually: run with `--target claude` against a temp `CLAUDE_SETTINGS=/tmp/s.json` (empty
  and pre-populated); assert valid JSON + idempotent re-run (no duplicates); repeat `--target codex` against a temp
  hooks.json. (A tiny bash/jq assertion block or a Go `os/exec` test is fine.)
- [ ] Step 3: Commit — `feat(hooks) install-comms-hooks.sh: register wake+deploy in Claude Code or Codex`.

## Self-review
Spec coverage: wake/reflect/close/block-alert (T2,T3) · deploy evidence (T1) · installer both targets + Stop swap
(T4) · no-op-when-unset + exit-0 (all) · loop guard via cursor advance (T2). Types consistent: `decide`, `state
{Cursor,ReflectCount}`, `inboxMsg`, `isDeployCommand`, `parseInbox`, `buildPostBody`/`buildInboxBody` named the
same across tasks. Deploy = human decision (this plan builds + installs into config; it does not run the loop).
