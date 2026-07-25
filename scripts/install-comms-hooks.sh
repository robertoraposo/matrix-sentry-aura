#!/usr/bin/env bash
# install-comms-hooks.sh — register Matrix Sentry's comms lifecycle hooks
# (sentry-wake + sentry-deploy) into a Claude Code OR a Codex install. Idempotent:
# safe to re-run. Real lifecycle hooks the harness executes, superseding the
# memory-only "operational hooks".
#
# It installs two binaries into $HOME/.local/bin and merges hook config:
#   * sentry-wake   — Stop hook: wakes the turn on a new comms inbox reply, else
#                     runs the reflect cadence, else posts a close note. This
#                     REPLACES any existing sentry-reflect Stop hook (wake reuses
#                     the reflect cadence, so two Stop hooks are not needed).
#   * sentry-deploy — PostToolUse (Bash) hook: posts deploy/merge evidence to comms.
# Both are NO-OPs unless SENTRY_MCP_URL is configured.
#
# Usage:
#   ./install-comms-hooks.sh --build                          # build both from THIS repo (needs go)
#   ./install-comms-hooks.sh --wake-bin ./sentry-wake --deploy-bin ./sentry-deploy
#   ./install-comms-hooks.sh --target codex --build           # register in Codex instead of Claude Code
#   ./install-comms-hooks.sh --build --url https://mcp.blazesphere.net/mcp   # also set the MCP URL
#
# Flags:
#   --target claude|codex   which harness config to merge into (default claude)
#   --build                 build sentry-wake+sentry-deploy from ./cmd (repo root, needs go)
#   --wake-bin PATH         use a prebuilt sentry-wake binary
#   --deploy-bin PATH       use a prebuilt sentry-deploy binary
#   --url URL               write SENTRY_MCP_URL to ~/.matrix-sentry.env if absent
#
# Requires: jq. For --build: go + this repo. Works on macOS and Linux.
set -euo pipefail

WAKE_DEST="${SENTRY_WAKE_BIN:-$HOME/.local/bin/sentry-wake}"
DEPLOY_DEST="${SENTRY_DEPLOY_BIN:-$HOME/.local/bin/sentry-deploy}"
CLAUDE_SETTINGS_FILE="${CLAUDE_SETTINGS:-$HOME/.claude/settings.json}"
CODEX_HOOKS_FILE="${CODEX_HOOKS:-$HOME/.codex/hooks.json}"
ENVF="$HOME/.matrix-sentry.env"
STATUS_MSG="matrix-sentry: checking comms inbox"

TARGET="claude"; URL=""; DO_BUILD=0; WAKE_PREBUILT=""; DEPLOY_PREBUILT=""

usage() {
  sed -n '2,36p' "$0" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
  case "$1" in
    --target)     TARGET="${2:?--target needs claude|codex}"; shift 2;;
    --url)        URL="${2:?--url needs a value}"; shift 2;;
    --build)      DO_BUILD=1; shift;;
    --wake-bin)   WAKE_PREBUILT="${2:?--wake-bin needs a path}"; shift 2;;
    --deploy-bin) DEPLOY_PREBUILT="${2:?--deploy-bin needs a path}"; shift 2;;
    -h|--help)    usage; exit 0;;
    *) echo "unknown arg: $1" >&2; usage >&2; exit 2;;
  esac
done

case "$TARGET" in
  claude|codex) ;;
  *) echo "error: --target must be claude or codex (got: $TARGET)" >&2; exit 2;;
esac

command -v jq >/dev/null 2>&1 || { echo "error: jq is required" >&2; exit 1; }
mkdir -p "$(dirname "$WAKE_DEST")" "$(dirname "$DEPLOY_DEST")"

# --- 1. binaries -----------------------------------------------------------
# place_bin <cmd-name> <flag-label> <prebuilt-path> <dest>
place_bin() {
  cmd="$1"; label="$2"; prebuilt="$3"; dest="$4"
  if [ -n "$prebuilt" ]; then
    [ -f "$prebuilt" ] || { echo "error: --$label path not found: $prebuilt" >&2; exit 1; }
    install -m 0755 "$prebuilt" "$dest"
    echo "installed $cmd from $prebuilt -> $dest"
  elif [ "$DO_BUILD" = 1 ]; then
    command -v go >/dev/null 2>&1 || { echo "error: --build needs go" >&2; exit 1; }
    [ -d "./cmd/$cmd" ] || { echo "error: run --build from the matrix-sentry repo root" >&2; exit 1; }
    go build -trimpath -o "$dest" "./cmd/$cmd"
    chmod +x "$dest"
    echo "built $cmd -> $dest"
  elif [ -x "$dest" ]; then
    echo "$cmd already present at $dest (pass --$label or --build to replace)"
  else
    echo "error: no $cmd binary. Pass --$label <path> or --build (from the repo)." >&2
    exit 1
  fi
}

