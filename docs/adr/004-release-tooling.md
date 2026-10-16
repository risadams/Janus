# ADR-004: Release tooling, without GoReleaser

Status: accepted · Date: 2026-10-16

## Context

Releases are built by `scripts/release.sh`, about fifty lines that build
six archives and write `checksums.txt`, and published by
`.github/workflows/release.yml`, which also signs the checksums
([ADR-003](003-signed-checksums.md)) and attests provenance. M9 adds
Homebrew (#260), Scoop and winget (#261), notarization (#262), an SBOM
(#263), and reproducible builds (#264). GoReleaser does most of these
from one config file, and is the usual answer. ADR-001 does not apply to
it, since it is a build tool and not a module Janus imports, but it would
take over most of the release path.

One constraint came first. Since v0.9.0-rc.13, every installed Janus
runs `janus update`, which expects the release laid out exactly as
`release.sh` lays it out: `janus_<version>_<os>_<arch>.tar.gz` (`.zip`
on Windows), holding a folder of the same name with `janus` or
`janus.exe` in it, beside `checksums.txt` in `sha256sum` form and
`checksums.txt.sig`. A release laid out otherwise breaks the update of
every copy already installed. GoReleaser's defaults differ in the name
of the checksums file and in not wrapping the binary in a folder.

The decision was reached by settling each need on its own, in an
interview with the owner, before asking which tool fits. Each answer is
below with the reason that decided it.

## Decision

Keep `scripts/release.sh`, and build each piece ourselves, from the
standard library or the vendor's own tool.

1. **The release layout is a contract.** Names, folder layout,
   `checksums.txt` and `checksums.txt.sig` stay as they are. The release
   workflow checks every build by running `janus update`'s own install
   code (`update.Install`) against it, for every platform, so a change
   that would break an installed copy fails the pull request.
2. **Notarization uses Apple's tools.** A macOS job runs `codesign` and
   `xcrun notarytool`. The Developer ID key is the most valuable secret
   in the release, and only Apple's own tools touch it; GoReleaser's way
   is `quill`, a reimplementation that runs on Linux. A macOS runner
   costs nothing for a public repository.
3. **Homebrew gets a hand-written formula that installs the release
   archives**, with their sums from `checksums.txt`, so the binary
   people run is the one that was signed and attested. Homebrew does not
   quarantine what it downloads, so this is also the way round
   Gatekeeper until notarization lands. GoReleaser now writes casks
   instead, and casks are for macOS applications.
4. **The tap and the Scoop bucket are `risadams/homebrew-tap` and
   `risadams/scoop-bucket`**, each written by the release workflow with
   a deploy key of its own: one repository each, no expiry, and nothing
   to do with the owner's other access. CI's commits there are not
   signed, so neither repository requires signed commits.
5. **winget is automated**: on a stable tag, a Windows job runs
   Microsoft's `wingetcreate update --submit`, which opens the pull
   request to `microsoft/winget-pkgs`.
6. **The winget token belongs to a machine account** that owns the fork
   of `winget-pkgs` and nothing else, since `wingetcreate` needs a
   classic token whose `public_repo` scope covers every public
   repository its account can write to. A leak can then only open pull
   requests Microsoft reviews. The token expires after a year, and the
   job fails saying to renew `WINGET_TOKEN` rather than failing obscurely.
7. **The SBOM is written by `cyclonedx-gomod`**, the CycloneDX project's
   own tool, run with `go run` at an exact version as CI runs
   `staticcheck`, once per platform, and bound to its archive with
   `actions/attest-sbom`. Syft would read the binaries rather than the
   build, but is a far larger tool to bring into the release path.
8. **Archives are made by a small Go program** using `archive/tar`,
   `archive/zip`, and `compress/gzip`, with times taken from the tagged
   commit, fixed owners, and sorted entries, so the same tag gives the
   same `checksums.txt` on any OS. A notarized darwin binary carries
   Apple's signature, which a rebuild cannot make; for those the claim is
   that the binary before signing is the same, and SECURITY.md says so.
9. **Each channel is its own job after publishing**, run only on stable
   tags. A channel can only be updated once the release's URLs exist, so
   it can fail after the release is public; as separate jobs, the
   release and the other channels stand, and `gh run rerun --failed`
   retries the one. Each job only writes a manifest for a tag that
   exists, so running it again is harmless. winget's own validation runs
   after our job has finished, so a failure there shows in its pull
   request, not in our workflow.

With these settled, GoReleaser would contribute a Scoop manifest of about
twenty lines, and hooks that call tools chosen above. That is not worth
handing over the release path or fitting the contract to its config.

## Consequences

The release path stays readable end to end, in shell, small Go programs,
and the vendors' own tools, and a change to it is a change to code in
this repository. Each channel is ours to keep working: when Homebrew,
Scoop, or winget change their formats, nothing upstream fixes it for us.

The owner sets up, once: the two repositories and their deploy keys
(`HOMEBREW_TAP_KEY`, `SCOOP_BUCKET_KEY`), the machine account, its fork
and token (`WINGET_TOKEN`), and, when the Developer ID arrives, the
certificate and notary credentials.

Revisit if the release path grows past what a person can read in one
sitting, or if GoReleaser gains byte-reproducible archives and native
notarization, which would remove the two reasons that weighed most.
