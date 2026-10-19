#!/usr/bin/env bash
# Writes the Homebrew formula for one release to stdout (#260):
#
#   scripts/formula.sh v1.2.3 dist/checksums.txt > Formula/janus.rb
#   scripts/formula.sh v1.2.3 dist/checksums.txt "file://$PWD/dist" > janus.rb
#
# The formula installs the release's own archives, with their sums from
# checksums.txt, so what Homebrew installs is the binary that was signed,
# notarized, and attested (ADR-004). The third argument replaces the
# release's download URL, so that a pull request can install the formula
# from the archives it has just built. The release workflow writes the
# formula into risadams/homebrew-tap on each stable tag.
set -euo pipefail

version="${1:?usage: scripts/formula.sh <version> <checksums.txt> [base URL]}"
sums="${2:?usage: scripts/formula.sh <version> <checksums.txt> [base URL]}"
v="${version#v}"
base="${3:-https://github.com/risadams/Janus/releases/download/${version}}"

sum() {
  local name="janus_${v}_$1.tar.gz" s
  s="$(awk -v n="$name" '$2 == n || $2 == "*" n { print $1 }' "$sums")"
  [ -n "$s" ] || { echo "formula.sh: ${name} is not in ${sums}" >&2; exit 1; }
  echo "$s"
}
block() {
  printf '      url "%s/janus_%s_%s.tar.gz"\n      sha256 "%s"\n' "$base" "$v" "$1" "$(sum "$1")"
}

cat <<RUBY
# Written by Janus's release workflow (scripts/formula.sh in
# risadams/Janus) for each stable release; a change made here is lost at
# the next one.
class Janus < Formula
  desc "Local proxy for Claude Code that tracks cost and keeps secrets local"
  homepage "https://risadams.github.io/Janus/"
  version "${v}"
  license "MIT"

  on_macos do
    on_arm do
$(block darwin_arm64)
    end
    on_intel do
$(block darwin_amd64)
    end
  end

  on_linux do
    on_arm do
$(block linux_arm64)
    end
    on_intel do
$(block linux_amd64)
    end
  end

  def install
    bin.install "janus"
    generate_completions_from_executable(bin/"janus", "completion")
  end

  test do
    assert_equal "janus v#{version}", shell_output("#{bin}/janus version").strip
  end
end
RUBY
