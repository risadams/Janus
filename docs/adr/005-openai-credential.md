# ADR-005: The one credential Janus holds

Status: accepted · Date: 2026-10-21

## Context

Until v0.10.0 Janus held no credential. Claude Code sends its own with
every request, and Janus passes it to Anthropic untouched and forgets it
when the request ends. The threat model rests on that, and
[keepwarm.md](../research/keepwarm.md) turned down a feature because it
would have kept a credential longer.

v0.10.0 lets an OpenAI Codex model answer Claude Code's requests (#367).
Claude Code sends nothing OpenAI would accept, so Janus has to supply an
OpenAI credential itself. The Codex CLI is different: it sends its own,
which passes through like Claude Code's and is not covered here.

The ways considered:

- **A key in the config file.** Simple, and the config is created
  owner-readable. But configs get pasted into issues, copied between
  machines, and backed up, and `janus config upgrade` leaves a `.bak`
  copy beside it. A key there leaks the first time any of that happens.
- **The Codex CLI's ChatGPT login** (`~/.codex/auth.json`), to spend a
  ChatGPT plan instead of API credit. It would mean refreshing another
  program's tokens and calling an endpoint OpenAI does not document for
  other programs. On the Anthropic side, the owner has ruled out the same
  move with Claude Code's login.
- **An environment variable.** The usual way to give a program an API
  key, and nothing is written to disk by Janus. But a Janus started by
  `janus service`, under launchd or systemd, does not see the shell's
  environment.
- **The key written into the service unit** by `janus service install`.
  That fixes the service, but puts the key in a file that was never meant
  to hold a secret.
- **The operating system's credential store**: the macOS Keychain, the
  Windows Credential Manager, or the Secret Service on Linux. The key is
  stored encrypted, by the user, with a tool made for it, and a service
  can read it.

## Decision

Janus reads one credential of its own: the OpenAI API key that the OpenAI
backend sends with Claude Code's translated requests. `openai.key` in the
config says where it comes from, and is one of:

- `env:NAME`: the environment variable `NAME`, such as
  `env:OPENAI_API_KEY`.
- `keychain:NAME`: the entry `NAME` in the operating system's credential
  store.

A literal key in `openai.key` is refused at startup, with a message that
says how to use either source instead.

- **The key is read once, at startup**, and kept in memory only. It goes
  only on requests to the configured OpenAI base URL. It is never
  written to the config, a log, a capture, the ledger, or a service unit,
  and doctor says where it comes from and whether it was found, never what
  it is. Tests prove it reaches nothing but the fake OpenAI server.
- **The credential store is read with what the platform already has**, so
  ADR-001 holds: `security find-generic-password` on macOS, `secret-tool`
  from libsecret on Linux, and `CredReadW` from `advapi32.dll` through the
  standard `syscall` package on Windows. A command is run with a fixed
  argument list, never through a shell. If the store or its tool is
  missing, the error names the command that adds the entry.
- **Credentials still do not cross.** Claude Code's Anthropic credential
  is never sent to OpenAI, and the OpenAI key is never sent to Anthropic
  or LM Studio.
- **Nothing else changes.** Janus still keeps no client's credential
  beyond its request, so keepwarm.md's decision stands.

## Consequences

Claude Code can be answered by OpenAI, from a shell or a service, without
the key ever being in a file Janus writes. Janus now holds a secret of
its own, so a memory dump or a debugger attached to Janus can reveal it,
which was not true before; the threat model says so. Setting up the
keychain is one more step for people who run Janus as a service.

Revisit if OpenAI offers a way for a local program to use a ChatGPT plan
that it documents for third parties, or if a second provider needs a
credential of its own: this decision covers one key, not a store of them.
