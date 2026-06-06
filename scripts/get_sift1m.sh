#!/usr/bin/env bash
# get_sift1m.sh — descarga el benchmark SIFT1M (descriptores reales + ground
# truth de INRIA) para correr cmd/sift1m. ~168 MB comprimido / ~580 MB extraido.
#
# Uso:   ./get_sift1m.sh [DEST_DIR]
# Default DEST_DIR=/data/sift
set -euo pipefail

DEST="${1:-/data/sift}"
mkdir -p "$DEST"
cd "$DEST"

have_all() {
  [[ -s sift_base.fvecs && -s sift_learn.fvecs && -s sift_query.fvecs && -s sift_groundtruth.ivecs ]]
}

if have_all; then
  echo "[ok] SIFT1M ya esta en $DEST"
  exit 0
fi

echo "[*] Intentando tar unico (fzliu/sift1m, ~168 MB)..."
if curl -fSL --retry 3 -o sift.tar.gz \
    "https://huggingface.co/datasets/fzliu/sift1m/resolve/main/sift.tar.gz"; then
  tar -xzf sift.tar.gz
  # el tar extrae en ./sift/ ; sube los archivos a DEST
  if [[ -d sift ]]; then mv -f sift/* . && rmdir sift 2>/dev/null || true; fi
  rm -f sift.tar.gz
fi

if ! have_all; then
  echo "[*] Fallback: archivos sueltos (qbo-odp/sift1m via Git LFS)..."
  BASE="https://huggingface.co/datasets/qbo-odp/sift1m/resolve/main"
  for f in sift_base.fvecs sift_learn.fvecs sift_query.fvecs sift_groundtruth.ivecs; do
    [[ -s "$f" ]] || curl -fSL --retry 3 -o "$f" "$BASE/$f"
  done
fi

if have_all; then
  echo "[ok] Listo en $DEST:"
  ls -la sift_*.fvecs sift_*.ivecs
  echo
  echo "Correr:  go build -o sift1m ./cmd/sift1m && ./sift1m -dir $DEST -prefix sift -m 8 -k 256 -iter 25"
else
  echo "[ERROR] No se pudieron obtener todos los archivos." >&2
  echo "Alternativa manual (corpus TEXMEX original):" >&2
  echo "  wget ftp://ftp.irisa.fr/local/texmex/corpus/sift.tar.gz" >&2
  exit 1
fi
