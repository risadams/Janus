# Spike: what Claude Code sends

Issue: #5 · Date: 2026-10-11 · Status: complete for headless sessions; interactive sessions still to capture

## Question

What request classes does Claude Code produce, how big are they, how do
they use the cache, and which signals identify them reliably?

## Method

Three headless sessions (`claude -p`, Claude Code 2.1.296) run through
blush against a scratch copy of the blush source, with
`ENABLE_TOOL_SEARCH=true`:

| Session | Task | Requests to `/v1/messages` |
| --- | --- | --- |
| feature | Add a `-version` flag with test and docs | 13 |
| debug | Find and fix a seeded failing test | 4 |
| subagent | Architecture review using three parallel subagents | 19 (8 main, 11 subagent) |

36 Messages requests in total. This is a small sample of short tasks on a
small repo. It shows the shape of the traffic; it is not a baseline. That
is M1's job (#19).

**Gap: no interactive session was captured.** Headless mode identifies
itself as `cc_entrypoint=sdk-cli` and may skip work the interactive UI
does. Anything listed under "Not seen" could still exist there.

## Decisions

1. **Classify on headers first.** Subagent requests carry an
   `X-Claude-Code-Agent-Id` header that main-loop requests lack. That is a
   cleaner signal than fingerprinting the system prompt.
2. **Subagents are the first candidate for local routing**, ahead of
   utility calls, which did not appear at all. A subagent is a separate
   conversation with its own id, starts from a fresh history, and hands
   back plain text, so routing one locally never mixes origins inside a
   conversation.
3. **Requests must be normalised before LM Studio will accept them.**
   Real requests use wire features LM Studio rejects. See "Replay".
4. **Charter D1 is answered: subscription auth.** Requests carry an OAuth
   bearer token, so "spend" is rate-limit headroom, and dollar figures are
   list-price estimates.

## Request classes seen

| Class | How to recognise it | Tools | System prompt | Cache TTL |
| --- | --- | --- | --- | --- |
| Main loop | No `X-Claude-Code-Agent-Id` header | 11 | 3 blocks, about 9k characters | 1 hour |
| Subagent | `X-Claude-Code-Agent-Id` present; first system block contains `cc_is_subagent` | 8 | 3 blocks, about 2.7k characters | 5 minutes |
| Connectivity check | `GET /` and `GET /api/hello`, once each at session start, empty body | – | – | – |

Common to both Messages classes: model `claude-opus-5-5`, `stream: true`,
`max_tokens: 128000`, `thinking: {type: "adaptive", display: "omitted"}`,
`output_config: {effort: "medium"}`, `context_management` with a
`clear_thinking` edit.

### Not seen

- Any model other than the main one. No small/fast-model calls.
- `POST /v1/messages/count_tokens`.
- Title generation, summarisation, or compaction requests.
- Non-streaming Messages requests.

## Signals

| Signal | Where | Reliability |
| --- | --- | --- |
| `X-Claude-Code-Session-Id` | Header | One value per session, shared by the main loop and its subagents. Use as the session key. |
| `X-Claude-Code-Agent-Id` | Header | Present only on subagent requests; one value per subagent. Use as the conversation key. |
| `cc_is_subagent`, `cc_entrypoint`, `cc_version` | First system block, a one-line billing header | Confirms class and entry point. Text inside a prompt, so more likely to change than a header. |
| `metadata.user_id` | Body | A JSON string holding device, account, and session ids. Redundant with the headers; contains account identifiers, so keep it out of logs meant for sharing. |
| Tool count and system prompt text | Body | Differ by class but depend on the user's configuration. Fallback only. |
| `User-Agent`, `X-App` | Header | Identify Claude Code and its version; useful for noticing a release that changes the wire shape. |

## Size

| | Main loop | Subagent |
| --- | --- | --- |
| Request body | 50–129 kB | 44–105 kB |
| Largest context in any request (input + cache read + cache write) | 44,469 tokens | 38,178 tokens |
| Context on the first turn | about 17,400 tokens | about 16,200 tokens |

Every request fits inside the 61,696-token context that
`qwen/qwen3.8-27b` was loaded with. These were short tasks; long sessions
will grow past it, so the context check in FR-3.4 is still required.

With tool search on, tool schemas are about 22k characters for 11 tools.
Further tools arrive inside the conversation rather than in `tools`.

## Cache behaviour

