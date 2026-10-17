#!/usr/bin/env bash
# Runs gosec, the Go security checker, over Janus with the arguments given:
#
#   scripts/gosec.sh ./...
#   scripts/gosec.sh -fmt sarif -out gosec.sarif -no-fail ./...
#
# gosec v2.29.0 cannot read the export data Go 1.27 writes
# (securego/gosec#1774), so run with `go run ...@v2.29.0` it fails to
# type-check every package and silently runs only its syntax rules. This
# builds it against a newer golang.org/x/tools instead, in a module of its
# own outside the repository, so nothing is added to Janus's go.mod
# (docs/adr/001-dependencies.md). Both versions are pinned, and go.sum is
# checked against the Go checksum database as for any module. Once a gosec
# release fixes #1774, replace all of this with
# `go run github.com/securego/gosec/v2/cmd/gosec@<that version>`, as
# staticcheck and govulncheck are run.
#
# The binary is kept under the user's cache directory, so only the first
# run builds it.
set -euo pipefail

gosec_version=v2.29.0
tools_version=v0.52.0

cd "$(dirname "$0")/.."

cache="${XDG_CACHE_HOME:-$HOME/.cache}/janus-gosec/${gosec_version}-tools-${tools_version}"
bin="$cache/gosec"
if [ ! -x "$bin" ]; then
  build="$(mktemp -d)"
  trap 'rm -rf "$build"' EXIT
  (
    cd "$build"
    export GOFLAGS=-mod=mod
    go mod init gosec-build >/dev/null 2>&1
    go get "github.com/securego/gosec/v2/cmd/gosec@${gosec_version}" "golang.org/x/tools@${tools_version}" >/dev/null 2>&1 \
      || { echo "gosec.sh: could not fetch gosec ${gosec_version} with x/tools ${tools_version}" >&2; exit 1; }
    mkdir -p "$cache"
    go build -o "$bin" github.com/securego/gosec/v2/cmd/gosec
  )
fi

exec "$bin" "$@"
