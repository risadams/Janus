# Recipe: try the local model first, with Claude as the net

Send a kind of request to the local model, check each answer, and have
Claude answer instead when the local one is broken. Claude Code receives
one normal response either way.

Use this in place of a plain `local` rule when you are not yet sure the
local model is up to the work, which at the start is always.

The reason it exists is that a local model fails in ways you would want
to catch: it stops halfway, calls a tool that is not there, or says it
cannot help. Without a check, Claude Code takes that answer as it is and
carries on from it. With one, a broken answer never reaches Claude Code,
and you can find out how often the model fails before you rely on it.

## Set it up

You need a local model configured, as in
[the subagents recipe](claude-code.md#1-tell-janus-about-the-local-model).
Then one rule:

```json
"routing": [
  { "name": "subagents", "class": "subagent", "route": "local-then-escalate", "local": "janus-local", "maxSeconds": 60 }
],
"features": { "route": true, "escalate": false, "compress": false, "guard": false }
```

`maxSeconds` is how long the local model may take over one answer before
Claude is asked instead. The default is 120.

## What gets checked

An answer is rejected, and the request sent to Claude, if it is empty, cut
off, calls a tool that does not exist or with arguments that do not fit
the tool's schema, or reads like a refusal or confusion. Rejected too if
LM Studio fails or runs out of time. The
[reference](../CONFIG.md#local-first-with-claude-as-the-safety-net) lists
the checks by name and how to switch one off.

**The checks catch broken answers, not wrong ones.** A well-formed answer
that is mistaken goes through. That is the judgement you are making about
the model when you route work to it.

## What it costs

Time, and it costs it twice when the local model fails: the client waits
for the local attempt, then for Claude. Nothing is streamed while the
local answer is being held for checking, so Claude Code shows nothing
until it is accepted. The answer has to be held, because once part of it
has reached Claude Code it cannot be taken back.

![What local-first costs in time: Claude alone streams at once and is done in a few seconds; a local answer is held back about 34 seconds while it is checked, and if it fails, Claude answers after that wait](../diagrams/local-first.svg#gh-light-mode-only)
![What local-first costs in time: Claude alone streams at once and is done in a few seconds; a local answer is held back about 34 seconds while it is checked, and if it fails, Claude answers after that wait](../diagrams/local-first-dark.svg#gh-dark-mode-only)

The times are from real use: Claude's the range seen in the first tests,
the local model's the median of local-first attempts on 12 to 14 October.

A model that fails often is worse than not using it. Watch the rate:

```bash
janus stats -since 24h
```

The "Local first" section shows, for each class of request, how many
were tried locally, how many answers were accepted, rejected by a check,
or failed because the model was unavailable, and the median time an
attempt took. Rejected and failed requests paid that time and then
Claude's as well. Above it, "Saved by answering locally" prices the
accepted answers at the Claude model each asked for: what the rule saved.
Over a week, `janus stats -since 7d -daily` shows the same day by day,
so a day the model did worse stands out.

In one test session with a small model, one answer was rejected as empty
and three passed the checks; two of those three were long and useless,
and Claude redid the work. That is what "broken, not wrong" means in
practice, and why the model matters more than the rule. The
[LM Studio notes](../research/lmstudio.md) have the details.

## Seeing one decision

When an answer is rejected, the console line says why, and with captures
on the rejected answer is saved beside the request as
`<id>_v1-messages.attempt.response.txt`.

```bash
janus audit <id>
```

## When to stop using it

Once the escalation rate for a rule is low and has stayed low, a plain
`"route": "local"` rule gives the same answers and streams them as they
come. It still sends a request to Claude when LM Studio is down or the
request does not fit.
