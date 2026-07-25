#!/usr/bin/env bash
# Smoke test for install-comms-hooks.sh. Hermetic: HOME + config files live under
# a temp dir; the sentry-* binaries are a stub that exits 0. Asserts the jq merge
# produces VALID JSON and is idempotent (no duplicate command entries) for an
# empty settings file, a pre-populated one (with a sentry-reflect Stop group that
# must be swapped out), and a Codex hooks.json created from scratch.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALLER="$HERE/install-comms-hooks.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

WAKE="$WORK/bin/sentry-wake"
DEPLOY="$WORK/bin/sentry-deploy"
# Stub source binary standing in for a real sentry-* build: exits 0 on any input
# (same contract as /bin/true, used portably since /bin/true is absent on macOS).
STUB="$WORK/stub"
printf '#!/bin/sh\nexit 0\n' > "$STUB"; chmod +x "$STUB"
echo "using stub binary: $STUB"

run() { # run <target> <config-file>
  local target="$1" file="$2"
  HOME="$WORK/home" \
  SENTRY_WAKE_BIN="$WAKE" SENTRY_DEPLOY_BIN="$DEPLOY" \
  CLAUDE_SETTINGS="$file" CODEX_HOOKS="$file" \
    bash "$INSTALLER" --target "$target" --wake-bin "$STUB" --deploy-bin "$STUB" >/dev/null 2>&1
}

fail=0
assert() { # assert <desc> <actual> <expected>
  if [ "$2" = "$3" ]; then echo "  PASS: $1 ($2)"
  else echo "  FAIL: $1 — got '$2', want '$3'"; fail=1; fi
}

n_wake()    { jq --arg c "$WAKE"   '[.hooks.Stop[]?.hooks[]?        | select(.command==$c)] | length' "$1"; }
n_deploy()  { jq --arg c "$DEPLOY" '[.hooks.PostToolUse[]?.hooks[]? | select(.command==$c)] | length' "$1"; }
n_reflect() { jq '[.hooks.Stop[]?.hooks[]? | select((.command//"")|endswith("sentry-reflect"))] | length' "$1"; }
valid()     { jq -e . "$1" >/dev/null 2>&1 && echo yes || echo no; }

echo "== Case 1: claude, empty {} =="
C1="$WORK/t.json"; printf '{}' > "$C1"
run claude "$C1"
assert "valid JSON"         "$(valid "$C1")"    "yes"
assert "one wake Stop"      "$(n_wake "$C1")"   "1"
assert "one deploy PTU"     "$(n_deploy "$C1")" "1"
echo "  re-run (idempotency)..."; run claude "$C1"
assert "valid after re-run" "$(valid "$C1")"    "yes"
assert "still one wake"     "$(n_wake "$C1")"   "1"
assert "still one deploy"   "$(n_deploy "$C1")" "1"

echo "== Case 2: claude, pre-populated (existing sentry-reflect Stop + unrelated PostToolUse) =="
C2="$WORK/t2.json"
cat > "$C2" <<'JSON'
{
  "hooks": {
    "Stop": [
      {"hooks":[{"type":"command","command":"/home/u/.local/bin/sentry-reflect","timeout":8,"async":false}]}
    ],
    "PostToolUse": [
      {"matcher":"Write","hooks":[{"type":"command","command":"/usr/local/bin/other-hook"}]}
    ]
  }
}
JSON
run claude "$C2"
assert "valid JSON"             "$(valid "$C2")"     "yes"
assert "sentry-reflect removed" "$(n_reflect "$C2")" "0"
assert "one wake Stop"          "$(n_wake "$C2")"    "1"
assert "one deploy PTU"         "$(n_deploy "$C2")"  "1"
assert "unrelated PTU kept"     "$(jq '[.hooks.PostToolUse[]?.hooks[]? | select(.command=="/usr/local/bin/other-hook")] | length' "$C2")" "1"
echo "  re-run (idempotency)..."; run claude "$C2"
assert "still one wake"         "$(n_wake "$C2")"    "1"
assert "still one deploy"       "$(n_deploy "$C2")"  "1"
assert "still zero reflect"     "$(n_reflect "$C2")" "0"

echo "== Case 3: codex, file absent (installer creates it) =="
C3="$WORK/th.json"
run codex "$C3"
assert "file created + valid"   "$(valid "$C3")"     "yes"
assert "one wake Stop"          "$(n_wake "$C3")"    "1"
assert "one deploy PTU"         "$(n_deploy "$C3")"  "1"
echo "  re-run (idempotency)..."; run codex "$C3"
assert "still one wake"         "$(n_wake "$C3")"    "1"
assert "still one deploy"       "$(n_deploy "$C3")"  "1"

echo "== binaries are harness-safe (exit 0 on empty input) =="
printf '{}' | "$STUB" && echo "  PASS: printf '{}' | <bin> exits 0"

echo
if [ "$fail" = 0 ]; then echo "ALL SMOKE ASSERTIONS PASSED"; else echo "SMOKE TEST FAILED"; exit 1; fi
