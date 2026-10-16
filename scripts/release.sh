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
#
# The same tag builds the same bytes on any machine (#264): the Go
# toolchain is the exact one go.mod names, the build records no paths,
# build ID, or VCS state, and scripts/archive dates every file at the
# commit's time. SOURCE_DATE_EPOCH, in seconds, overrides that time.
set -euo pipefail

version="${1:?usage: scripts/release.sh <version>, for example v1.2.3}"
targets=(darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64)

cd "$(dirname "$0")/.."

toolchain="$(sed -n 's/^toolchain //p' go.mod)"
[ -n "$toolchain" ] || { echo "release.sh: go.mod names no toolchain" >&2; exit 1; }
export GOTOOLCHAIN="$toolchain" GOFLAGS="" CGO_ENABLED=0 LC_ALL=C
export SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct)}"

rm -rf dist
mkdir -p dist
tools="$(mktemp -d)"
trap 'rm -rf "$tools"' EXIT
go build -o "$tools/archive" ./scripts/archive

for target in "${targets[@]}"; do
  os="${target%/*}"
  arch="${target#*/}"
  name="janus_${version#v}_${os}_${arch}"
  stage="dist/${name}"
  binary="janus"
  if [ "$os" = "windows" ]; then binary="janus.exe"; fi

  mkdir -p "$stage"
  GOOS="$os" GOARCH="$arch" go build -trimpath -buildvcs=false \
    -ldflags "-s -w -buildid= -X main.version=${version}" -o "${stage}/${binary}" ./cmd/janus
  cp README.md LICENSE "$stage/"

  ext=tar.gz
  if [ "$os" = "windows" ]; then ext=zip; fi
  "$tools/archive" -o "dist/${name}.${ext}" "$stage"
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
