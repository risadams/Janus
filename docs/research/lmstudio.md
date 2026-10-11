# Spike: LM Studio API surface

Issue: #6 · Date: 2026-10-11 · Status: complete

## Question

Can Janus send Messages-shaped requests to LM Studio as they are, or must it
translate to and from OpenAI chat completions?

## Decision

**Native.** LM Studio serves an Anthropic-compatible `POST /v1/messages`
that handles text, tool use, tool results, thinking, and streaming in the
Anthropic event format. Janus sends Messages requests to it directly and
does not build an OpenAI translation layer. Issue #22 shrinks to request
clean-up and response normalisation, listed under "What Janus must do".

## Setup

- LM Studio (the Bionic app) on an Apple M5 Max, 36 GB. The spike was run
  against `http://localhost:1234`, because the server was then bound to
  `127.0.0.1` only. Later the same day it was rebound to all interfaces and
  now also answers at `http://studio.tuesday.local:1234`, the address Janus
  uses (FR-3.5). A `/v1/messages` request through the hostname was checked
  and works.
- To serve on the network, set it in the Bionic app or in its own config
  file, `~/.lmstudio/apps/bionic/.internal/http-server-config.json`
  (`port`, `networkInterface`, `autoStartOnLaunch`), edited while the app is
  quit. Do not use `lms server start` with Bionic: it tries to wake a
  separate headless service, which takes local inference away from the app
  and stops its server.
- Models tried: `qwen/qwen3.8-27b` (MLX 4-bit) and
  `gemma-4-e4b-it-obliterated` (GGUF Q4_K_M). Both advertise `tool_use`.
- Every result below is from a hand-run `curl`, one or two runs each. These
  are observations of behaviour, not benchmarks.

## Endpoints

| Endpoint | Result |
| --- | --- |
| `POST /v1/messages` | Works, streaming and non-streaming |
| `POST /v1/messages/count_tokens` | Not implemented |
| `POST /v1/chat/completions` | Works |
| `GET /v1/models` | Lists every downloaded model, loaded or not |
| `GET /api/v0/models` | Adds `state` (`loaded` / `not-loaded`), `max_context_length`, `loaded_context_length`, `capabilities`, quantization |
| `GET /api/v1/models` | Same information, newer shape, with `loaded_instances` |
| `POST /api/v1/models/unload` | Works, body `{"instance_id": "<model id>"}` |

## What works on `/v1/messages` (qwen)

- **Text**, with `system` as a string or as an array of text blocks.
- **Tool use**: correct `tool_use` block, `stop_reason: "tool_use"`.
- **Streaming**: `message_start`, `content_block_start`,
  `content_block_delta`, `content_block_stop`, `message_delta`,
  `message_stop`, in order. Tool arguments arrive as `input_json_delta`.
- **Tool results**: a history containing a `tool_use` with an
  Anthropic-style `toolu_...` id and its `tool_result` was accepted and
  answered correctly.
- **Thinking**: `thinking: {type: "enabled", budget_tokens}` produced a
  `thinking` block followed by a `text` block.
- **Foreign thinking in history**: an assistant turn containing a
  `thinking` block with a made-up `signature` was accepted.
- **`tool_choice`** forcing a named tool was honoured (the model added a
  line of text before the call).
- **`stop_reason`** values `end_turn`, `max_tokens`, `stop_sequence` (with
  `stop_sequence` filled in), and `tool_use` all appeared when expected.
- **Claude Code extras are tolerated**: `cache_control` on system and
  message blocks, `metadata`, and `anthropic-beta` headers caused no error.

## Differences from the Anthropic API

