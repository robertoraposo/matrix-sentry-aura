#!/usr/bin/env bash
# Matrix Sentry — one-line installer for the auto-remember Stop hook.
#   curl -fsSL https://mcp.blazesphere.net/install.sh | bash -s -- --token <SENTRY_MCP_TOKEN>
#   (or safer, keeps the token out of shell history:)
#   SENTRY_MCP_TOKEN=xxx curl -fsSL https://mcp.blazesphere.net/install.sh | bash
#
# Downloads the signed sentry-reflect binary for this OS/arch, VERIFIES its
# ECDSA signature against the embedded public key (aborts on mismatch), installs
# the Stop hook into ~/.claude/settings.json, sets SENTRY_MCP_URL, and connects
# the matrix-sentry MCP server. Requires: curl, openssl, jq (claude CLI for the
# MCP connect step). Idempotent.
set -euo pipefail

BASE="${SENTRY_BASE_URL:-https://mcp.blazesphere.net}"
URL="${SENTRY_MCP_URL:-$BASE/mcp}"
TOKEN="${SENTRY_MCP_TOKEN:-}"
BIN_DEST="${SENTRY_REFLECT_BIN:-$HOME/.local/bin/sentry-reflect}"
SETTINGS="${CLAUDE_SETTINGS:-$HOME/.claude/settings.json}"
ENVF="$HOME/.matrix-sentry.env"
STATUS_MSG="matrix-sentry: reflecting on durable knowledge"

while [ $# -gt 0 ]; do
  case "$1" in
    --token) TOKEN="${2:?}"; shift 2;;
    --url)   URL="${2:?}"; shift 2;;
    --base)  BASE="${2:?}"; shift 2;;
    -h|--help) sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done

for t in curl openssl jq; do command -v "$t" >/dev/null 2>&1 || { echo "error: '$t' is required" >&2; exit 1; }; done

# embedded ECDSA P-256 public key (trust anchor for the binary signature)
PUBKEY='-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEFtomzIQnPOzqvtDeINLdFdsxdZWS
sKc0EvVfpZc9zpkX5L+nGxQINx8+HCoycpK2oLYZtnVN5SLyMz3Do8JAEA==
-----END PUBLIC KEY-----'

# 1. detect os/arch -> artifact name
os=$(uname -s); arch=$(uname -m)
case "$os" in Darwin) os=darwin;; Linux) os=linux;; *) echo "unsupported OS: $os" >&2; exit 1;; esac
case "$arch" in arm64|aarch64) arch=arm64;; x86_64|amd64) arch=amd64;; *) echo "unsupported arch: $arch" >&2; exit 1;; esac
artifact="sentry-reflect-$os-$arch"

# 2. download binary + signature
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "downloading $artifact ..."
curl -fsSL "$BASE/dl/$artifact"     -o "$tmp/bin"
curl -fsSL "$BASE/dl/$artifact.sig" -o "$tmp/bin.sig"

# 3. verify signature (abort on mismatch)
printf '%s\n' "$PUBKEY" > "$tmp/pub.pem"
if openssl dgst -sha256 -verify "$tmp/pub.pem" -signature "$tmp/bin.sig" "$tmp/bin" >/dev/null 2>&1; then
  echo "signature: Verified OK"
else
  echo "error: signature verification FAILED — refusing to install" >&2
  exit 1
fi

# 4. install binary
mkdir -p "$(dirname "$BIN_DEST")"
install -m 0755 "$tmp/bin" "$BIN_DEST"
echo "installed -> $BIN_DEST"

# 5. merge Stop hook into settings.json (idempotent; validate before replacing)
mkdir -p "$(dirname "$SETTINGS")"
[ -f "$SETTINGS" ] || echo '{}' > "$SETTINGS"
jq -e . "$SETTINGS" >/dev/null 2>&1 || { echo "error: $SETTINGS is not valid JSON; fix it first" >&2; exit 1; }
st=$(mktemp "${SETTINGS}.XXXXXX")
jq --arg cmd "$BIN_DEST" --arg msg "$STATUS_MSG" '
  .hooks = (.hooks // {})
  | .hooks.Stop = ((.hooks.Stop // []) as $s
      | if any($s[]?.hooks[]?; .command == $cmd) then $s
        else $s + [{"hooks":[{"type":"command","command":$cmd,"timeout":8,"statusMessage":$msg,"async":false}]}]
        end)
' "$SETTINGS" > "$st"
jq -e . "$st" >/dev/null 2>&1 || { echo "error: settings merge produced invalid JSON, aborting" >&2; rm -f "$st"; exit 1; }
mv "$st" "$SETTINGS"
echo "Stop hook present in $SETTINGS"

# 6. SENTRY_MCP_URL (else the hook is a no-op)
if [ -f "$ENVF" ] && grep -q '^SENTRY_MCP_URL=' "$ENVF"; then :; else echo "SENTRY_MCP_URL=$URL" >> "$ENVF"; fi

# 7. connect the matrix-sentry MCP server (needs the claude CLI + a token)
if command -v claude >/dev/null 2>&1; then
  if [ -n "$TOKEN" ]; then
    claude mcp remove -s user matrix-sentry >/dev/null 2>&1 || true
    if claude mcp add -s user --transport http matrix-sentry "$URL" --header "Authorization: Bearer $TOKEN" >/dev/null 2>&1; then
      echo "connected MCP server 'matrix-sentry' -> $URL"
    else
      echo "WARNING: 'claude mcp add' failed — connect manually (check 'claude mcp list')."
    fi
  else
    echo "no --token / SENTRY_MCP_TOKEN given — skipped MCP connect (hook installed; connect the server manually)."
  fi
else
  echo "no 'claude' CLI on PATH — skipped MCP connect (install Claude Code, then re-run or connect manually)."
fi

# 8. verify + next step
printf '{}' | "$BIN_DEST" >/dev/null 2>&1 && echo "verify: sentry-reflect runs cleanly." || echo "WARNING: sentry-reflect exited non-zero on empty input."
echo
echo "Done. Open /hooks once (or restart Claude Code) so the settings watcher loads the hook."
