#!/usr/bin/env bash
# Builds the release archives for one version into dist/:
#
#   scripts/release.sh v1.2.3
#
# One archive per platform, each holding the janus binary, the README and
# the licence, and a checksums.txt of SHA-256 sums over the archives. The
# binaries are static and carry the version given. Every installed Janus
# updates itself from releases laid out this way, so the names and the
# layout are fixed (docs/adr/004-release-tooling.md), and the release
# workflow checks them with scripts/checklayout.
set -euo pipefail

version="${1:?usage: scripts/release.sh <version>, for example v1.2.3}"
targets=(darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64)

cd "$(dirname "$0")/.."
rm -rf dist
mkdir -p dist

for target in "${targets[@]}"; do
  os="${target%/*}"
  arch="${target#*/}"
  name="janus_${version#v}_${os}_${arch}"
  stage="dist/${name}"
  binary="janus"
  if [ "$os" = "windows" ]; then binary="janus.exe"; fi

  mkdir -p "$stage"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X main.version=${version}" -o "${stage}/${binary}" ./cmd/janus
  cp README.md LICENSE "$stage/"

  if [ "$os" = "windows" ]; then
    (cd dist && zip -q -r "${name}.zip" "$name")
  else
    tar -C dist -czf "dist/${name}.tar.gz" "$name"
  fi
  rm -rf "$stage"
  echo "built ${name}"
done

cd dist
if command -v sha256sum >/dev/null; then
  sha256sum janus_* > checksums.txt
else
  shasum -a 256 janus_* > checksums.txt
fi
cat checksums.txt
