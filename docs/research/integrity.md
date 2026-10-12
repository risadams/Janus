# Spike: can a conversation mix local and upstream turns?

Issue: #7 · Date: 2026-10-11, case by case 2026-10-12 · Status: complete

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

## Finding 3: case by case

On 2026-10-12 each kind of mixed history was tried on its own. A
throwaway pass-through proxy sat between headless Claude Code (2.1.296,
`claude -p --effort high`, model `claude-opus-5-5`) and the API. Claude
Code authenticated as usual with the machine's Pro subscription; the
proxy forwarded the auth header untouched and never read or stored it.
On each request that already held assistant turns, it made one change to
the history, the kind a local model's turn would bring, and recorded the
API's status, error, and usage.

The task was a short tool loop over a scratch repo: three file reads, one
of them needing worked reasoning, so that the history held signed thinking
blocks (`display: "omitted"`: empty text and a real signature) once the
model had thought.

| Change to the history | API answer |
| --- | --- |
| None (control) | 200 |
| Signature field removed from the last turn's thinking block | 400 `messages.7.content.0.thinking.signature: Field required` |
| Ten characters of a real signature altered | 400 ``Invalid `signature` in `thinking` block`` |
| Thinking text replaced with a local model's, signature `""` | 400 ``Invalid `signature` in `thinking` block`` |
| Signed thinking removed from the last assistant turn | 200 |
| Signed thinking removed from every assistant turn | 200 |
| Last turn made local-shaped: text first, no thinking, tool ids `call_…` | 200 |
| Every tool id renamed to `call_…`, in `tool_use` and `tool_result` | 200 |
| `thinking: {type: "disabled"}` | 400 `"thinking.type.disabled" is not supported for this model. Use "thinking.type.adaptive" and "output_config.effort"` |

What this adds to findings 1 and 2:

- **The API checks every signature it is shown, and nothing else about
  origin.** A thinking block must carry a signature the API issued, for
  that text. Tool ids in any format, a turn with no thinking, and text
  before a tool call all pass.
- **Removing signed thinking is accepted, but not free.** In the turn
  after the removal, Claude worked the reasoning out again: 840 and 875
  output tokens with a fresh thinking block, against 117 with no thinking
  at the same step of the control. One run each, and adaptive thinking
  varies from run to run, so read this as a direction, not a size.
- **Thinking off is not an option on this model.** The "thinking on and
  off" half of the original question does not arise for
  `claude-opus-5-5`; every request must use adaptive thinking.
- **Claude Code repairs a rejected history itself.** All three times a
  signature was refused, Claude Code sent the request again without the
  thinking blocks in the failing turn, and that succeeded. So a mistake
  here costs a failed request rather than a failed turn, which also means
  it is easy to miss without the ledger.

## Routing granularity

| Granularity | Safe? | Why |
| --- | --- | --- |
| Per request class | Yes | Nothing crosses between conversations. |
| Per whole subagent conversation | Yes | A subagent's conversation never mixes origins. |
| Local, then upstream within a conversation (escalation, fallback) | Yes, with `drop_unsigned_thinking` | The only history the API rejects is thinking it did not sign, and the repair removes exactly that. |
| Turn by turn in the main loop | Accepted by the API; not recommended | No longer an integrity question. Each local turn costs Claude its reasoning for the next turn (finding 3), and a local turn takes 10 to 50 seconds where Claude takes 2 to 8 (#8). Whether that ever pays is for shadow and replay data on real work to show. |

FR-4.4 is met by the repair: no routing Janus can do produces a history
the API rejects, as long as no local thinking block is ever sent upstream
as a thinking block.

## Still open

- What a later Claude turn does, in quality terms, with a history that
  holds a local model's tool calls. Acceptance is not the same as working
  well.
- A local turn that is only thinking, with no text or tool call. The
  repair replaces it with a placeholder text block; that path is tested
  against a fake upstream only.
