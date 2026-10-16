# ADR-003: Signed checksums for releases

Status: accepted · Date: 2026-10-16

## Context

Each release publishes `checksums.txt`, the SHA-256 of every archive,
beside the archives themselves. That catches a download cut short or
corrupted, but not a compromised release: whoever can replace an archive
on the release page can replace `checksums.txt` too. `janus update` (#258)
will replace the running binary with a download, so it needs a check that
does not trust the page the download came from.

Three ways were weighed:

- **Provenance attestation**, which the release workflow already makes
  (`actions/attest-build-provenance`). It proves the most: that an archive
  was built by this workflow from the tagged commit. But checking it means
  a Sigstore bundle, a certificate chain to Fulcio's root, and a
  transparency-log proof. In Go that is `sigstore-go` and its many
  modules, against [ADR-001](001-dependencies.md); the other way is to run
  `gh attestation verify`, which most people who install Janus do not
  have, and which needs a GitHub login.
- **An ed25519 signature over `checksums.txt`**, made in CI with a key held
  as an Actions secret, its public half compiled into Janus. `crypto/ed25519`
  checks it in a few lines, offline, with nothing to install. It proves
  less than the attestation: that the holder of the key signed these sums,
  not how the archives were built.
- **minisign, signify, or SSH signatures**, which wrap ed25519 in a format
  of their own. Each would let people check by hand with a tool they may
  have, but minisign's default form hashes with BLAKE2b, which the
  standard library lacks, and each format is more to write and parse than
  a bare signature, for no gain to `janus update`.

## Decision

Every release publishes `checksums.txt.sig`: the raw 64-byte ed25519
signature of `checksums.txt`.

- The private key is the Actions secret `JANUS_RELEASE_KEY`, and nowhere
  else. `go run ./scripts/signsums -new | gh secret set JANUS_RELEASE_KEY`
  makes one: the private half goes straight into the secret and the public
  half is added to `internal/update/release-keys.txt`, which is compiled
  into Janus.
- The release workflow signs on a tag and checks the signature against
  that list before publishing, so a release that Janus cannot check is
  never published. A pull request signs with a key made for the run, so the
  signing is tested before a tag depends on it.
- Janus accepts a signature by any key in the list. A new key is added at
  the top and old keys stay, so the releases they signed can still be
  checked.
- People check by hand with OpenSSL, as [SECURITY.md](../../SECURITY.md)
  sets out; the release workflow runs the same commands, so the
  instructions cannot drift from what works.
- The provenance attestation stays, for those who want the stronger claim.

**Rotating the key.** Run `signsums -new` as above and commit the list.
Releases from then on are signed with the new key; a copy of Janus built
before the change knows only the old key, so it can check releases up to
the rotation and refuses later ones, saying that the download is not
signed by a key it knows. Updating by hand once brings it the new key.

**A key that is lost** (the secret deleted) is rotated as above. **A key
that leaks** is rotated, and its line removed from the list, so that new
builds stop trusting it; old builds still do, so the release notes say to
update by hand, and which versions are affected.

## Consequences

`janus update` can refuse a tampered download using only the standard
library and the binary it is replacing. A tag can no longer be published
until `JANUS_RELEASE_KEY` is set. The signature is only as safe as the
secret, and anyone who can change the release workflow on `main` can use
it; the attestation does not have that weakness, and moving to it is worth
revisiting if Go gains a small, standard way to check Sigstore bundles.
