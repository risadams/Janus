# Spike: can a conversation mix local and upstream turns?

Issue: #7 · Date: 2026-10-11 · Status: one question answered by experiment; the rest still open

## Question

What does the Anthropic API do with a conversation in which some assistant
turns came from a local model?

## Method

No API key is available on this machine, so the API could not be probed
with hand-built requests as the issue planned. Instead a real Claude Code
session was run through Janus with a rule that sends only small subagent
turns local (`"class": "subagent", "maxTokens": 16500`). A subagent's
first turn is under that size and its second is over, so the conversation
starts on `qwen/qwen3.8-27b` and continues on Claude, with the local
turn in its history.

## Finding 1: unsigned thinking blocks are rejected

The second turn, sent upstream, was refused:

```
HTTP 400
{"type":"error","error":{"type":"invalid_request_error",
 "message":"messages.1.content.0: Invalid `signature` in `thinking` block"}}
```

The history Claude Code sent back contained the local turn as:

```json
[{"type":"thinking","thinking":"The user is asking to count the lines in…","signature":""},
 {"type":"tool_use","id":"587714761","name":"Bash", …}]
```

LM Studio returns thinking blocks with no `signature` field. Claude Code
stores the block and replays it with `"signature": ""`. The API signs the
thinking it produces and rejects a block whose signature is missing or
does not verify.

Claude Code recovered by starting the subagent again from scratch, so the
session finished, having paid for the failed attempt and a fresh
conversation.

**This affected shipped behaviour.** Any rule that can send part of a
conversation local and part upstream produced it: a `maxTokens`
condition, or a `local` rule falling back upstream when the conversation
outgrew the local model's context.

## Decision

Before a request goes upstream, Janus removes from its history any
thinking block with an empty or missing signature
(`backend.RepairForUpstream`, ledger stage `drop_unsigned_thinking`).

- Blocks the API signed are never touched, including the ones Claude Code
  receives with empty text under `display: "omitted"`; their signature is
  not empty. A conversation that only ever went to Claude is forwarded
  byte for byte, as before.
- The repair is on the request, on the way up, and not on the local
  response on the way down. That covers histories that already hold local
  thinking, and avoids rewriting block indexes in a live stream.
- Requests going to the local model keep the local thinking.

## Not yet verified

**Whether the API accepts the repaired request.** The repair was written
and tested against a fake upstream, but the re-run against the real API
could not be completed: DNS on the test network failed before it ran.
Until that run succeeds, treat the fix as untested where it matters.

Open questions the repaired run should answer:

1. Is an assistant turn with a tool call and no thinking block accepted
   mid-conversation when the request has thinking enabled?
2. Are LM Studio's tool-use ids, which are bare numbers
   (`"587714761"`), accepted in `tool_use` and `tool_result` blocks?
3. Does anything else about a local turn trip validation?

## Still open from the original spike

- Behaviour with thinking disabled.
- What a later Claude turn does, in quality terms, with a history that
  holds a local model's tool calls. Acceptance is not the same as working
  well.
- Routing granularity. Until the above is known, the safe unit is still a
  whole conversation: route a subagent local from its first turn and keep
  it there, which is what a plain `"class": "subagent"` rule does.