| Behaviour | Consequence for Janus |
| --- | --- |
| An unknown `model` (tried `claude-haiku-5-5`) is not rejected. The loaded model answers, and the response's `model` field names the local model. | Janus must choose the local model explicitly, and rewrite `model` in the response to what the client asked for (#25). |
| A request for a model that is not loaded loads it on demand. The first request took about 11 s for both models. | First-request latency needs a long timeout or a warm-up; `janus doctor` should report what is loaded (#21). |
| `tool_use` ids are bare numbers (`"364900003"`), not `toolu_...`. | Fine while a conversation stays local. If a locally produced turn is ever replayed upstream, ids are one more thing that may not match. Feeds #7 and #31. |
| `thinking` blocks carry no `signature`. | Same: a local thinking block cannot be replayed upstream as-is. Feeds #7. |
| Tool arguments stream as one `input_json_delta` chunk rather than many. | None, if the SSE reader does not assume chunking (#12). |
| `usage` has `input_tokens`, `output_tokens`, `cache_read_input_tokens`. There is no `cache_creation_input_tokens`. `cache_read_input_tokens` was non-zero on repeated prompts, reflecting LM Studio's own prompt cache. | The ledger must treat missing usage fields as absent, and must not price local cache reads (#14, #15). |
| No `count_tokens`. | Token counting stays upstream, or Janus estimates. Decide in #23 when the class is seen in real traffic. |
| An unsupported path returns HTTP 200 with `{"error": "Unexpected endpoint or method..."}`. | Never trust the status code alone from LM Studio; check the body shape (#21). |
| Validation errors return HTTP 400 in Anthropic's shape (`{"type":"error","error":{"type":"invalid_request_error",...}}`). Malformed JSON returns 400 in a different, OpenAI-like shape. | Error handling must accept both shapes. |

## Model behaviour matters more than the API

`gemma-4-e4b-it-obliterated`, given a prompt that should produce a tool
call, returned HTTP 200, `stop_reason: "end_turn"`, an **empty `content`
array**, and a non-zero `output_tokens`. The same happened on
`/v1/chat/completions` (empty `content`, empty `tool_calls`, 139 completion
tokens), so the output is being lost between the model and LM Studio's
tool-call parsing, not in the Anthropic endpoint. In another run the same
model invented file contents as plain text instead of calling the tool.

This confirms charter risk R3 for small models, and it is the concrete case
the validators in #30 exist for: a response can be well-formed and
successful by every protocol signal and still contain nothing. "Not empty
when `output_tokens` is above zero" should be one of the first validators.

`qwen/qwen3.8-27b` had none of these problems in the cases tried.

## Capacity observations

- `qwen/qwen3.8-27b` supports a 262,144-token context but was loaded with
  **61,696**. The loaded value, not the maximum, is the real limit, and it
  is what FR-3.4 must check. Whether that is large enough for any of Claude
  Code's request classes is a question for the traffic spike (#5).
- One streamed 300-token generation on qwen: first token after 0.3 s, about
  26 tokens/s. A single run on a short prompt; the model benchmark (#8)
  should measure this properly with realistic prompt sizes.

## What Janus must do

1. Send Messages requests natively to `/v1/messages`; no OpenAI translation.
2. Set `model` to a configured local model on the way in, and restore the
   requested name on the way out.
3. Read `loaded_context_length` from `/api/v0/models` (or the `/api/v1`
   equivalent) and refuse requests that exceed it.
4. Treat HTTP 200 as necessary but not sufficient: check for an error body
   and for empty content.
5. Keep `usage` parsing tolerant of missing fields.

## Later observations (2026-10-11, through Janus)

- **`input_tokens` includes cached tokens.** A turn reported
  `input_tokens: 3468` with `cache_read_input_tokens: 2048`. The Anthropic
  API reports the two apart. The ledger subtracts.
- **The prompt cache helps later turns.** In a subagent conversation the
  first turn took 24 s and the following ones 7 to 14 s, with most of the
  prompt read from cache.
- **Real Claude Code subagents ran to completion on `qwen/qwen3.8-27b`**:
  four to six tool-using turns, correct final answer, on a trivial task.

## Not tested

- Images and documents in content blocks.
- Parallel tool calls in one turn.
- Requests at realistic Claude Code size (very large system prompt, dozens
  of tool schemas). This is the most important gap and belongs to #8.
- Behaviour when a request exceeds the loaded context.
- Concurrent requests.
- Whether stripping `cache_control` and beta headers changes anything; they
  were tolerated, so Janus can leave them in for now.
