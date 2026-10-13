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

## Supported versions

Janus is in pre-release. Fixes go into the next release candidate; there
are no backports.
