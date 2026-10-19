#!/usr/bin/env bash
# Writes the Scoop manifest for one release to stdout (#261):
#
#   scripts/scoop-manifest.sh v1.2.3 dist/checksums.txt > janus.json
#   scripts/scoop-manifest.sh v1.2.3 dist/checksums.txt http://127.0.0.1:8901 > janus.json
#
# The manifest installs the release's own archives, with their sums from
# checksums.txt (ADR-004). The third argument replaces the release's
# download URL, so that a pull request can install the manifest from the
# archives it has just built. The release workflow writes the manifest
# into risadams/scoop-bucket on each stable tag.
set -euo pipefail

usage="usage: scripts/scoop-manifest.sh <version> <checksums.txt> [base URL]"
version="${1:?$usage}"
sums="${2:?$usage}"
v="${version#v}"
base="${3:-https://github.com/risadams/Janus/releases/download/${version}}"

sum() {
  local s
  s="$(awk -v n="$1" '$2 == n || $2 == "*" n { print $1 }' "$sums")"
  [ -n "$s" ] || { echo "scoop-manifest.sh: $1 is not in ${sums}" >&2; exit 1; }
  echo "$s"
}

amd64="janus_${v}_windows_amd64"
arm64="janus_${v}_windows_arm64"
# "##" is Scoop's comment field: it is not shown to the person installing.
jq -n --arg v "$v" --arg base "$base" \
  --arg amd64 "$amd64" --arg amd64sum "$(sum "${amd64}.zip")" \
  --arg arm64 "$arm64" --arg arm64sum "$(sum "${arm64}.zip")" '{
  version: $v,
  description: "A local proxy for Claude Code that tracks cost and keeps secrets local.",
  homepage: "https://risadams.github.io/Janus/",
  license: "MIT",
  "##": "Written by the Janus release workflow (scripts/scoop-manifest.sh in risadams/Janus); a change made here is lost at the next release.",
  architecture: {
    "64bit": { url: "\($base)/\($amd64).zip", hash: $amd64sum, extract_dir: $amd64 },
    arm64: { url: "\($base)/\($arm64).zip", hash: $arm64sum, extract_dir: $arm64 }
  },
  bin: "janus.exe"
}'
