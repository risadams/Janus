# Spike: can a conversation mix local and upstream turns?

Issue: #7 · Date: 2026-10-11 · Status: the main question answered by experiment; quality questions still open

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

## Finding 2: with the repair, the mixed conversation is accepted

The same session was run again with the repair in place. One subagent
conversation, two turns:

| Turn | Answered by | What happened |
| --- | --- | --- |
| 1 | `qwen/qwen3.8-27b` | Thinking, text, and a `Bash` tool call with id `"..."` as a bare number |
| 2 | Claude, after `drop_unsigned_thinking` | HTTP 200. Claude read the tool result and finished the task correctly. |

So, for this case:

1. **An assistant turn with a tool call and no thinking block is accepted**
   mid-conversation, with adaptive thinking enabled on the request.
2. **LM Studio's numeric tool-use ids are accepted** in `tool_use` and the
   matching `tool_result`.
3. Nothing else about the local turn tripped validation.

This is one run of a two-turn conversation on one model. It shows the
repair removes the failure that was observed; it does not show that no
other shape of local turn can fail.

## Still open from the original spike

- Behaviour with thinking disabled.
- What a later Claude turn does, in quality terms, with a history that
  holds a local model's tool calls. Acceptance is not the same as working
  well.
- A local turn that is only thinking, with no text or tool call. The
  repair replaces it with a placeholder text block; that path is tested
  against a fake upstream only.
- Routing granularity. A conversation can now cross from local to
  upstream without failing, which is what escalation needs. Whether it
  should, turn by turn, is a quality question that shadow and replay data
  on real work will have to answer.
