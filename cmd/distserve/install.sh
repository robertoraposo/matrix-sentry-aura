#!/bin/sh
# Matrix Sentry — one-shot installer for the sentry-record PostToolUse hook.
#   curl -fsSL http://10.10.10.96:8810/install.sh | sh
# Installs the hook binary, writes the env (URL+token), registers the MCP server,
# and wires the global PostToolUse hook. Idempotent; safe to re-run.
set -e

BASE="http://10.10.10.96:8810"
MCP_URL="http://10.10.10.96:8808/mcp"
# Token is NOT published with this script — pass it inline so it never sits on a
# no-auth URL:   curl -fsSL .../install.sh | SENTRY_MCP_TOKEN=xxxx sh
TOKEN="${SENTRY_MCP_TOKEN:-}"
if [ -z "$TOKEN" ]; then
  echo "matrix-sentry: set SENTRY_MCP_TOKEN, e.g.:"
  echo "  curl -fsSL $BASE/install.sh | SENTRY_MCP_TOKEN=<token> sh"
  exit 1
fi

ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64)   BIN_REMOTE=sentry-record.linux-amd64 ;;
  aarch64|arm64)  BIN_REMOTE=sentry-record.linux-arm64 ;;
  *) echo "matrix-sentry: unsupported arch '$ARCH'"; exit 1 ;;
esac

BIN="$HOME/.local/bin/sentry-record"
mkdir -p "$HOME/.local/bin"
echo "matrix-sentry: downloading $BIN_REMOTE ..."
curl -fsSL "$BASE/$BIN_REMOTE" -o "$BIN"
chmod +x "$BIN"

echo "matrix-sentry: writing ~/.matrix-sentry.env (chmod 600) ..."
umask 177
cat > "$HOME/.matrix-sentry.env" <<EOF
SENTRY_MCP_URL=$MCP_URL
SENTRY_MCP_TOKEN=$TOKEN
EOF

if command -v claude >/dev/null 2>&1; then
  echo "matrix-sentry: registering MCP server (user scope) ..."
  claude mcp add --transport http matrix-sentry "$MCP_URL" \
    --header "Authorization: Bearer $TOKEN" -s user >/dev/null 2>&1 \
    && echo "  MCP registered." \
    || echo "  MCP add skipped (already registered or CLI busy)."
else
  echo "matrix-sentry: 'claude' CLI not found — skipping MCP registration."
fi

SETTINGS="$HOME/.claude/settings.json"
if command -v python3 >/dev/null 2>&1; then
  echo "matrix-sentry: wiring PostToolUse hook into $SETTINGS ..."
  python3 - "$SETTINGS" "$BIN" <<'PY'
import json, os, sys
path, binp = sys.argv[1], sys.argv[2]
os.makedirs(os.path.dirname(path), exist_ok=True)
d = json.load(open(path)) if os.path.exists(path) else {}
ptu = d.setdefault("hooks", {}).setdefault("PostToolUse", [])
already = any(any(h.get("command") == binp for h in e.get("hooks", [])) for e in ptu)
if already:
    print("  hook already present.")
else:
    if os.path.exists(path):
        json.dump(d, open(path + ".bak", "w"), indent=2)
    ptu.append({
        "matcher": "Read|Edit|Write|MultiEdit|NotebookEdit|Grep|Glob|Bash",
        "hooks": [{"type": "command", "command": binp, "async": True}],
    })
    json.dump(d, open(path, "w"), indent=2)
    print("  hook added (backup at %s.bak)." % path if os.path.exists(path + ".bak") else "  hook added.")
PY
else
  echo "matrix-sentry: python3 not found — add this to $SETTINGS manually:"
  echo '  "hooks":{"PostToolUse":[{"matcher":"Read|Edit|Write|MultiEdit|NotebookEdit|Grep|Glob|Bash","hooks":[{"type":"command","command":"'"$BIN"'","async":true}]}]}'
fi

echo ""
echo "matrix-sentry: DONE."
echo "  -> Open /hooks once (or restart Claude Code) to activate the hook."
echo "  -> Health check: curl -s http://10.10.10.96:8808/   (expect: matrix-sentry mcp ok)"
