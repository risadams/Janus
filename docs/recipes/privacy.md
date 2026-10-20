# Recipe: keep secrets and private files from leaving

Claude Code sends Anthropic whatever it reads, and it reads a lot: config
files, command output, the file you asked about and the three next to it.
This recipe stops the parts you would rather keep.

There are three levels, each a setting. Start with the first and look at
what it finds before turning on the rest. Seeing what actually leaves
first matters for two reasons: it tells you whether you have a problem
at all, and it shows you what the guard catches before you let it change
your requests, so a false match surprises you in a report and not in the
middle of a deploy.

![Three levels of keeping things in: find out what leaves, replace secrets with placeholders, and keep named files local](../diagrams/privacy-levels.svg#gh-light-mode-only)
![Three levels of keeping things in: find out what leaves, replace secrets with placeholders, and keep named files local](../diagrams/privacy-levels-dark.svg#gh-dark-mode-only)

## 1. Find out what is leaving

```json
"features": { "route": false, "escalate": false, "compress": false, "guard": true }
```

Nothing changes in what is sent. Janus looks for credentials in each
request bound for Anthropic and records how many, of what kind, and in
which part of the request. Use Claude Code for a while, then:

```bash
janus audit
```

Under "Secrets" it lists the requests that went to Anthropic with
something that looks like a credential in them. Most people find some:
an `.env` file Claude read, a key in a config file, a token in a shell
command's output. Treat any listed there as disclosed.

## 2. Replace secrets on the way out

```json
"guard": { "redact": true }
```

Each secret is now replaced with a placeholder such as
`JANUS_REDACTED_3f9a1c0b77de` before the request leaves, and put back in
the answer. Claude can still use the secret, in a command or an edit,
without having seen it: it writes the placeholder, and Janus restores the
real value before Claude Code acts.

If something that is not a secret is being replaced, add the exact value
to `guard.allow`. If something that is a secret is missed, add a pattern
to `guard.patterns`. Both are in [the guard](../GUARD.md#tuning).

## 3. Keep whole files local

Redaction goes by what a secret looks like. For content that is private
but does not look like anything, name where it lives:

```json
"guard": {
  "redact": true,
  "localOnly": {
    "paths": [".env*", "secrets/**", "*.pem", "customers/**"],
    "markers": ["COMPANY CONFIDENTIAL"]
  }
}
```

When Claude reads one of these files, Janus replaces what it read with a
notice before the request goes to Anthropic. Claude is told the content
was withheld and carries on without it. Any text carrying a marker is
treated the same way.

To have the local model answer instead, so that the question still gets
an answer from something that can see the file, add `"action": "local"`.
Read [what that means](../GUARD.md#what-happens-to-it) first: the rest of
that conversation stays with the local model.

## Check it

```bash
janus audit -verify
```

re-reads every request as it was sent to Anthropic and looks for secrets
and local-only content, using the detectors and policy as they are now.
It exits non-zero if it finds any, so it can be run from a script. For
one request:

```bash
janus audit <id>          # what was found, replaced, and withheld
janus audit -body <id>    # the exact bytes that were sent
```

## Know the limits

- **Detection is by pattern.** A secret in no known format with no
  telling name beside it is not found.
- **The policy follows tool calls and markers**, not meaning. Content
  from a private file that arrives another way, pasted in or printed by a
  script, is not recognised.
- **Captures on your disk are not redacted.** They hold each request as
  Claude Code sent it. Set `logging.capture` to false if that is not
  wanted; `janus audit -verify` needs them.
- **This keeps things from Anthropic, not from a prompt injection.**
  Claude can use a placeholder in a command, and the real secret is put
  back when it does.

The [threat model](../THREAT-MODEL.md) has these in full.
