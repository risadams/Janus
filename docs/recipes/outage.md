# Recipe: keep working when Claude is down

Have the local model stand in when Anthropic cannot answer: an outage, an
overload, no network, or a rate limit. On a subscription, running out of
usage arrives as a rate limit, so this covers that too.

## Set it up

You need a local model configured, as in
[the subagents recipe](claude-code.md#1-tell-janus-about-the-local-model).
Then name it as the stand-in on a rule that sends requests upstream:

```json
"routing": [
  { "name": "everything", "route": "upstream", "ifDown": "janus-local" }
],
"features": { "route": true, "escalate": false, "compress": false, "guard": false }
```

To move some work local *before* the limit is reached, see
[pacing](pace.md).

A rule with no `class` matches every request. Put it last if you have
other rules; the first rule that matches decides.

## What happens

Each request goes to Claude as usual. If Claude says it is out of usage (a
`429` that won't clear for a minute or more), the same request is sent to
the local model at once. A shorter failure (an overload, a server error,
no connection, a brief rate limit) is passed to Claude Code to retry; only
if Claude fails three times within two minutes does the local model step
in. Either way the local model's answer is returned. The response
carries an `X-Janus-Fallback` header, the console line shows that Claude
was tried first and why it failed, the ledger records it, and
`janus stats` lists when it happened, under "When Claude could not answer":

```
When Claude could not answer
  from              to     cause         requests  stood in  limit      answering again
  2026-10-14 09:54  11:24  rate_limited  61        60        five_hour  11:30
```

Each row is a run of requests Claude did not answer, none more than 15
minutes after the one before. **stood in** is how many the local model
answered; the rest reached Claude Code as errors, to retry. For a rate
limit, **limit** is which of the subscription's limits was reached
(`five_hour`, `seven_day`) and **answering again** is when Claude said it
would answer again.

Errors about the request itself, such as an expired login or a bad
request, are passed to Claude Code untouched. The local model would not
fix them.

## What to expect

- **The answers are the local model's.** It is standing in for Claude on
  work that was meant for Claude, including the main conversation. Expect
  slower and weaker; the aim is to keep going, not to match.
- **Claude Code is not told.** It sees a normal response naming the model
  it asked for. Watch Janus's console to know which you are talking to.
- **Requests too large for the local model fail** with Claude's original
  error, since there is nowhere else to send them. A long main
  conversation can outgrow a local context. A model LM Studio has not
  loaded is only sent requests up to 32,768 tokens unless `contextTokens`
  says otherwise. The console line's `↳ no local stand-in: ...` and the
  ledger's `reason` say why the local model did not answer.
- **It only works before an answer starts.** If Claude fails part-way
  through a streamed answer, what was already sent cannot be taken back.
- **Picking up again is safe.** When Claude is back, the conversation
  goes upstream with the local model's turns in it. Janus removes the one
  thing Anthropic would reject, the local model's unsigned thinking.
- **Janus says when Claude takes over again.** Claude reads the local
  model's turns as its own, so the first answer it gives the main
  conversation afterwards gets a note:

  ```
  ↳ Claude is answering again, after 12 turns from the local model since 09:54; it reads them as its own, so check what they did
  ```

  The ledger marks that answer with `handback`, and `janus stats` counts
  them. It is a good moment to ask Claude to review what was done in the
  meantime.

## Trying it

Point `upstream.baseUrl` at a port with nothing on it, restart Janus, and
use Claude Code. Each request should be answered locally, with the console
line showing the failed attempt on Claude first. Put the URL back
afterwards.

## With a local-only policy

Content the [local-only policy](../GUARD.md#local-only-content) protects
is never sent upstream by a fallback in the other direction: when LM
Studio is the one that is down, a request that must stay local is refused
rather than sent to Claude.

The [reference](../CONFIG.md#keep-working-when-claude-is-down) has the
details.
