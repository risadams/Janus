#!/usr/bin/env bash
# Cuts a release by pushing a signed tag; the release workflow does the rest:
#
#   scripts/tag-release.sh rc                 v0.9.0-rc.8 -> v0.9.0-rc.9
#   scripts/tag-release.sh final              v0.9.0-rc.9 -> v0.9.0
#   scripts/tag-release.sh minor --rc         v0.9.0      -> v0.10.0-rc.1
#   scripts/tag-release.sh patch|minor|major  v0.9.0      -> v0.9.1, v0.10.0, v1.0.0
#
# Options: -m "message" sets the tag message instead of opening an editor;
# --yes pushes without asking; --dry-run checks and says what it would tag,
# and tags nothing. Where there is no terminal to ask on, such as a command
# run with ! in Claude Code, -m and --yes are needed (#181).
#
# A stable tag (not a release candidate) also moves docs/vX.Y to it, which
# the docs site builds its default version from (#404).
#
# The release itself is never made by hand on GitHub: the workflow creates
# it from the tag, and fails if one already exists (#166). The tag is signed
# with the owner's SSH key, which a workflow cannot do, so this runs locally
# rather than as a button in Actions.
set -euo pipefail

usage="usage: scripts/tag-release.sh rc|final|patch|minor|major [--rc] [-m message] [--yes] [--dry-run]"
die() { echo "tag-release: $*" >&2; exit 1; }

bump="" rc=false dry=false yes=false message=""
while [ $# -gt 0 ]; do
  case "$1" in
    rc | final | patch | minor | major) bump="$1" ;;
    --rc) rc=true ;;
    --dry-run) dry=true ;;
    --yes | -y) yes=true ;;
    -m) [ $# -ge 2 ] || die "$usage"; message="$2"; shift ;;
    -h | --help) echo "$usage"; exit 0 ;;
    *) die "$usage" ;;
  esac
  shift
done
[ -n "$bump" ] || die "$usage"
if $rc && { [ "$bump" = rc ] || [ "$bump" = final ]; }; then
  die "--rc goes with patch, minor, or major"
fi
command -v gh >/dev/null || die "needs the GitHub CLI, gh"

# Without a terminal there is no editor for the message and no one to
# answer "Push?". Say so now, before anything is tagged.
if ! $dry && ! { : </dev/tty; } 2>/dev/null; then
  [ -n "$message" ] || die "there is no terminal to open an editor in; give the tag message with -m"
  $yes || die "there is no terminal to ask before pushing on; pass --yes to push without asking"
fi

cd "$(dirname "$0")/.."

# The tag goes on what is on GitHub's main, nothing else.
git fetch --quiet --tags origin main
[ "$(git branch --show-current)" = main ] || die "switch to main first"
[ -z "$(git status --porcelain)" ] || die "the working tree has changes; commit or stash them"
head="$(git rev-parse HEAD)"
[ "$head" = "$(git rev-parse origin/main)" ] || die "main is not the same as origin/main; pull or push first"

# The newest tag, with release candidates sorted before their release.
latest="$(git -c versionsort.suffix=- tag --list 'v[0-9]*' --sort=-v:refname | head -n 1)"
latest="${latest:-v0.0.0}"
[[ "$latest" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)(-rc\.([0-9]+))?$ ]] ||
  die "the newest tag, $latest, is not vX.Y.Z or vX.Y.Z-rc.N"
major="${BASH_REMATCH[1]}" minor="${BASH_REMATCH[2]}" patch="${BASH_REMATCH[3]}" n="${BASH_REMATCH[5]}"
core="v${major}.${minor}.${patch}"

case "$bump" in
  rc)
    [ -n "$n" ] || die "$latest is a release; start the next with patch --rc, minor --rc, or major --rc"
    next="${core}-rc.$((n + 1))" ;;
  final)
    [ -n "$n" ] || die "$latest is already a release"
    next="$core" ;;
  patch) next="v${major}.${minor}.$((patch + 1))" ;;
  minor) next="v${major}.$((minor + 1)).0" ;;
  major) next="v$((major + 1)).0.0" ;;
esac
if $rc; then next="${next}-rc.1"; fi

