# Recipe: choose the providers and their order

A rule's `fallback` list says which providers may answer its requests, and
in what order. Janus sends each request to the first that can take it, and
moves down the list when that one cannot answer. Four common shapes are
below; [CONFIG.md](../CONFIG.md#a-list-of-providers) has the details.

Every shape below is a single rule with no conditions, so it matches every
request. Put narrower rules above it if you have them: the first rule
that matches decides.

## Claude only

This is what Janus does with no rules at all: every request goes to
Claude, and a failure reaches Claude Code as Claude sent it.

```json
"features": { "route": false }
```

## OpenAI only

```json
"openai": { "key": "env:OPENAI_API_KEY" },
"routing": [
  { "name": "everything", "fallback": ["openai"] }
],
"features": { "route": true }
```

Put the key in `OPENAI_API_KEY` before starting Janus. Janus reads it once,
at startup, and won't start without it. Janus picks an OpenAI model for
each Claude model Claude Code asks for (`openai.models` in
[CONFIG.md](../CONFIG.md) changes them), and Claude Code is told the model
it asked for answered.

Claude Code still needs a Claude login, and auto mode's safety checks
stay with Claude unless a rule names `class: "safety"`. With no Anthropic
account at all, add `"upstream": {"off": true}`, and Janus sends nothing
to Anthropic: it answers Claude Code's start-up checks itself, `janus
claude` gives Claude Code a placeholder login, and the safety checks go
along the list too ([no Anthropic account](../CONFIG.md#no-anthropic-account)).

OpenAI support is built from OpenAI's published API reference and has not
yet been run against the real API.

## Local only

```json
"local": {
  "baseUrl": "http://127.0.0.1:1234",
  "models": { "janus-local": { "model": "qwen/qwen3.8-27b" } }
},
"routing": [
  { "name": "everything", "fallback": ["janus-local"] }
],
"features": { "route": true }
```

With nothing after it on the list, the local model waits for a free slot
rather than giving way. A conversation too long for it has its middle
left out to fit, as a stand-in does in [the outage recipe](outage.md). A
request it cannot take at all is refused with a `janus:` error saying
why, and is not sent anywhere else.

## Mixed

```json
"routing": [
  { "name": "everything", "fallback": ["upstream", "openai", "janus-local"] }
]
```

Claude answers while it can. When it is out of usage, or down for a
while, OpenAI answers. When OpenAI can't either, the local model does.
Each provider is left only for the failures that mean it can't answer for
a while. A brief one is passed to Claude Code to retry, so one blip
doesn't move the conversation.
[CONFIG.md](../CONFIG.md#a-list-of-providers) has the table of which
failures count.

The order is yours. `["openai", "upstream"]` puts OpenAI first.
`["upstream", "janus-local", "openai"]` tries the free local model before
the paid one.

![A rule's list of providers: a request goes to Claude first; if Claude cannot answer it moves to OpenAI, and if OpenAI cannot either, to the local model, each one passed over without being asked if the request may not go there or it is waiting for a limit to reset](../diagrams/provider-chain.svg#gh-light-mode-only)
![A rule's list of providers: a request goes to Claude first; if Claude cannot answer it moves to OpenAI, and if OpenAI cannot either, to the local model, each one passed over without being asked if the request may not go there or it is waiting for a limit to reset](../diagrams/provider-chain-dark.svg#gh-dark-mode-only)

## Moving and coming back

When Claude's usage limit is reached, its refusal says when the limit
resets. Every session then starts its list below Claude, without asking
it again, and goes back to Claude on the first turn after the reset.
Janus keeps the reset time in `limits.json` in its log folder, so a
restart doesn't send sessions back early. An OpenAI rate limit of a
minute or more is treated the same way. An outage has no reset time:
the provider is left alone for 30 seconds, then asked again, the wait
doubling up to 10 minutes while it keeps failing.

![Moving and coming back: Claude answers until its usage limit is reached; the 429 says when it resets; until then each turn goes to OpenAI without asking Claude; the first turn after the reset goes back to Claude, which is told how many turns OpenAI answered](../diagrams/move-and-return.svg#gh-light-mode-only)
![Moving and coming back: Claude answers until its usage limit is reached; the 429 says when it resets; until then each turn goes to OpenAI without asking Claude; the first turn after the reset goes back to Claude, which is told how many turns OpenAI answered](../diagrams/move-and-return-dark.svg#gh-dark-mode-only)

When Claude takes a conversation back, the console says how many turns
the others gave, since Claude reads them as its own.

## Choosing by hand

To start one session on another provider, through the same Janus:

```bash
janus claude -provider openai   # OpenAI first, then the rule's list ("codex" works too)
janus local                     # the local model only
```

To move sessions that are already running, from their next turn:

```bash
janus use codex                  # every session
janus use local -session <id>    # one, by the ledger's "session"
janus use -clear                 # back to the rules
```

A choice made with `janus use` ends by itself when its provider can't
answer.

## Keeping content from one provider

If your employer approves one provider and not the other, keep named
content from it, and a list goes on to the next provider for those
requests ([the guard](../GUARD.md#content-for-one-provider-and-not-another)):

```json
"features": { "guard": true },
"guard": { "keepFrom": { "openai": { "paths": ["clients/**"], "markers": ["CLIENT CONFIDENTIAL"] } } }
```

## Seeing what happened

When a request did not go to the first provider on its list, the console
says why on the line below it:

```
main       gpt-6.1-sol (openai)    ✓ 4.2s    answered    31k in · 0% cached    412 out
          ↳ Claude could not answer (upstream answered HTTP 429), so it went to OpenAI
```

The ledger record's `tried` lists each provider before the one that
answered, whether it failed or was passed over, and why
([LEDGER.md](../LEDGER.md)).

A conversation keeps its history as it moves: what each provider said
stays in it, and goes back to Claude in the same bytes Claude was sent
before, so Claude's prompt cache is rebuilt once per move rather than on
every turn.

What Janus changes when it sends a request to OpenAI, and what it drops,
is in [what Janus changes](../WHAT-JANUS-CHANGES.md#when-a-request-goes-to-openai).
`janus doctor` says which providers your rules use and whether OpenAI
answers.
