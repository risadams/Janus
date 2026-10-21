# The guard

The guard looks for secrets in requests on their way to Anthropic, and
can swap them for placeholders so that Claude never sees them.

## Why it exists

Claude Code sends Anthropic whatever it reads. Ask it to fix a failing
deploy and it may open `.env`, print a config file, or run a command
whose output has a token in it, and all of that goes upstream as part of
the conversation. It goes again on every later turn, because each turn
resends the whole conversation. Nobody decided to share the key; it was
just in the way.

Once a secret has left your machine you cannot call it back, so the only
useful moment to catch it is before it goes. Janus already sees every
request, which makes it the one place that can look. The guard is
cautious on purpose. It starts by only watching, so you can see what it
finds before you let it change anything, and when it cannot check a
request, it refuses to send it rather than guess.

## Two levels

Two levels for secrets, and a third, the local-only policy, for whole
files:

![Three levels of keeping things in: find out what leaves, replace secrets with placeholders, and keep named files local](diagrams/privacy-levels.svg#gh-light-mode-only)
![Three levels of keeping things in: find out what leaves, replace secrets with placeholders, and keep named files local](diagrams/privacy-levels-dark.svg#gh-dark-mode-only)

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

Separately, a **local-only policy** keeps the files, directories, and
marked content you name from going to Anthropic at all, whatever is in
them. See [Local-only content](#local-only-content).

## What it looks at

Everything in a request that came from your machine: the system prompt,
the text of messages, the results of tools (which is where file contents
and command output are), and the arguments of earlier tool calls.

It skips thinking blocks, whose signatures are random by design and not
yours, and images.

Only requests that leave the machine are scanned: those sent upstream,
and those sent to OpenAI, which are checked as Claude Code sent them,
before they are translated. One answered by the local model is not.
Throughout this page, "upstream" means either.

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

Why a placeholder, and not just a blank? Because the work often needs the
secret to go somewhere: into a deploy script, a `curl` header, a config
file. Deleting it would break that work, and you would turn redaction off
to get it done. With a placeholder, Claude can move the secret around
without knowing what it is.

![A secret’s round trip: Claude Code sends a tool result holding a key, Janus sends a placeholder to Anthropic in its place, Claude answers with a tool call that uses the placeholder, and Janus puts the real key back before Claude Code acts](diagrams/redaction.svg#gh-light-mode-only)
![A secret’s round trip: Claude Code sends a tool result holding a key, Janus sends a placeholder to Anthropic in its place, Claude answers with a tool call that uses the placeholder, and Janus puts the real key back before Claude Code acts](diagrams/redaction-dark.svg#gh-dark-mode-only)

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

- **The same secret always gets the same placeholder**, from turn to turn
  and across restarts. That keeps a request's bytes the same, so prompt
  caching still works after Janus is restarted or upgraded, and lets
  Claude refer to "that key" consistently. Placeholders come from a key
  Janus makes once and keeps as `redact.key` in the log folder; delete it
  and every placeholder changes, so any conversation holding a replaced
  secret is written to the cache again on its next turn.
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
- **A tool call that cannot be restored is stopped.** If Claude calls a
  tool with a placeholder Janus did not issue, or a damaged one, the turn
  ends with an error rather than handing your machine a command with a
  meaningless token in it. A streamed answer is cut off with an error
  event; what was sent before it cannot be recalled, but a client does not
  act on a turn that ended in an error. Claude Code shows this as "Server
  error mid-response"; the ledger and `janus audit` give the reason.

  Text and thinking are not stopped for this. Nothing acts on prose, and
  a session about Janus itself writes placeholder-shaped text all the
  time, so such a token is passed on as Claude wrote it. Placeholder-shaped
  text that was already in your request, whole or partial, is yours and
  is never an error anywhere. An answer that is not streamed is still
  checked whole and withheld.

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

Once a request in the range has gone to OpenAI, the report adds the same
two lines for OpenAI below Anthropic's, and the lines after them say
"Anthropic or OpenAI". A request sent to both, Claude unable to answer
and OpenAI answering, is counted under each.

**`-verify` does not take the ledger's word for it.** It opens the saved
copy of every request that left the machine, as it was sent, and runs the
detector over it again. What OpenAI was sent is the translated request,
in OpenAI's own shape; the detector reads every piece of text in it, and
the local-only policy, which reads a request's structure, re-checks only
what went to Anthropic. It exits with status 1 if any still holds a
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

**`-json` is for scripts.** `janus audit -json` prints the same tally,
never in color, and exits as the report does (1 when `-verify` finds a
leak). The fields, in snake_case as `janus stats -json` has them:

| Field | What it holds |
| --- | --- |
| `dir` | The log folder audited |
| `from`, `to` | The first and last request counted; left out when there were none |
| `requests` | Requests carrying your content in the range |
| `anthropic` | Those that reached Anthropic: `untouched`, `changed`, and of the changed, `redacted`, `withheld`, `repaired` |
| `openai` | Those that reached OpenAI, in the same shape. A request sent to both is counted in each. |
| `local_only`, `stopped` | Those answered only by the local model, and those sent nowhere |
| `kept_local` | Those the local-only policy kept off the machine altogether |
| `secrets` | `requests` that carried a secret off the machine, how many had them `replaced`, the `kinds` found with counts, and the `exposed` requests sent with them in: each an `id`, its `kinds`, and `where` |
| `blocked` | Requests the guard stopped: `id` and `why` |
| `verify` | Only with `-verify`: `checked`, `clean`, `unchecked`, and the `leaks`, shaped as `exposed` is |

Lists are always there, empty when there is nothing in them. With a
request id, `janus audit -json <id>` prints that request's ledger record,
as [the ledger](LEDGER.md) describes it. Fields may be added in later
versions; none are removed or change meaning within a major version
([releases](RELEASES.md)).

For the wider picture, including the listening port, files on disk, and
the connection to LM Studio, see the [threat model](THREAT-MODEL.md).

What the guard costs in time is in the
[performance notes](research/performance.md): a few milliseconds on an
ordinary request, once it has seen the conversation.

## The leak tests

"Nothing protected goes upstream" is a test that runs on every change, in
`internal/route/leak_test.go`. It sends a conversation seeded with every
kind of secret the guard knows, in every part of a request that carries
text, together with local-only content in each form the policy recognises.
It sends it down every route janus has: straight through, token counting,
the override header, each kind of rule, every fallback and escalation, and
with Anthropic failing. Then it reads every byte the stand-in for
Anthropic received.

A second test runs the same conversation with the guard off and requires
the check to find every item, so that the first cannot pass by looking in
the wrong place.

The secrets are invented, and what the test proves is bounded by what the
detectors and the policy recognise: it shows that nothing recognised gets
past on any route, not that everything is recognised.

## Local-only content

Detectors go by what content looks like. The local-only policy goes by
where it came from: you name files, directories, or a marker, and whatever
is in them does not go to Anthropic, secret-shaped or not.

It exists because much of what should stay private looks like nothing in
particular. A customer list, a contract, an unreleased plan, or a client's
code that you are not allowed to share is ordinary text, and no pattern
will find it. You know where it lives, though, so the policy asks you
for that instead.

```json
"features": { "guard": true },
"guard": {
  "localOnly": {
    "paths": ["secrets/**", ".env*", "*.pem"],
    "markers": ["COMPANY CONFIDENTIAL"]
  }
}
```

A path is matched against the end of a file's path, so `*.pem` protects
any such file wherever it is, `.env*` any file whose name starts that way,
and `secrets/**` everything under any directory called `secrets`. `*`
stays within one directory; `**` crosses them; a pattern naming a
directory protects what is in it. A marker is a string: any message text
or tool result containing it is protected.

What counts as protected content:

| Content | Why |
| --- | --- |
| The whole result of a tool call that named a protected path | `Read secrets/db.yaml`, `cat .env \| head`, an MCP tool given the path. For Claude Code's own tools the path is looked for in the path arguments and the command; for any other tool, in every argument. Some arguments are not searched, because they mention a path without reading it (see [What is not a read](#what-is-not-a-read)). |
| Any message text or tool result carrying a marker | The whole block is protected, not only the marker. |
| A line of other tool output that quotes a protected file | A search across the tree prints `secrets/db.yaml:2:...` without the call ever naming that file. Those lines go, and the lines of context around them; the rest of the output stays. |

A line that only names a protected file, as a directory listing does, is
left alone: Claude can know the file exists.

### What is not a read

A session about secrets names its patterns all the time: in a search,
in a jq path such as `.environments[]`, in the text of an issue. None of
these reads a file, so none of them withholds a result:

- What Write writes, what Edit replaces, a command's description, and the
  Grep tool's pattern.
- A heredoc a command only copies to a file, or uses as the text of an
  issue or commit.
- In a shell command:
  - the filter of `jq`, and the `--jq` of `gh`
  - the pattern of `grep`, `git grep`, and `rg`
  - the `--title` and `--body` of `gh issue` or `gh pr` `create`, `edit`, or `comment`

Janus reads the command as the shell would, quotes and all, and lets an
argument off only when it is sure what the command does with it:

- **The program's options are all ones Janus knows.** An unknown option
  might take the next word as its value. `grep --include '*.pem'` still
  withholds, because `--include` names files to read.
- **What the command prints goes only to Claude.** That includes passing
  it through a pipe to `head`, `sort`, `grep`, and the like. A command
  whose output goes on to `xargs`, a loop, a file, or a `$(...)` could
  read that output as a path, so every word of it is searched.
- **Nothing in it runs code.** A command with a shell, an interpreter,
  `eval`, or `xargs` is searched whole. So is anything Janus does not
  follow: `$(...)`, `<(...)`, a subshell, a comment, a heredoc it keeps.
- **The expression is the one the program sees.** An unquoted glob or a
  variable could become something else, and a jq program with `import`
  or `include` reads files. These are searched.

Every other argument is a path as far as the policy is concerned, so a
pattern in it still withholds the result. This is safe, and in a
repository that talks about its secret files it is still the commonest
false positive: in a week of use, about half of what is still withheld came from
heredocs given to `python3` and from `$(gh issue create ...)`.

### What happens to it

`action` chooses, and the default is `withhold`.

**`withhold`**: the request goes where it was going, and in anything bound
for Anthropic the protected content is replaced by a notice saying that it
was withheld and why. Claude carries on without it, and says so:

> I can't tell you this one. When I tried to read `secrets/db.yaml`, a
> tool called janus blocked it. It's set so anything under `secrets/**`
> stays on your machine and isn't sent to me.

Your own machine is unaffected: Claude Code still read the file, and a
request that a rule sends to the local model goes there whole. The
replacement is the same every time, so the conversation stays cacheable.

**`local`**: a request holding protected content is answered by the local
model instead, whole, and never goes to Anthropic. Set `local` to the
`local.models` entry to use if there is more than one. This outranks
everything: an `X-Janus-Route: upstream` header, the routing rules, the
fallback to Claude when LM Studio is down, and escalation. If the local
model cannot take the request, it is refused with an error saying why, and
is not sent. Routing does not need to be switched on for this.

Once a conversation has read a protected file, every later turn of it
still holds that tool call, so with `local` the rest of the conversation
is the local model's. That is the point, and it is also the cost: use
`withhold` if you want Claude to keep working around the gap.

Two things are the same under either action. Lines of search output that
quote a protected file are withheld, and do not by themselves send a
conversation to the local model. And token counting, which only Anthropic
can do, has protected content withheld from it.

### It fails closed

A request that cannot be checked is not sent: one that does not parse, or
is too large to read whole (32 MB). After withholding, the request is
checked again as it will be sent, and if anything protected is still
there it is stopped. Either shows up as a `500` in Claude Code with a
message starting `janus:`, and under "Stopped by the guard" in
`janus audit`.

### Where it shows

The console line says what happened, naming the path or marker and never
the content:

```
16:35:50  main       Opus 5.5                ✓ 4.3s    answered    ...
          ↳ local-only: 1 withheld from upstream (Read read /app/secrets/db.yaml, which "secrets/**" makes local-only)
16:36:22  main       qwen3.8-27b (local)     ✓ 31.8s   answered    ...
          ↳ local-only: kept on this machine (Read read /app/secrets/db.yaml, which "secrets/**" makes local-only)
```

The ledger records the same in `guard.local_only` and `guard.withheld`.
`janus audit` counts requests that went with content withheld and requests
kept from Anthropic, and `janus audit -verify` re-reads what was sent and
checks it against the policy as it is now, as well as for secrets. That
makes it a way to find out what an earlier session sent before you added
a path.

### What it does not cover

Be clear about these before relying on it.

- **It follows tool calls and markers, not meaning.** Content from a
  protected file that arrives some other way is not recognised: pasted
  into the prompt, printed by a script that read the file itself, or
  retold in the report a subagent hands back.
- **Under `local`, the local model's answer is ordinary text.** It can
  repeat what it read. While the tool call is still in the conversation
  the conversation stays local, but once Claude Code compacts the history
  into a summary the call is gone, and a summary that repeats the content
  would go upstream. `withhold` does not have this problem, because
  nothing downstream of the file ever saw it.
- **Search output is recognised by its shape**, `path:line`. A tool that
  prints the file name on a line of its own with the matches under it is
  not caught. Claude Code's own Grep, `grep -r`, and `rg` when piped all
  use the recognised shape.
- **A heredoc that may be code is searched as part of the command.** If
  the command runs a shell or an interpreter, pipes the heredoc on, or
  runs a file a heredoc wrote, Janus can't tell a mention from a read, so
  a protected path quoted in the heredoc still withholds the result. The
  same goes for any command it cannot follow (see
  [What is not a read](#what-is-not-a-read)).
- **Paths with spaces in them** are not recognised in a shell command or
  in search output. They are in a tool argument that is only the path.
- **The system prompt and tool definitions** are not checked for markers.
  Claude Code puts `CLAUDE.md` and the like in messages, which are.
- **Captures are not withheld.** The request as Claude Code sent it is
  saved under `logs/captures/` in full, on your disk, readable only by
  you. Turn captures off if that is not wanted.
- **What Claude writes.** If Claude is asked to write a protected file, it
  composed the content, so Anthropic has seen it.
