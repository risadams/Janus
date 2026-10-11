# The guard

The guard deals with secrets in requests on their way to Anthropic. It has
two levels.

**Detect.** With `features.guard` on, Janus scans each request that goes
upstream and records what it finds: how many secrets, of what kinds, and
in which part of the request. Nothing is changed. Use it to find out what
is leaving your machine.

```json
{ "features": { "guard": true } }
```

**Redact.** With `guard.redact` as well, each secret is replaced with a
placeholder before the request is sent, and put back in the answer. Claude
never sees the secret, and what it does with it still works.

```json
{ "features": { "guard": true }, "guard": { "redact": true } }
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

## Redaction

A secret in a request becomes a placeholder such as
`JANUS_REDACTED_3f9a1c07b2e4`. Claude sees the placeholder wherever the
secret was, and can use it: pass it to a command, write it to a file, put
it in a URL. When the answer comes back, Janus replaces each placeholder
with the secret it stands for before the client sees it. The tool call
that runs on your machine has the real value.

```
Read .env                       ->  Claude is sent  AWS_ACCESS_KEY_ID=JANUS_REDACTED_3f9a1c07b2e4
Claude: Write export.sh with
  export AWS_ACCESS_KEY_ID=JANUS_REDACTED_3f9a1c07b2e4
                                ->  your machine writes  export AWS_ACCESS_KEY_ID=AKIA...
```

This was tested with a real Claude Code session: Claude read a `.env` file
holding three planted credentials and was asked to write each value into a
second file. The second file held all three real values and no
placeholders, and the ledger recorded three secrets kept from going
upstream and three put back.

How it behaves:

- **The same secret always gets the same placeholder**, for as long as
  Janus runs. That keeps a request's bytes the same from turn to turn, so
  prompt caching still works, and lets Claude refer to "that key"
  consistently.
- **A secret is removed everywhere once it has been seen.** If Claude
  repeats a password in a later message, with nothing around it to mark
  it as one, it is still replaced.
- **Placeholders cut across pieces of a streamed answer are handled.**
  The test suite cuts one at every possible position.
- **Requests to the local model are not redacted.** They do not go to
  Anthropic, and the local model needs the real values.
- **Token counting requests are redacted too.**

It fails closed, in two places:

- **A request that cannot be checked is not sent.** If Janus cannot read
  a request to look for secrets, or finds one it cannot remove, the client
  gets an error and nothing goes upstream.
- **An answer that cannot be restored is stopped.** If Claude refers to a
  secret with a placeholder Janus did not issue, or a damaged one, the
  turn ends with an error rather than handing your machine a command with
  a meaningless token in it. A streamed answer is cut off with an error
  event; what was sent before it cannot be recalled, but a client does not
  act on a turn that ended in an error.

What it costs and what it cannot do:

- **It only protects what it detects.** Everything under "How well it
  works" applies. A secret the detectors miss goes upstream as before.
- **Claude cannot reason about a value it cannot see.** It can move a
  secret around; it cannot tell you whether a key looks valid or what a
  token's prefix means.
- **Answers to redacted requests arrive uncompressed**, because Janus has
  to rewrite them. Requests with nothing to redact are unaffected.
- **Captures still hold the original request.** A capture is the request
  as your client sent it, secrets included. They are owner-only files;
  turn `logging.capture` off if that is not acceptable.
- **Janus remembers the secrets it has seen for as long as it runs**, in
  memory, so that it can keep removing them. Nothing is written to disk.
- A stopped turn has to be retried by hand. Janus does not retry it.

## Tuning

```json
{
  "features": { "guard": true },
  "guard": {
    "patterns": ["ACME-[0-9]{6}-[A-Z]{4}", "licence:\\s*(\\S+)"],
    "allow": ["AKIAIOSFODNN7EXAMPLE"],
    "entropy": false,
    "redact": true
  }
}
```

| Field | Meaning |
| --- | --- |
| `patterns` | Your own regular expressions. If a pattern has a capture group, the group is the secret; otherwise the whole match. |
| `allow` | Exact values that are never reported, such as a public test key that appears throughout a codebase. |
| `entropy` | Turn on the entropy search. |
| `redact` | Replace secrets before they are sent, and restore them in answers. Off by default: the guard then only detects. |

## Where the results go

- **Console:** on the request's line, `[guard: 3 found going upstream:
  aws_access_key, github_token, url_password]` when detecting, or `[guard: 3
  kept from going upstream (...), 3 put back in the answer]` when
  redacting.
- **Ledger:** a `guard` field with the count, the kinds, and where in the
  request, for example `messages[3] user (tool_result)`; and when
  redacting, how many were replaced, how many put back, and why an
  exchange was stopped if it was.
- **`janus stats`:** a note with the number of requests affected.

None of these contain the secret. The capture of the request does, because
a capture is the request exactly as it was sent; captures are owner-only
files and can be switched off.

A secret that is in a conversation is sent again with every turn that
follows, and is counted each time. The counts are occurrences across
requests, not distinct secrets.

## Checking what left the machine

`janus audit` answers the question directly, from the ledger and the
captures.

```bash
janus audit                 # where everything went, and what became of any secrets
janus audit -since 24h
janus audit -verify         # also re-read what was sent and look for secrets in it
janus audit <request id>    # one request
janus audit -body <id>      # that request, exactly as it was sent
```

```
Where 5 requests carrying your content went
       3  to Anthropic, exactly as your client sent them
       2  to Anthropic, changed by janus on the way (2 with secrets replaced)
       0  to the local model only, never to Anthropic
       0  refused or stopped by janus, and sent nowhere

Secrets
  2 requests bound for Anthropic carried something that looks like a secret
  (aws_access_key 3, github_token 3, url_password 3).
       2  had them replaced with placeholders before sending
       0  were SENT WITH THE SECRETS IN THEM
```

If any request went with secrets in it, the audit lists each one by id,
kind, and place, and says to rotate them. It never prints a secret.

**`-verify` does not take the ledger's word for it.** It opens the saved
copy of every request that went to Anthropic, as it was sent, and runs the
detector over it again. It exits with status 1 if any still holds a
secret, so it can be used in a script. Because it uses the detector as
configured now, it also catches what a pattern you have added since would
have caught at the time.

```
Verification: re-read 5 requests as they were sent to Anthropic
       5  contain nothing that looks like a secret
       0  STILL CONTAIN SOMETHING THAT LOOKS LIKE A SECRET
       0  could not be checked: no capture was saved, or it has been pruned
```

**`-body` prints the bytes.** When Janus changed a request before sending
it, the capture holds both versions: `.request.txt` as your client sent
it, and `.sent.txt` as it went. `janus audit -body <id>` prints the one
that went. In the test session, this is the `.env` file as Anthropic
received it:

```
AWS_ACCESS_KEY_ID=JANUS_REDACTED_e1a4c4ad4cea
DATABASE_URL=postgres://app:JANUS_REDACTED_73d86e825765@db.internal:5432/orders
GITHUB_TOKEN=JANUS_REDACTED_102b543b324f
```

What it depends on:

- **Captures.** A request with no capture, because `logging.capture` was
  off or the capture has been pruned, appears in the counts and cannot be
  verified or printed.
- **The detector.** "Contain nothing that looks like a secret" means
  nothing a detector recognises. It is evidence, not proof.

## What it does not do yet

- Keep chosen files or directories from ever going upstream, whatever is
  in them.