place_bin sentry-wake   wake-bin   "$WAKE_PREBUILT"   "$WAKE_DEST"
place_bin sentry-deploy deploy-bin "$DEPLOY_PREBUILT" "$DEPLOY_DEST"

# --- 2. hook config (idempotent jq merge, atomic write) --------------------
# Merges the same {hooks:{Stop,PostToolUse}} shape into whichever harness file we
# target. Stop -> sentry-wake, dropping any Stop group whose command ends in
# "sentry-reflect" (the swap). PostToolUse += a Bash matcher group running
# sentry-deploy (async). Idempotent by command path.
merge_hooks() {
  file="$1"
  mkdir -p "$(dirname "$file")"
  [ -f "$file" ] || echo '{"hooks":{}}' > "$file"
  jq -e . "$file" >/dev/null 2>&1 || { echo "error: $file is not valid JSON; fix it first" >&2; exit 1; }

  tmp="$(mktemp "${file}.XXXXXX")"
  jq --arg wake "$WAKE_DEST" --arg deploy "$DEPLOY_DEST" --arg msg "$STATUS_MSG" '
    .hooks = (.hooks // {})
    | .hooks.Stop = (
        ( (.hooks.Stop // [])
          | map(select( any(.hooks[]?; (.command // "") | endswith("sentry-reflect")) | not )) ) as $s
        | if any($s[]?.hooks[]?; .command == $wake) then $s
          else $s + [{"hooks":[{"type":"command","command":$wake,"timeout":8,"statusMessage":$msg,"async":false}]}]
          end
      )
    | .hooks.PostToolUse = (
        (.hooks.PostToolUse // [])
        | if any(.[]?.hooks[]?; .command == $deploy) then .
          else . + [{"matcher":"Bash","hooks":[{"type":"command","command":$deploy,"async":true}]}]
          end
      )
  ' "$file" > "$tmp"
  jq -e . "$tmp" >/dev/null 2>&1 || { echo "error: merge produced invalid JSON, aborting" >&2; rm -f "$tmp"; exit 1; }
  mv "$tmp" "$file"
  echo "wake (Stop) + deploy (PostToolUse) hooks present in $file"
}

if [ "$TARGET" = claude ]; then
  merge_hooks "$CLAUDE_SETTINGS_FILE"
else
  merge_hooks "$CODEX_HOOKS_FILE"
fi

# --- 3. SENTRY_MCP_URL (else the hooks are no-ops) -------------------------
if [ -n "$URL" ]; then
  if [ -f "$ENVF" ] && grep -q '^SENTRY_MCP_URL=' "$ENVF"; then
    echo "SENTRY_MCP_URL already in $ENVF (left as-is)"
  else
    echo "SENTRY_MCP_URL=$URL" >> "$ENVF"
    echo "wrote SENTRY_MCP_URL -> $ENVF"
  fi
fi
if [ -z "${SENTRY_MCP_URL:-}" ] && ! grep -qs '^SENTRY_MCP_URL=' "$ENVF"; then
  echo "WARNING: SENTRY_MCP_URL is not set (env or $ENVF). The hooks will be NO-OPs"
  echo "         until you set it. Re-run with:  --url <your-mcp-url>"
fi

# --- 4. verify each binary is harness-safe (exit 0 on empty input) ---------
for b in "$WAKE_DEST" "$DEPLOY_DEST"; do
  if printf '{}' | "$b" >/dev/null 2>&1; then
    echo "verify: $b runs cleanly (exit 0)."
  else
    echo "WARNING: $b exited non-zero on empty input — check the build."
  fi
done

# --- 5. next steps ---------------------------------------------------------
echo
if [ "$TARGET" = codex ]; then
  echo "Codex: enable hooks in ~/.codex/config.toml (they will not fire otherwise):"
  echo "  [features]"
  echo "  hooks = true"
  echo
  echo "Done. Restart Codex so it reloads $CODEX_HOOKS_FILE."
else
  echo "Done. Open /hooks once (or restart Claude Code) so the settings watcher loads the hooks."
fi
echo "sentry-wake wakes the turn on a new comms reply (Stop); sentry-deploy posts deploy evidence (PostToolUse)."
echo "Ensure the matrix-sentry MCP server is connected on this machine (SENTRY_MCP_URL)."
