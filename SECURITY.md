# Security

Janus sits between Claude Code and the Anthropic API, so it sees your
credentials, prompts, and code. A problem that could expose any of them, or
let something reach a machine it shouldn't, is a security problem.

## Reporting

Report it privately: on the repository's **Security** tab, choose
**Report a vulnerability**
(<https://github.com/risadams/Janus/security/advisories/new>). Please don't
open a public issue for it.

Say what you found, how to reproduce it, and which version (`janus version`)
you ran. Leave out real credentials and captures.

## What Janus does and doesn't protect against

The [threat model](docs/THREAT-MODEL.md) sets out what Janus is trusted
with, who it defends against, and the risks it accepts. A report is most
useful when it shows one of those defences failing, or a risk the threat
model doesn't account for.

## Checking a download

Each release's `checksums.txt` is signed with the Janus release key, and
the signature is `checksums.txt.sig` beside it. The keys are listed, the
newest first, in
[`internal/update/release-keys.txt`](internal/update/release-keys.txt);
take the key from the repository rather than from a release, which is
the thing being checked. [ADR-003](docs/adr/003-signed-checksums.md) says
why it is done this way and how keys are rotated.

With OpenSSL 3 or later, and the key as `key`:

```bash
key='<the newest line of release-keys.txt>'
curl -LO "${url%/*}/checksums.txt.sig"   # url as in the README
{ printf '\060\052\060\005\006\003\053\145\160\003\041\000'; printf '%s' "$key" | base64 -d; } > janus-release.der
openssl pkeyutl -verify -pubin -keyform DER -inkey janus-release.der \
  -rawin -in checksums.txt -sigfile checksums.txt.sig
```

It prints `Signature Verified Successfully`, or fails. The first `printf`
is the fixed header that turns a bare ed25519 key into the DER form
OpenSSL reads. Then check the archive against the sums with `shasum -a
256 -c checksums.txt --ignore-missing`, as in the README. The release
workflow runs these same commands on every build, so they work as
written.

`gh attestation verify <archive> --repo risadams/Janus` checks a stronger
claim, that this repository's workflow built the archive from the tagged
commit, if you have the GitHub CLI.

## Supported versions

Janus is in pre-release. Fixes go into the next release candidate; there
are no backports.
