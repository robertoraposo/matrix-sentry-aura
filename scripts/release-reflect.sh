#!/usr/bin/env bash
# release-reflect.sh — cross-compile the 4 sentry-reflect binaries, ship to the
# VM, sign each with /root/sentry-sign.key, and place install.sh. Run from repo root.
# The private signing key lives ONLY on the VM (/root/sentry-sign.key, 0600) and
# is never committed or served. deploy/sentry-dl/install.sh embeds the PUBLIC key.
set -euo pipefail
VM="${VM:-matrix-sentry}"; DL="/root/sentry-dl"
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
  os=${t%/*}; arch=${t#*/}
  GOOS=$os GOARCH=$arch go build -trimpath -o "$tmp/sentry-reflect-$os-$arch" ./cmd/sentry-reflect
done
ssh "$VM" "mkdir -p $DL"
scp "$tmp"/sentry-reflect-* deploy/sentry-dl/install.sh "$VM:$DL/"
ssh "$VM" "cd $DL && for b in sentry-reflect-*; do case \$b in *.sig) continue;; esac; openssl dgst -sha256 -sign /root/sentry-sign.key -out \$b.sig \$b; openssl dgst -sha256 -verify /root/sentry-sign.pub -signature \$b.sig \$b; done"
echo "released to $VM:$DL"
