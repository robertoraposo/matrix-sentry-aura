#!/usr/bin/env bash
# get_corpus.sh — downloads a diverse text corpus from Project Gutenberg for
# the Scenario B real-embeddings benchmark (cmd/sembed). ~7 MB of public domain
# literature across 6 genres: romance, detective, gothic, philosophy, science,
# historical fiction.
#
# Usage:   ./get_corpus.sh [DEST_DIR]
# Default DEST_DIR=/data/sembed/corpus
set -euo pipefail

DEST="${1:-/data/sembed/corpus}"
mkdir -p "$DEST"
cd "$DEST"

declare -A BOOKS=(
  [pride.txt]="https://www.gutenberg.org/files/1342/1342-0.txt"
  [sherlock.txt]="https://www.gutenberg.org/files/1661/1661-0.txt"
  [frankenstein.txt]="https://www.gutenberg.org/files/84/84-0.txt"
  [republic.txt]="https://www.gutenberg.org/files/1497/1497-0.txt"
  [origin.txt]="https://www.gutenberg.org/files/1228/1228-0.txt"
  [warandpeace.txt]="https://www.gutenberg.org/files/2600/2600-0.txt"
)

count=0
for fname in "${!BOOKS[@]}"; do
  if [[ -s "$fname" ]]; then
    echo "[ok] $fname already present"
  else
    echo "[*] Downloading $fname..."
    curl -fSL --retry 3 -o "$fname" "${BOOKS[$fname]}"
  fi
  ((count++))
done

echo
echo "[ok] Corpus ready in $DEST ($count books):"
ls -la *.txt
echo
echo "Run:  go run ./cmd/sembed -dir /data/sembed -corpus $DEST"
