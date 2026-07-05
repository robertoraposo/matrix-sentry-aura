#!/usr/bin/env bash
# install-reflect-hook.sh — replicate Matrix Sentry's auto-remember Stop hook
# onto a Claude Code install (any machine). Idempotent: safe to re-run.
#
# It installs the `sentry-reflect` binary (see cmd/sentry-reflect), then merges a
# Stop hook into ~/.claude/settings.json that runs it. Every 40 tool-uses the
# hook injects the "reflect + save durable memory" nudge (the auto-remember
# cycle). The hook is a NO-OP unless SENTRY_MCP_URL is configured.
#
# Usage:
#   ./install-reflect-hook.sh --build              # build from THIS repo (needs go)
#   ./install-reflect-hook.sh --bin /path/to/sentry-reflect   # use a prebuilt binary
#   ./install-reflect-hook.sh --url https://mcp.blazesphere.net/mcp   # also set the MCP URL
# Flags combine, e.g.:  ./install-reflect-hook.sh --build --url https://mcp.blazesphere.net/mcp
#
# Requires: jq. For --build: go + this repo. Works on macOS and Linux.
set -euo pipefail

BIN_DEST="${SENTRY_REFLECT_BIN:-$HOME/.local/bin/sentry-reflect}"
SETTINGS="${CLAUDE_SETTINGS:-$HOME/.claude/settings.json}"
ENVF="$HOME/.matrix-sentry.env"
STATUS_MSG="matrix-sentry: reflecting on durable knowledge"

URL=""; PREBUILT=""; DO_BUILD=0

usage() {
  sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
  case "$1" in
    --url)   URL="${2:?--url needs a value}"; shift 2;;
    --bin)   PREBUILT="${2:?--bin needs a path}"; shift 2;;
    --build) DO_BUILD=1; shift;;
    -h|--help) usage; exit 0;;
    *) echo "unknown arg: $1" >&2; usage >&2; exit 2;;
  esac
done

command -v jq >/dev/null 2>&1 || { echo "error: jq is required" >&2; exit 1; }
mkdir -p "$(dirname "$BIN_DEST")"

# --- 1. binary -------------------------------------------------------------
if [ -n "$PREBUILT" ]; then
  [ -f "$PREBUILT" ] || { echo "error: --bin path not found: $PREBUILT" >&2; exit 1; }
  install -m 0755 "$PREBUILT" "$BIN_DEST"
  echo "installed binary from $PREBUILT -> $BIN_DEST"
elif [ "$DO_BUILD" = 1 ]; then
  command -v go >/dev/null 2>&1 || { echo "error: --build needs go" >&2; exit 1; }
  [ -d ./cmd/sentry-reflect ] || { echo "error: run --build from the matrix-sentry repo root" >&2; exit 1; }
  go build -trimpath -o "$BIN_DEST" ./cmd/sentry-reflect
  chmod +x "$BIN_DEST"
  echo "built sentry-reflect -> $BIN_DEST"
elif [ -x "$BIN_DEST" ]; then
  echo "binary already present at $BIN_DEST (pass --bin or --build to replace)"
else
  echo "error: no binary. Pass --bin <path> or --build (from the repo)." >&2
  exit 1
fi

# --- 2. Stop hook in settings.json (idempotent merge) ----------------------
mkdir -p "$(dirname "$SETTINGS")"
[ -f "$SETTINGS" ] || echo '{}' > "$SETTINGS"
jq -e . "$SETTINGS" >/dev/null 2>&1 || { echo "error: $SETTINGS is not valid JSON; fix it first" >&2; exit 1; }

tmp="$(mktemp "${SETTINGS}.XXXXXX")"
jq --arg cmd "$BIN_DEST" --arg msg "$STATUS_MSG" '
  .hooks = (.hooks // {})
  | .hooks.Stop = ((.hooks.Stop // []) as $s
      | if any($s[]?.hooks[]?; .command == $cmd) then $s
        else $s + [{"hooks":[{"type":"command","command":$cmd,"timeout":8,"statusMessage":$msg,"async":false}]}]
        end)
' "$SETTINGS" > "$tmp"
jq -e . "$tmp" >/dev/null 2>&1 || { echo "error: merge produced invalid JSON, aborting" >&2; rm -f "$tmp"; exit 1; }
mv "$tmp" "$SETTINGS"
echo "Stop hook present in $SETTINGS"

# --- 3. SENTRY_MCP_URL (else the hook is a no-op) --------------------------
if [ -n "$URL" ]; then
  if [ -f "$ENVF" ] && grep -q '^SENTRY_MCP_URL=' "$ENVF"; then
    echo "SENTRY_MCP_URL already in $ENVF (left as-is)"
  else
    echo "SENTRY_MCP_URL=$URL" >> "$ENVF"
    echo "wrote SENTRY_MCP_URL -> $ENVF"
  fi
fi
if [ -z "${SENTRY_MCP_URL:-}" ] && ! grep -qs '^SENTRY_MCP_URL=' "$ENVF"; then
  echo "WARNING: SENTRY_MCP_URL is not set (env or $ENVF). The hook will be a NO-OP"
  echo "         until you set it. Re-run with:  --url <your-mcp-url>"
fi

# --- 4. verify -------------------------------------------------------------
if printf '{}' | "$BIN_DEST" >/dev/null 2>&1; then
  echo "verify: $BIN_DEST runs cleanly (exit 0)."
else
  echo "WARNING: $BIN_DEST exited non-zero on empty input — check the build."
fi

echo
echo "Done. Open /hooks once (or restart Claude Code) so the settings watcher loads the hook."
echo "The hook fires the auto-remember nudge every 40 tool-uses; ensure the matrix-sentry MCP server is connected on this machine."
