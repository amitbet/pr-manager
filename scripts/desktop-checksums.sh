#!/usr/bin/env bash
# Adds the given desktop packages' lines to the release's
# desktop-checksums.txt, keeping the lines other jobs published. The macOS and
# Linux packages and the Windows one are published by separate jobs, so
# either may come first; the jobs share a concurrency group so they do not
# overwrite each other.
# Usage: desktop-checksums.sh TAG PATH...
set -euo pipefail
tag=$1
shift
out=$(mktemp -d)
sums="$out/desktop-checksums.txt"
: > "$sums"
if gh release view "$tag" --json assets --jq '.assets[].name' | grep -qxF desktop-checksums.txt; then
  gh release download "$tag" -p desktop-checksums.txt -D "$out" --clobber
fi
for f in "$@"; do
  name=$(basename "$f")
  hash=$(sha256sum "$f" | cut -d' ' -f1)
  awk -v n="$name" '$2 != n' "$sums" > "$sums.new"
  echo "$hash  $name" >> "$sums.new"
  mv "$sums.new" "$sums"
done
sort -k2 -o "$sums" "$sums"
cat "$sums"
gh release upload "$tag" "$sums" --clobber