# A stable release's docs are the site's default from now on: docs/vX.Y is
# moved to it, before the tag is pushed so that the docs run the tag starts
# finds it. A fix cherry-picked onto the branch since the last release is
# on main too, and so in this tag; when the branch cannot fast-forward for
# that reason, a commit on top of it takes the tag's files, so its history
# stays linear and nothing is force-pushed.
docs_branch=""
if [[ "$next" =~ ^v([0-9]+)\.([0-9]+)\.[0-9]+$ ]]; then
  docs_branch="docs/v${BASH_REMATCH[1]}.${BASH_REMATCH[2]}"
fi

if git rev-parse --quiet --verify "refs/tags/$next" >/dev/null ||
  [ -n "$(git ls-remote --tags origin "refs/tags/$next")" ]; then
  die "$next is already tagged"
fi
if gh release view "$next" >/dev/null 2>&1; then
  die "a release named $next already exists on GitHub; delete it, or the workflow cannot publish"
fi
if [ "$latest" != v0.0.0 ] && [ "$bump" != final ] &&
  [ "$(git rev-parse "$latest^{commit}")" = "$head" ]; then
  die "nothing has changed since $latest"
fi

# The release workflow runs only a short build check, trusting that the full
# suite and the fuzz run have already passed on this commit.
for workflow in test.yml fuzz.yml; do
  result="$(gh run list --workflow "$workflow" --commit "$head" --event push --limit 1 \
    --json status,conclusion --jq '.[0] | "\(.status) \(.conclusion)"')"
  case "$result" in
    "completed success") ;;
    "") die "$workflow has not run on $(git rev-parse --short HEAD) yet" ;;
    completed*) die "$workflow did not pass on $(git rev-parse --short HEAD): ${result#completed }" ;;
    *) die "$workflow is still running on $(git rev-parse --short HEAD); try again when it finishes" ;;
  esac
done

if [ "$latest" = v0.0.0 ]; then
  commits="$(git log --format='%h %s' HEAD)"
else
  commits="$(git log --format='%h %s' "$latest..HEAD")"
fi
echo "Tagging $next on $(git rev-parse --short HEAD), after $latest:"
if [ -n "$commits" ]; then
  echo "$commits" | sed 's/^/  /'
else
  commits="(none: the same commit as $latest)"
  echo "  $commits"
fi

if [ -n "$docs_branch" ]; then
  echo "The docs site's default docs will be $next's, on $docs_branch."
fi

if $dry; then
  echo "Dry run: nothing tagged."
  exit 0
fi

# Signing goes through the SSH agent; with no key in it, git would stop to
# ask for a passphrase it cannot be given.
ssh-add -l >/dev/null 2>&1 || die "no key in the SSH agent; load the signing key with ssh-add"

if [ -n "$message" ]; then
  git tag -s "$next" -m "$message"
else
  template="$(printf '\n# Say in a line or two what this release brings; lines starting\n# with # are dropped. The commits since %s:\n#\n' "$latest")"
  template="${template}$(echo "$commits" | sed 's/^/#   /')"
  git tag -s -e "$next" -m "$template"
fi
git cat-file -p "$next" | grep -q -- '-----BEGIN SSH SIGNATURE-----' || {
  git tag -d "$next" >/dev/null
  die "the tag came out unsigned, so it was removed; check git's gpg.format and user.signingkey"
}

answer=y
if ! $yes; then
  printf 'Push %s to origin? [y/N] ' "$next"
  read -r answer </dev/tty || answer=""
fi
case "$answer" in
  y | Y | yes) ;;
  *) git tag -d "$next" >/dev/null; echo "Not pushed; the tag was removed."; exit 0 ;;
esac
if [ -n "$docs_branch" ]; then
  if git ls-remote --exit-code --heads origin "$docs_branch" >/dev/null; then
    git fetch --quiet origin "+refs/heads/$docs_branch:refs/remotes/origin/$docs_branch"
    docs_tip="$head"
    if ! git merge-base --is-ancestor "origin/$docs_branch" "$head"; then
      docs_tip="$(git commit-tree -S "$head^{tree}" -p "origin/$docs_branch" -m "Take the docs of $next")"
    fi
    git push origin "$docs_tip:refs/heads/$docs_branch"
  else
    git push origin "$head:refs/heads/$docs_branch"
  fi
fi
git push origin "refs/tags/$next"

echo "Pushed. The release workflow builds and publishes it; follow it with"
echo "  gh run watch \$(gh run list --workflow release.yml --branch $next --limit 1 --json databaseId --jq '.[0].databaseId')"
echo "It appears at https://github.com/risadams/Janus/releases/tag/$next"
