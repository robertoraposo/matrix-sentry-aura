#!/bin/sh
# Matrix Sentry — one-shot installer for the sentry-record PostToolUse hook.
#   curl -fsSL http://10.10.10.96:8810/install.sh | SENTRY_MCP_TOKEN=<token> sh
# Installs the hook binary (integrity-pinned), writes the env, registers the MCP
# server, and wires the global PostToolUse hook. Idempotent; safe to re-run.
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

# Pinned binary integrity (sha256 of the artifacts built from this commit). The
# download is over plain HTTP, so verify the bytes before trusting/executing
# them — defends against a tampered/MITM'd binary on the no-auth endpoint.
SHA_AMD64="5e7ec94e35ca5c5d72b6bc7811104a0f06e1874312a35534d308284f7b7096c2"
SHA_ARM64="c0ae07e52b598e44629fd5f56452e4d937b04bb482590eb992fec89b890431d0"

ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64)   BIN_REMOTE=sentry-record.linux-amd64; WANT=$SHA_AMD64 ;;
  aarch64|arm64)  BIN_REMOTE=sentry-record.linux-arm64; WANT=$SHA_ARM64 ;;
  *) echo "matrix-sentry: unsupported arch '$ARCH'"; exit 1 ;;
esac

BIN="$HOME/.local/bin/sentry-record"
mkdir -p "$HOME/.local/bin"
echo "matrix-sentry: downloading $BIN_REMOTE ..."
curl -fsSL "$BASE/$BIN_REMOTE" -o "$BIN.tmp"

# verify checksum (sha256sum on Linux, shasum -a 256 elsewhere)
if command -v sha256sum >/dev/null 2>&1; then
  GOT=$(sha256sum "$BIN.tmp" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  GOT=$(shasum -a 256 "$BIN.tmp" | awk '{print $1}')
else
  rm -f "$BIN.tmp"; echo "matrix-sentry: no sha256 tool available; aborting"; exit 1
fi
if [ "$GOT" != "$WANT" ]; then
  rm -f "$BIN.tmp"
  echo "matrix-sentry: CHECKSUM MISMATCH — refusing to install."
  echo "  expected $WANT"
  echo "  got      $GOT"
  exit 1
fi
mv "$BIN.tmp" "$BIN"
chmod +x "$BIN"
echo "  checksum OK."

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
    print("  hook added.")
PY
else
  echo "matrix-sentry: python3 not found — add this to $SETTINGS manually:"
  echo '  "hooks":{"PostToolUse":[{"matcher":"Read|Edit|Write|MultiEdit|NotebookEdit|Grep|Glob|Bash","hooks":[{"type":"command","command":"'"$BIN"'","async":true}]}]}'
fi

echo ""
echo "matrix-sentry: DONE."
echo "  -> Open /hooks once (or restart Claude Code) to activate the hook."
echo "  -> Health check: curl -s http://10.10.10.96:8808/   (expect: matrix-sentry mcp ok)"
