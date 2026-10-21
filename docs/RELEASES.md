# Releases

How Janus versions are numbered, what each kind of release promises, and
what happens when one has to be withdrawn. Releases are cut as
[CONTRIBUTING](../CONTRIBUTING.md) describes; how they are built, signed,
and checked is in [ADR-003](adr/003-signed-checksums.md) and
[ADR-004](adr/004-release-tooling.md).

## Version numbers

Janus follows [semantic versioning](https://semver.org): `vMAJOR.MINOR.PATCH`,
with release candidates of a coming version as `vMAJOR.MINOR.PATCH-rc.N`.

- **A tag is never moved or used twice.** If a release goes wrong, the fix
  is a new version. A tag whose release failed to publish keeps its number,
  and the next tag takes the one after: v0.9.0-rc.14 and rc.15 stopped in
  the release workflow and were never published, so the next release was
  rc.16.
- **A pre-release** (anything with `-rc.N`) is a candidate: it may change
  anything. GitHub marks it as a pre-release, and `janus update`,
  `janus version -check`, `janus doctor`, and the startup notice offer one
  only with `-pre` or `update.prerelease`. The Homebrew, Scoop, and winget
  packages get stable releases only.
- **A stable release** keeps the promises below for every later release
  with the same major version. Its docs become the
  [docs site](https://risadams.github.io/Janus/)'s default; main's,
  which may describe what is not released yet, are under "Next".

## Release notes

Each release's notes open with the message of its signed tag, which
`scripts/tag-release.sh` asks for: a line or two, for people, on what the
release brings. Below it, GitHub lists the pull requests since the last
release. When a release fixes a security problem, its tag message names
it, with the advisory or CVE, so the notes say which vulnerabilities it
fixes.

## What a stable release promises

From 1.0, within one major version:

- **Your config keeps working.** A newer Janus reads an older config,
  upgrading it in memory and saying what changed.
- **Your ledger stays readable.** `janus stats` from a newer Janus reads
  every ledger an older one wrote.
- **Commands and flags stay.** A flag may gain a better default only if
  the old behaviour is still available.
- **`-json` output keeps its fields.** New fields may appear; none are
  removed or change meaning.
- **Updates keep working.** The release layout `janus update` depends on
  does not change, and a new signing key is added alongside the old one
  rather than replacing it.
- **Pass-through stays byte-for-byte.** With every feature off, what
  reaches the API is what a direct connection would send.

A minor version adds features and settings, off by default as the
charter requires. A patch only fixes. A major version may break any of
the above, and its release notes say what and how to move.

Not promised: the wording and layout of the console and of other
human-readable output, and anything under `internal/`, which Go does not
let other modules import. Janus is a program, not a library.

Before 1.0, a minor version (0.9 to 0.10) may break these, and its release
notes say so.

## A version 2

Go requires a module's major version from 2 on to be in its path. Janus 2
would be `github.com/risadams/janus/v2`, installed with
`go install github.com/risadams/janus/v2/cmd/janus@latest`, and the
install guide, `janus update`'s advice for a `go install` copy, and the
Homebrew formula would all change to that path in the release that makes
it.

## Withdrawing a release

A release is withdrawn when running it is worse than not: a security
problem, a change that loses or corrupts the ledger, a break in
byte-for-byte pass-through, or a break in `janus update` itself. A
release that is merely buggy is fixed by the next one, not withdrawn.

1. **Fix forward.** Publish a release that fixes it, with a higher
   version. Never move the withdrawn tag or reuse its number.
2. **Stop offering it.** Turn the withdrawn release on GitHub back into a
   draft. `janus update`, `version -check`, `doctor`, and the startup
   notice skip drafts, so no copy is offered it, and its downloads are no
   longer public.
3. **Retract it in Go.** The fixing release's `go.mod` adds
   `retract vX.Y.Z // why`. `go install …@latest` then skips it, and
   `go list -m -u` warns anyone building against it. A retraction only
   takes effect once it is published in a newer version, which is why the
   fix comes first.
4. **Move the packages on.** Once Homebrew, Scoop, and winget carry
   Janus, the fixing release updates the formula and the manifest past the
   withdrawn version, and a pull request to `microsoft/winget-pkgs` removes
   it from winget.
5. **Tell people who installed it.** Janus has no way to tell a copy
   that its own version was withdrawn; it can only say that a newer
   release is out. So:
   - The fixing release's notes start by naming the withdrawn version, what
     was wrong with it, who is affected, and what to do (usually
     `janus update`).
   - A security problem also gets a GitHub security advisory for the
     affected versions, as [SECURITY.md](../SECURITY.md) describes, which
     reaches anyone watching the repository's advisories.
   - An issue, pinned to the repository until the next stable release,
     says the same.