- **Uncached input is effectively zero**: 2 to 4 tokens per request. All
  other input is a cache write or a cache read.
- **Four breakpoints at most.** Two on the system blocks, one at the end of
  the second message (after the injected tool definitions), and one that
  moves along the tail of the conversation.
- **The main loop writes with a 1-hour TTL, subagents with 5 minutes.**
- Each turn reads the previous prefix from cache and writes only the new
  tail, typically a few hundred to a few thousand tokens.

Token totals across all three sessions:

| | Requests | Cache write | Cache read | Output | of which thinking |
| --- | --- | --- | --- | --- | --- |
| Main loop | 25 | 96,200 (1h) | 719,306 | 15,208 | 940 |
| Subagent | 11 | 78,896 (5m) | 230,960 | 12,587 | 4,346 |

Subagents were 11 of 36 requests but 45% of cache-write tokens and 45% of
output tokens, because each one starts a new conversation and pays to write
its whole prefix.

**These are not priced.** Claude Code reported list-price costs of $0.49,
$0.26, and $1.28 for the three sessions. Applying the commonly quoted
multipliers (cache read 0.1x, 5-minute write 1.25x, 1-hour write 2x, output
5x input) to these token counts does not reproduce those figures with any
single base price; the implied base ranges from $3.42 to $4.09 per million.
The pricing model in #15 must be built from the published price list and
checked against Claude Code's own reported cost, not from those ratios.

### What this means for the charter

- **Compression (M5) has a narrow target.** With no uncached input, a
  transform can only save by shrinking what gets written to and read from
  cache, and it loses if it disturbs a prefix that is already cached. This
  is charter risk R1 seen in real numbers.
- **Routing whole subagent conversations avoids the cache entirely**, which
  is why it looks like the better first move.

## Wire features beyond the basic Messages API

All of these appeared in real requests or responses, and the typed model in
#11 has to carry them through untouched:

- Messages with `role: "system"` in the middle of a conversation, with
  string or block content.
- `tool_addition` content blocks that carry a tool definition inside a
  message, some with `cache_control`.
- `defer_loading` on tool definitions, and a placeholder tool for deferred
  tools.
- Top-level `context_management` and `output_config`.
- `cache_control` with a `ttl`.
- Thinking blocks that stream with empty text and a signature
  (`display: "omitted"`), via `thinking_delta` and `signature_delta`.
- `usage` with `cache_creation` split by TTL, `output_tokens_details`,
  `iterations`, `service_tier`, and `inference_geo`.
- `message_delta` with `stop_details` and `container`.
- `ping` events in the stream.
- Fourteen `anthropic-beta` values on every request.

## Replay against the local model

Two real first-turn requests were replayed to `qwen/qwen3.8-27b` through
LM Studio's `/v1/messages`, one run each.

**Unmodified, both were rejected** with HTTP 400:
`request.messages.1.content.1.type: Invalid literal value, expected "text"`.
LM Studio does not accept `tool_addition` blocks. Removing
`context_management`, `output_config`, `thinking`, and `metadata` made no
difference; those are tolerated.

**After normalising, both were accepted.** The normalisation: move each
`tool_addition` definition into `tools`, drop the block from the message,
and drop deferred placeholder tools.

| Request | Prompt tokens (local count) | Time to first token | Total | Local result | Claude's result |
| --- | --- | --- | --- | --- | --- |
| Subagent, turn 1 | 11,018 | 20 s | 53 s | Thinking, then four `Read` calls with correct absolute paths | One `Bash` call |
| Main loop, turn 1 | 15,151 | 26 s | 45 s | Thinking, then one `Bash` call | One `Bash` call |

So the local model can take a real Claude Code request and return valid
tool calls. It is slow to start: 20 to 26 seconds before the first token on
a cold prompt of this size, where Claude finished whole turns in 2 to 6
seconds. Whether LM Studio's prompt cache shortens later turns of the same
conversation was not measured, and nothing here says whether a local
subagent reaches a correct final answer. Both belong to #8.

## Follow-ups

- Capture an interactive session, and one long enough to trigger
  compaction, to fill the "Not seen" list. Needs to be run by hand.
- #22: add the `tool_addition` normalisation to the LM Studio backend.
- #23: classes are `main`, `subagent`, `connectivity`, `unknown` for now.
- #28: evaluate subagents as the first class to switch.
