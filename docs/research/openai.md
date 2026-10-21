# Spike: Claude Code's traffic on OpenAI's Responses API

Issue: #368 · Date: 2026-10-21 · Status: complete

## Question

Claude Code speaks the Anthropic Messages API. To let an OpenAI model
answer it (FR-11), Janus has to translate each request into OpenAI's
Responses API and each answer back. What carries over field by field,
what cannot, what the prompt cache needs, and which models are
candidates?

## Method

Nothing was sent to OpenAI: the owner has no account (charter D7). The
mapping is built from three sources, all read on 2026-10-21:

- OpenAI's published API reference and guides, in their Markdown form
  under `developers.openai.com`, listed under [Sources](#sources).
  `platform.openai.com` refused the fetch (HTTP 403); the same pages are
  served from `developers.openai.com`.
- The open-source Codex CLI, [openai/codex](https://github.com/openai/codex)
  at commit `d138398cef` (2026-10-21), as evidence of what a real client
  sends and how it reads the answers.
- Janus's own record of what Claude Code sends:
  [traffic.md](traffic.md), [integrity.md](integrity.md),
  [lmstudio.md](lmstudio.md), and `internal/backend` (`Normalize`,
  `RepairForUpstream`).

Where the docs say nothing, contradict themselves, or describe something
only real traffic can confirm, the row is marked **#399**, the owner's
validation sessions.

## Decision

The translation is feasible, and closer than expected: OpenAI's newer
models cache with explicit breakpoints, up to four writes per request,
much as Anthropic's do. The Codex CLI already sends what Janus should
send. In short:

- **Every request** goes with `store: false`, no `previous_response_id`,
  `truncation: "disabled"`, `stream` as Claude Code asked, and the full
  translated history. That is exactly what the Codex CLI sends
  (`codex-rs/core/src/client.rs`, `build_responses_request`). It costs
  nothing extra in tokens: input is billed in full even when chaining with
  `previous_response_id` ([conversation state][state]).
- **Caching** uses explicit mode on GPT-5.6 and later: each Anthropic
  `cache_control` becomes a `prompt_cache_breakpoint` on the same block,
  so the four breakpoints Claude Code places become OpenAI's four writes.
  `prompt_cache_key` is a hash of Claude Code's session id.
- **Candidates**, defaults for #374 to confirm with the owner: `gpt-6.1-sol`
  for Opus and Sonnet requests, `gpt-6-luna` for Haiku, `gpt-6-astra` by
  choice. Effort maps one to one onto `reasoning.effort`, clamped to what
  the model accepts. The dedicated `-codex` models stop at `gpt-5.3-codex`,
  which has a 400k window, a 2025 knowledge cutoff, and no explicit cache
  breakpoints. Codex itself now recommends `gpt-6.1-sol` ([Codex
  models][codexmodels]).

What each later issue should build:

| Issue | Build |
| --- | --- |
| #369 fake server | `/v1/responses` streaming the events in [Response stream](#response-stream), with `encrypted_content` complete only on `response.output_item.done`, `obfuscation` fields on deltas, and `sequence_number`; `response.completed`, `response.incomplete`, `response.failed`, and the mid-stream `error` event; HTTP errors in OpenAI's shape with the codes in [Errors](#errors); rate-limit headers with duration values (`6m0s`); and `/v1/responses/input_tokens` |
| #372 request | The tables below. Above all: system blocks become `developer` messages, not `instructions`; `tool_addition` blocks become `additional_tools` items in place, not lifted into `tools`; tool names outside `[A-Za-z0-9_]{1,64}` are encoded with a hash suffix and decoded from the request's own tool list; `strict: false` on every function; Claude's thinking blocks are left out |
| #373 response | Events mapped as in [Response stream](#response-stream); usage mapped so that Claude Code's totals stay right; an OpenAI 401 or 403 is never passed on as one, because Claude Code would take it as its own login failing |
| #374 models | The model and effort tables in [Models](#models) |
| #375 history | Going to OpenAI, Claude's thinking and redacted thinking are dropped; going to Claude, every thinking block Janus made is dropped, and `RepairForUpstream` must learn to recognise Janus's own envelope as well as empty signatures (#410) |
| #376 no account | `count_tokens` is answered by OpenAI's `/v1/responses/input_tokens` on the translated request, with the estimate as a fallback; the start-up quota check is answered by Janus without calling OpenAI; requests with Anthropic server tools skip OpenAI |
| #407 window fit | Every GPT-6 and GPT-5.6 candidate has a 1,050,000-token window, larger than Claude's, so fitting matters little on OpenAI. Past 272K input tokens the whole request costs twice the input and cache price and 1.5 times the output price; the ledger must price that and the console should show it |
| #410 reasoning | Feasible: return each response's reasoning items, and the `phase` of its assistant messages, inside a thinking block whose signature is a Janus envelope; send them back to OpenAI on later turns to the same model family; drop them on the way to Claude |

## Request

### Top level

| Anthropic (as Claude Code sends it) | Responses | Notes |
| --- | --- | --- |
| `model` | `model` | Mapped per [Models](#models); the requested name is restored in the answer, as for LM Studio |
| `max_tokens` (128000) | `max_output_tokens` | Includes reasoning tokens on OpenAI ([reasoning][reasoning]); capped at the model's 128,000 |
| `stream` | `stream` | The quota check is the one non-streaming request Claude Code sends |
| `system` (array of text blocks) | Leading `developer` message(s), one `input_text` per block | Not `instructions`: an explicit breakpoint cannot go there ([caching][caching]). The first block, Claude Code's one-line billing header (`x-anthropic-billing-header: cc_version=…`), is left out: it means nothing to OpenAI |
| `messages` | `input` items, in order | See [Messages](#messages) |
| `tools` | `tools`, `type: "function"` with `name`, `description`, `parameters`, `strict: false` | Tools marked `defer_loading` are left out and added where a tool search loads them; see [Tool search](#tool-search) |
| `tool_choice: {type: auto}` / `any` / `none` / `{type: tool, name}` | `"auto"` / `"required"` / `"none"` / `{type: "function", name}` | [function calling][fc] |
| `tool_choice.disable_parallel_tool_use: true` | `parallel_tool_calls: false` | Otherwise left unset (default true) |
| `thinking` (`adaptive`, `display: "omitted"`) | `reasoning.effort` from `output_config.effort`; no `reasoning.summary` | With `display: "summarized"`, `reasoning.summary: "auto"` and the summary streams as thinking text |
| `thinking` (`enabled`, `budget_tokens`) | `reasoning.effort` by budget | See [Models](#models) |
| `output_config.effort` | `reasoning.effort` | One to one, clamped |
| `output_config.format` (JSON schema) | `text.format` (`json_schema`) | Not seen from Claude Code; translated if it comes |
| `temperature`, `top_p`, `top_k` | Left out | Claude Code sends none with thinking on; OpenAI's reasoning models are not documented to honour them |
| `stop_sequences` | Left out, and recorded in the ledger | The Responses API has no stop parameter (create reference); `stop_reason: "stop_sequence"` never occurs |
| `metadata.user_id` | Left out | It holds account and device ids ([traffic.md](traffic.md)). `safety_identifier` and `metadata` are not sent |
| `cache_control` on blocks | `prompt_cache_breakpoint: {mode: "explicit"}` on the same block, `prompt_cache_options.mode: "explicit"` | See [Prompt cache](#prompt-cache) |
| `context_management` (`clear_thinking`) | Left out; `reasoning.context` left at the model's default | OpenAI's `context_management` means server-side compaction, which would rewrite the prefix ([compaction][compaction]) |
| `service_tier`, `container`, `mcp_servers`, `inference_geo` | Left out | Anthropic-specific |
| `anthropic-beta`, `anthropic-version`, `x-api-key`, `Authorization` | Not sent | `Authorization: Bearer` carries Janus's OpenAI key (ADR-005) |
| `X-Claude-Code-Session-Id` | `prompt_cache_key` = first 32 hex characters of SHA-256 of the session id | A random id, hashed anyway so OpenAI never sees Claude Code's own value |
| (none) | `store: false`, `truncation: "disabled"` | `store` defaults to true, which keeps the response for at least 30 days ([create][create]); `truncation: "auto"` would drop the start of the conversation silently |

`include: ["reasoning.encrypted_content"]` is no longer needed: with
`store: false`, reasoning items carry `encrypted_content` by default
([reasoning][reasoning]). The Codex CLI still sends it, and the API still
accepts it, so Janus sends it too, in case an organization's setting
makes the default differ (**#399**).

### Messages

| Anthropic | Responses | Notes |
| --- | --- | --- |
| `user` message, string or `text` blocks | `{role: "user", content: [{type: "input_text", text}]}` | A string becomes one `input_text` block, the same way every turn |
| `image`, `source.type: base64` | `{type: "input_image", image_url: "data:<media_type>;base64,<data>"}` | PNG, JPEG, WEBP, and non-animated GIF; up to 512 MB per request ([vision][vision]) |
| `image`, `source.type: url` | `{type: "input_image", image_url: <url>}` | |
| `image` or `document`, `source.type: file` | Not translatable | An Anthropic Files API id means nothing to OpenAI; the request skips OpenAI |
| `document`, PDF, base64 | `{type: "input_file", filename: "document.pdf", file_data: "data:application/pdf;base64,…"}` | PDFs give the model both text and page images ([file inputs][files]). `title`, `context`, and `citations` are left out |
| `document`, plain-text source | `input_text` | |
| `system` message mid-conversation | `{role: "developer", …}` at the same place | Not folded into the user message as for LM Studio: OpenAI accepts developer messages anywhere, and an explicit breakpoint keeps it a cache boundary ([caching][caching]) |
| `assistant` `text` block | `{type: "message", role: "assistant", content: [{type: "output_text", text}]}` | `phase` only if #410 carried it |
| `assistant` `tool_use` | `{type: "function_call", call_id: <id>, name, arguments: <input as JSON>}` | `call_id` is the Anthropic id unchanged; no `id` field (optional) |
| `user` `tool_result` | `{type: "function_call_output", call_id: <tool_use_id>, output}` | `output` is a string, or an array of `input_text`, `input_image`, `input_file` when the result has images ([fc][fc]) |
| `tool_result.is_error: true` | The text, prefixed `Error:` | OpenAI has no error flag on a function output |
| Text beside tool results in one user message | A `user` message after the outputs | Outputs must follow their calls |
| `assistant` `thinking`, `redacted_thinking` (Claude's) | Left out | Signed for Anthropic; OpenAI cannot use them. Left out the same way every turn, so the prefix is stable |
| `assistant` `thinking` carrying Janus's envelope | The reasoning items inside it, in place | #410, same model family only |
| `tool_addition` block | `{type: "additional_tools", role: "developer", tools: […]}` at the same place | Lifting it into `tools`, as LM Studio needs, would change the start of every later request and break the cache. `additional_tools` exists for this ([tool search][toolsearch]) |
| `tool_reference` in a tool result | Text naming the tool, then an `additional_tools` item holding its definition, straight after the output | See [Tool search](#tool-search) |

Several tool calls in one assistant turn become several `function_call`
items in order, and their results the matching outputs in the same order.

### Tool names and schemas

- **The docs give no limit on a function's name.** The only stated rule
  is for a response format's name: `a-z`, `A-Z`, `0-9`, underscores and
  dashes, at most 64 characters ([create][create]). The Codex CLI
  replaces every character outside `[A-Za-z0-9_]` with `_` and keeps names
  to 128 characters, adding a 12-character SHA-1 suffix when it has to
  shorten or two names collide (`codex-rs/codex-mcp/src/mcp/mod.rs`,
  `sanitize_responses_api_tool_name`; `codex-mcp/src/tools.rs`,
  `MAX_TOOL_NAME_LENGTH`).
- **What Claude Code sends**: its own tools have plain names (`Bash`,
  `Read`, `Edit`, …). MCP tools are `mcp__<server>__<tool>`, and server
  names can hold dashes, as in `mcp__claude-in-chrome__computer`.
- **Decision**: a name that matches `^[A-Za-z0-9_]{1,64}$` goes as it is.
  Any other is encoded: each other character becomes `_`, the result is
  cut to 51 characters, and `_` and the first 12 hex characters of the
  SHA-1 of the original name are added. On the way back, a call's name is
  decoded from the tools in the same request, so no table is kept across
  turns or restarts. The 64-character limit is the stricter of the two
  documented rules, and whether OpenAI accepts dashes is **#399**.
- **Schemas go with `strict: false`.** Strict mode needs
  `additionalProperties: false` on every object and every property
  `required` ([fc][fc]). Claude Code's schemas have optional fields, and
  making them nullable-required would change what the tools accept. If
  `strict` is left out, Responses tries to convert the schema to strict
  itself, which could change it without notice. The Codex CLI sends
  `strict: false` for every tool (`codex-rs/tools/src/responses_api.rs`).
  The structured-outputs guide contradicts itself on whether `pattern`
  and `format` work in strict mode ([structured outputs][so]); with
  strict off it does not matter.

### Tool search

Claude Code's tool search is an ordinary tool (`ToolSearch`) whose result
holds `tool_reference` blocks. The Anthropic API expands each one into
the definition of a tool that was sent with `defer_loading`. OpenAI has a
tool search of its own, but in it the model, not a client tool, asks for
the search ([tool search][toolsearch]). So Janus does the expansion
itself, in the history:

1. Tools marked `defer_loading` are not sent in `tools`.
2. A `tool_result` holding `tool_reference` blocks becomes a
   `function_call_output` with text naming each tool, as for LM Studio
   (#169). Straight after it comes an `additional_tools` item with the
   definitions of the tools named.

The result depends only on that point in the history, so each turn sends
the same bytes. OpenAI loads tools at the point they appear in the input,
which "allows the model's cache to be preserved from one request to
another" ([tool search][toolsearch]). `additional_tools` items do not
accept an explicit breakpoint ([caching][caching]); the next message's
breakpoint covers them.

### Prompt cache

What OpenAI's cache needs ([caching][caching]):

- **The same rendered prefix.** Anything changed before a breakpoint
  misses from there on. That covers `model`, `tools` (including order),
  `parallel_tool_calls`, `reasoning.effort`, and the text format.
- **A breakpoint.** On GPT-5.6 and later, explicit breakpoints go on
  content blocks (`input_text`, `input_image`, `input_file`, and the
  parts of a function output), up to four writes per request, with the 2
  first and 50 latest explicit breakpoints checked on lookup. In implicit
  mode OpenAI places one at the end of the latest eligible message.
  Earlier models, `gpt-5.3-codex` among them, cache only implicitly, at
  fixed intervals, helped by `prompt_cache_key`.
- **At least 1,024 visible tokens** before the breakpoint on GPT-5.6 and
  later.
- **Prices** on GPT-5.6 and later: a write costs 1.25 times uncached input
  for the default 30-minute lifetime and 2.5 times for 12 hours; a read
  0.1 times (0.05 times on `gpt-6.1-sol`).

How the translation keeps it:

- **Determinism.** Every rule above maps the same Anthropic history to
  the same bytes: no timestamps, no counters, no map iteration order, a
  string message always becoming the same block, and dropped blocks
  dropped every time. #372's test that turn N+1's prefix equals turn N's
  is the guard.
- **Breakpoints follow Claude Code's.** Claude Code places at most four
  `cache_control` marks: two on the system blocks, one after the injected
  tool definitions, one moving along the tail ([traffic.md](traffic.md)).
  Each becomes an explicit breakpoint on the same block, in explicit
  mode, so OpenAI writes where Claude would and nowhere else. A mark on a
  block that is left out (the billing header, a thinking block) moves to
  the nearest earlier block that is kept.
- **Lifetimes.** Claude's 5-minute and 1-hour marks both become
  `"30m"`. A 1-hour mark could become `"12h"`, at twice the write price.
  The reference allows only `"30m"`, while the caching guide offers
  `"12h"` in the Responses API (create reference against
  [caching][caching]): **#399**, and `"30m"` until then.
- **Effort stays put.** A change of effort mid-conversation rewrites
  OpenAI's hidden instructions. GPT-6 models offer a `configuration_update`
  input item that changes effort without that ([reasoning][reasoning]);
  Claude Code keeps one effort per session, so Janus does not need it yet.

### What has no equivalent

| Claude Code sends | Decision |
| --- | --- |
| `POST /v1/messages/count_tokens` | Translate the request and call `POST /v1/responses/input_tokens`, which returns `input_tokens` ([input tokens][inputtokens]). Its price and rate limit are not documented (**#399**); if it fails, Janus estimates as the stand-in does (bytes / 3) and the ledger says so |
| The start-up quota check (`max_tokens: 1`, one message `quota`) | Answered by Janus without calling OpenAI. It exists to read Anthropic's rate-limit headers, which OpenAI cannot supply |
| Anthropic server tools (`web_search_*`, `web_fetch_*`, code execution), whose results come back as Anthropic-specific blocks | The request skips OpenAI and goes to the next provider; with no other, a `janus:` error says the tool needs Claude. OpenAI's hosted `web_search` returns a different shape, and translating it is not worth it before it is seen in Claude Code's traffic, where it has not been captured yet |
| `tool_reference` | Expanded by Janus, as above |
| `cache_control` | Becomes `prompt_cache_breakpoint`, as above |
| `context_management` (`clear_thinking`) | Left out; OpenAI applies its own reasoning context per model |
| `anthropic-beta` values | Not sent. Each beta's effect is in the body fields above or is Anthropic-only |
| `metadata.user_id` | Not sent |
| Files API ids in `source.type: file` | The request skips OpenAI |
| `stop_sequences` | Not sent; recorded |

## Response

### Response stream

The Responses stream is typed events with a `sequence_number`
([streaming events][events]). The Codex CLI dispatches on the `type` in
each event's data (`codex-rs/codex-api/src/sse/responses.rs`), and Janus
should too, not on the SSE `event:` line, whose presence the docs do not
show (**#399**).

| Responses event | Anthropic event(s) |
| --- | --- |
| `response.created` | `message_start`: `id` made from the response id, `model` the name Claude Code asked for, zero usage |
| `response.in_progress`, `response.queued`, `response.content_part.added`/`done`, `response.output_text.done` | Nothing |
| `response.output_item.added`, `reasoning` | `content_block_start` with `thinking` (empty text) |
| `response.reasoning_summary_text.delta` | `thinking_delta`, only when a summary was asked for |
| `response.output_item.done`, `reasoning` | `signature_delta` carrying the envelope (#410), or `""` without it, then `content_block_stop`. `encrypted_content` is complete only here ([create][create], Reasoning item) |
| `response.output_item.added`, `message` | `content_block_start` with `text` |
| `response.output_text.delta` | `text_delta` |
| `response.refusal.delta` | `text_delta`; the stop reason becomes `refusal` |
| `response.output_item.done`, `message` | `content_block_stop` |
| `response.output_item.added`, `function_call` | `content_block_start` with `tool_use`: `id` = `call_id`, `name` decoded, `input: {}` |
| `response.function_call_arguments.delta` | `input_json_delta` |
| `response.output_item.done`, `function_call` | `content_block_stop` |
| `response.completed` | `message_delta` with the stop reason and full usage, then `message_stop` |
| `response.incomplete` | The same, with the stop reason from `incomplete_details.reason` |
| `response.failed`, `error` | An Anthropic `error` event; see [Errors](#errors) |

Deltas carry an `obfuscation` field of random characters by default
([create][create], `stream_options`); it is ignored. A non-streaming
response is the same `output` array, assembled into one Messages body.

### Stop reasons

| Responses | Anthropic `stop_reason` |
| --- | --- |
| `completed`, last output item a `function_call` | `tool_use` |
| `completed`, otherwise | `end_turn` |
| `incomplete`, `max_output_tokens` | `max_tokens` |
| `incomplete`, `content_filter`; a refusal part; a `response.failed` with a policy code (`cyber_policy`, `bio_policy`, `misalignment_policy_violation`) | `refusal`, with OpenAI's message as text |
| `incomplete`, `max_messages` or `steered` | `end_turn`. Neither arises: both belong to features Janus does not use |

### Usage

| Responses `usage` | Anthropic `usage` |
| --- | --- |
| `input_tokens − cached_tokens − cache_write_tokens` | `input_tokens` |
| `input_tokens_details.cached_tokens` | `cache_read_input_tokens` |
| `input_tokens_details.cache_write_tokens` | `cache_creation_input_tokens` |
| `cache_write_tokens_details.ttl_30m_tokens` / `ttl_12h_tokens` | `cache_creation.ephemeral_5m_input_tokens` / `ephemeral_1h_input_tokens` |
| `output_tokens` (reasoning included) | `output_tokens` (thinking is included there too) |
| `output_tokens_details.reasoning_tokens` | Ledger only |

OpenAI's `input_tokens` includes cached and written tokens
([caching][caching]), as LM Studio's does and Anthropic's does not. The
four Anthropic input fields must add up to the true total, because
Claude Code judges how full its context is from them. The ledger (#381)
keeps OpenAI's own numbers and prices them with OpenAI's rates; the costs
Claude Code shows for those turns will be wrong, since it prices them as
Claude.

### Errors

An HTTP error's body is `{"error": {"type", "code", "message", "param"}}`.
The same object arrives mid-stream in a `response.failed` event's
`response.error`, or as an `error` event ([streaming events][events]).
The Codex CLI reads both (`codex-rs/codex-api/src/sse/responses_error.rs`,
`api_bridge.rs`).

| OpenAI | Meaning | To Claude Code (Anthropic shape, message starting `janus:`) | To the chain (#377) |
| --- | --- | --- | --- |
| 400, `context_length_exceeded` | Too long for the model | 400 `invalid_request_error`, saying the conversation is too long for the OpenAI model. Whether Claude Code then offers to compact depends on the wording it looks for (**#399**) | Next provider, if one fits |
| 400, other | Bad request: most likely a translation bug, which the ledger records | With no next provider, 400 `invalid_request_error`, with OpenAI's message | Moved: Claude and the local model never see the translation |
| 401, 403 | The OpenAI key was refused, or the region is not served | 500 `api_error`, saying OpenAI refused the key Janus reads and where it comes from. **Never 401 or 403**: Claude Code would take it as its own login failing | Out until restart or config change |
| 429, `rate_limit_exceeded`, `slow_down` | A per-minute limit | With no next provider, 429 `rate_limit_error`, passing on `retry-after` | Moved for the time `Retry-After` or the reset headers give |
| 429, `insufficient_quota`, `credit_balance_exhausted`, `organization_spend_limit_exceeded`, `project_spend_limit_exceeded`, `organization_usage_limit_exceeded` | Money or a monthly limit; retrying does not help ([errors][errors]) | With no next provider, 429 `rate_limit_error`, saying which and that it needs the owner | Out until the owner acts; no reset time is given |
| 500 | OpenAI's fault | 500 `api_error` | Moved, under the #150 rules |
| 503, `server_is_overloaded` | Overloaded | 529 `overloaded_error` | Moved, under the #150 rules |
| Mid-stream `response.failed` or `error` | As above, by code | An Anthropic `error` event, then the stream ends | As above |

### Rate-limit headers

OpenAI sends `x-ratelimit-limit-`, `-remaining-`, and `-reset-requests`
and `-tokens`, sometimes project-scoped versions of the token headers,
and `Retry-After` on a temporary 429 or 503 ([rate limits][ratelimits]).
These differ from Anthropic's in two ways that matter to #378 and #382:

- **The limits are per minute.** There is no five-hour or weekly usage
  as with Claude's subscription. The reset values are durations from now
  (`1s`, `6m0s`), not times, so Janus converts them on receipt.
- **The limits that end a session have no reset.** Credit, spend, and
  usage limits come back as 429 with a code and no reset header; they last
  until the owner acts or the month turns.

So on OpenAI, "return when the limit resets" applies to per-minute
limits only, and those pass in seconds or minutes. A quota error takes
OpenAI out of the chain until restart, and doctor says why.

## Models

From the model pages, read 2026-10-21. Prices are per million tokens, at
the standard tier.

| Model | Window | Max output | Effort accepted | Input | Cached input | Output | Explicit breakpoints |
| --- | --- | --- | --- | --- | --- | --- | --- |
| [`gpt-6-astra`][astra] | 1,050,000 | 128,000 | low … max | $10 | $1 | $50 | Yes |
| [`gpt-6.1-sol`][sol61] | 1,050,000 | 128,000 | low … max | $2 | $0.10 | $10 | Yes |
| [`gpt-6-luna`][luna] | 1,050,000 | 128,000 | none … max | $0.10 | $0.01 | $0.50 | Yes |
| [`gpt-5.6-sol`][sol56] | 1,050,000 | 128,000 | none … max | $4 (promotional, at least to 2026-11-21) | $0.40 | $20 | Yes |
| [`gpt-5.3-codex`][codex53] | 400,000 | 128,000 | low … xhigh | $1.75 | $0.175 | $14 | No |

On the GPT-6 and GPT-5.6 pages, cache writes cost 1.25 times input, and
a prompt over 272K input tokens is priced at twice the input and cache
rates and 1.5 times output for the whole request.

**Which models** (#374). The owner asked for "Codex". The `-codex` line
of models ends at `gpt-5.3-codex`, which is smaller, older, and caches
only implicitly. The Codex CLI itself now points its users at
`gpt-6.1-sol` ([Codex models][codexmodels]). Proposed defaults, to be
confirmed with the owner:

| Claude Code asks for | OpenAI model |
| --- | --- |
| an Opus or Fable model | `gpt-6.1-sol` |
| a Sonnet model | `gpt-6.1-sol` |
| a Haiku model | `gpt-6-luna` |
| anything else | the configured default |

`gpt-6-astra` costs five times as much as `gpt-6.1-sol` and is offered by
configuration only.

**Effort** (#374). Claude Code's `output_config.effort` maps one to one
onto `reasoning.effort` (`low`, `medium`, `high`, `xhigh`, `max`),
clamped to the nearest value the model accepts ([reasoning][reasoning]).
Where a request gives a thinking budget instead, the budget picks the
effort: under 4,000 tokens `low`, under 16,000 `medium`, under 32,000
`high`, otherwise `xhigh`. Thinking off becomes the model's lowest effort.
These thresholds are a starting point, not a measurement.

## Carrying reasoning back (#410)

Without its earlier reasoning, an OpenAI model starts each turn's
thinking from scratch. The docs recommend passing reasoning items back,
at least those since the last user message, when calling tools
([reasoning][reasoning]). It is possible statelessly:

- With `store: false`, each reasoning item in `output` has
  `encrypted_content`, and sending the items back lets the model use
  them; GPT-5.6 and later do so on every turn by default
  (`reasoning.context: "all_turns"`).
- Reasoning carries only within one model family: `gpt-5.6-sol` and
  `gpt-5.6-luna` share, GPT-5.6 and GPT-5.5 do not. Items from another
  family are ignored, not rejected.
- An item must be sent back as it came, with its `id`; the docs' own
  example keeps "every output item" (**#399** for whether `id` is needed
  with `store: false`).
- On GPT-5.5 and GPT-5.4, OpenAI also asks that each assistant message's
  `phase` (`commentary` or `final_answer`) be sent back, or a preamble may
  be taken as a final answer ([reasoning][reasoning]).

Claude Code keeps the thinking blocks it is given, signature and all, and
sends them back on later turns. [integrity.md](integrity.md) saw this
with LM Studio's empty signatures. So a response's reasoning items, its
messages' `phase` values, and the model family can travel in a thinking
block's signature as a Janus envelope, for example `janus-oai:1:` and
base64 of a small JSON object. Janus unpacks it on the way to OpenAI and
drops the whole block on the way to Claude. `RepairForUpstream` today
drops only empty signatures, so it must learn the envelope as well;
otherwise Claude refuses the request. How large the encrypted content
gets, and whether Claude Code keeps a very long signature intact, is
**#399**.

## What `store: false` costs

- **In tokens, nothing.** Input is billed in full whether the history is
  sent or chained with `previous_response_id` ([conversation
  state][state]), and the prompt cache works the same either way.
- **In bytes, the whole conversation each turn.** That is what Claude
  Code already sends to Anthropic.
- **What it does not stop.** OpenAI keeps abuse-monitoring logs of
  Responses traffic for up to 30 days whatever `store` says, unless the
  organization has Zero Data Retention or Modified Abuse Monitoring
  ([your data][data]). OpenAI says this traffic is not used for training.
  The threat model (#385) has to say so.

## What this does not show

- **Anything about real traffic.** No request was sent. Every mapping is
  read from documentation and from the Codex CLI's source, which shows
  what OpenAI accepts from one client, not from Janus.
- **How well an OpenAI model drives Claude Code.** Claude Code's system
  prompt says the model is Claude, and its tools are described for Claude.
  Whether `gpt-6.1-sol` follows them well is untested.
- **Whether Claude Code accepts every answer shape.** In particular, an
  Anthropic stream with zero usage at the start and the full usage at the
  end, very long signatures, and the error wording it reacts to.
- **The open points marked #399**:
  - which tool names OpenAI accepts
  - whether the `"12h"` cache lifetime is accepted
  - whether the SSE `event:` line is present
  - what `/v1/responses/input_tokens` costs
  - whether reasoning items need their `id` with `store: false`
  - how large the reasoning envelope gets
- **Prices and models that change.** Everything here is as of
  2026-10-21; one price is marked promotional. The pricing table in #381
  is to be dated and checked when it is written.

## Sources

Read 2026-10-21:

- [Create a model response][create]
- [Streaming events][events]
- [Input token counts][inputtokens]
- [Prompt caching][caching]
- [Reasoning models][reasoning]
- [Function calling][fc]
- [Structured outputs][so]
- [Tool search][toolsearch]
- [Images and vision][vision]
- [File inputs][files]
- [Conversation state][state]
- [Compaction][compaction]
- [Your data][data]
- [Rate limits][ratelimits]
- [Error codes][errors]
- Model pages: [`gpt-6-astra`][astra], [`gpt-6.1-sol`][sol61],
  [`gpt-6-luna`][luna], [`gpt-5.6-sol`][sol56], [`gpt-5.3-codex`][codex53]
- [Codex: models][codexmodels]
- [openai/codex](https://github.com/openai/codex) at `d138398cef`:
  - `codex-rs/core/src/client.rs`
  - `codex-rs/codex-api/src/common.rs`
  - `codex-rs/codex-api/src/sse/responses.rs`
  - `codex-rs/codex-api/src/sse/responses_error.rs`
  - `codex-rs/codex-api/src/api_bridge.rs`
  - `codex-rs/tools/src/responses_api.rs`
  - `codex-rs/codex-mcp/src/mcp/mod.rs`
  - `codex-rs/codex-mcp/src/tools.rs`

[create]: https://developers.openai.com/api/reference/resources/responses/methods/create.md
[events]: https://developers.openai.com/api/reference/resources/responses/streaming-events.md
[inputtokens]: https://developers.openai.com/api/reference/resources/responses/subresources/input_tokens.md
[caching]: https://developers.openai.com/api/docs/guides/prompt-caching.md
[reasoning]: https://developers.openai.com/api/docs/guides/reasoning.md
[fc]: https://developers.openai.com/api/docs/guides/function-calling.md
[so]: https://developers.openai.com/api/docs/guides/structured-outputs.md
[toolsearch]: https://developers.openai.com/api/docs/guides/tools-tool-search.md
[vision]: https://developers.openai.com/api/docs/guides/images-vision.md
[files]: https://developers.openai.com/api/docs/guides/file-inputs.md
[state]: https://developers.openai.com/api/docs/guides/conversation-state.md
[compaction]: https://developers.openai.com/api/docs/guides/compaction.md
[data]: https://developers.openai.com/api/docs/guides/your-data.md
[ratelimits]: https://developers.openai.com/api/docs/guides/rate-limits.md
[errors]: https://developers.openai.com/api/docs/guides/error-codes.md
[astra]: https://developers.openai.com/api/docs/models/gpt-6-astra.md
[sol61]: https://developers.openai.com/api/docs/models/gpt-6.1-sol.md
[luna]: https://developers.openai.com/api/docs/models/gpt-6-luna.md
[sol56]: https://developers.openai.com/api/docs/models/gpt-5.6-sol.md
[codex53]: https://developers.openai.com/api/docs/models/gpt-5.3-codex.md
[codexmodels]: https://developers.openai.com/codex/models.md
