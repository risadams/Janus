# The guard

The guard looks for secrets in requests on their way to Anthropic.

**It only detects so far.** With `features.guard` on, Janus scans each
Messages request that goes upstream and records what it finds: how many
secrets, of what kinds, and in which part of the request. It does not
remove, replace, or block anything yet. Use it to find out what is leaving
your machine before deciding what to do about it.

```json
{ "features": { "guard": true } }
```

## What it looks at

Everything in a request that came from your machine: the system prompt,
the text of messages, the results of tools (which is where file contents
and command output are), and the arguments of earlier tool calls.

It skips thinking blocks, whose signatures are random by design and not
yours, and images.

Only requests sent upstream are scanned. One that was answered by the
local model and never went to Anthropic is not.

## What it finds

| Kind | What it is |
| --- | --- |
| `private_key` | A PEM private key block |
| `anthropic_api_key`, `openai_api_key` | API keys by their prefix |
| `aws_access_key`, `aws_secret_key` | An AWS key id; a secret key next to its name |
| `github_token`, `slack_token`, `google_api_key`, `stripe_key` | Tokens with a recognisable shape |
| `jwt` | A JSON Web Token |
| `url_password` | The password in `scheme://user:password@host` |
| `assigned_secret` | A random-looking value assigned to a name such as `password`, `token`, `api_key`, or `secret` |
| `custom` | A match for one of your own patterns |
| `high_entropy` | A long random-looking string that fits none of the above. Off unless you turn it on. |

The detectors are tuned to stay quiet. A value has to be in a recognisable
format, or sit next to a name that says what it is and look generated.
Placeholders (`changeme`, `${API_KEY}`, `<your-token>`), references
(`os.environ["X"]`), and code that merely uses the word (`token =
parseToken(input)`) are not reported.

## How well it works

Measured on a labelled set built into the tests: 21 secrets across the
formats above, and 44 lookalikes (hashes, ids, placeholders, code,
certificates, lockfile checksums, embedded images).

| | Secrets found | False positives | Secrets with no format found |
| --- | --- | --- | --- |
| Default | 21 of 21 | 0 of 44 | 0 of 2 |
| With `entropy` on | 21 of 21 | 5 of 44 | 2 of 2 |

And on real traffic, in two Claude Code sessions:

- A session that read a file with three planted fake credentials: all
  three found, and none of their values written to the ledger or console.
- A session that read 164 kB of Go source, including this guard's own
  patterns: seven requests, no findings.

That is a small set and two sessions. It shows the detectors behave as
designed on the cases they were designed for. It does not tell you the
miss rate on secrets in formats nobody thought of.

**A secret with no recognisable format and no telling name is not found**
unless `entropy` is on. A bare random string pasted into a message is the
example.

### The entropy search

`"entropy": true` also reports any string of 32 or more characters that
looks random. It catches unformatted secrets, and it also reports lockfile
checksums, embedded images, certificates, and generated paths, because
those look random too. Expect noise. It is off by default for that reason.

## Tuning

```json
{
  "features": { "guard": true },
  "guard": {
    "patterns": ["ACME-[0-9]{6}-[A-Z]{4}", "licence:\\s*(\\S+)"],
    "allow": ["AKIAIOSFODNN7EXAMPLE"],
    "entropy": false
  }
}
```

| Field | Meaning |
| --- | --- |
| `patterns` | Your own regular expressions. If a pattern has a capture group, the group is the secret; otherwise the whole match. |
| `allow` | Exact values that are never reported, such as a public test key that appears throughout a codebase. |
| `entropy` | Turn on the entropy search. |

## Where the results go

- **Console:** `[guard: 3 found going upstream: aws_access_key,
  github_token, url_password]` on the request's line.
- **Ledger:** a `guard` field with the count, the kinds, and where in the
  request, for example `messages[3] user (tool_result)`.
- **`janus stats`:** a note with the number of requests affected.

None of these contain the secret. The capture of the request does, because
a capture is the request exactly as it was sent; captures are owner-only
files and can be switched off.

A secret that is in a conversation is sent again with every turn that
follows, and is counted each time. The counts are occurrences across
requests, not distinct secrets.

## What it does not do yet

- Remove or replace secrets before they are sent.
- Keep chosen files or directories from ever going upstream.
- Show, after the fact, exactly what left the machine.

Those are the rest of this milestone.
