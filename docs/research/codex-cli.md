# Spike: what the Codex CLI sends, and whether it can be pointed at Janus

Issue: #386 · Date: 2026-10-21 · Status: complete

## Question

M11 runs OpenAI's Codex CLI through Janus, passing its traffic to OpenAI
with its own login. Before that is built: how is the CLI pointed at
another address, for an API key and for a ChatGPT sign-in? What transport
and endpoints does it use? What kinds of request does it make, and how
are they told apart? Which headers identify a session or an account? And
could LM Studio answer its requests when OpenAI cannot?

## Decision

- **Point the Codex CLI at Janus with `openai_base_url`**, given on the
  command line by `janus codex` as `-c openai_base_url="…"`. This setting
  overrides the base URL of the built-in `openai` provider and keeps
  everything else about it, so the CLI sends what it would send OpenAI.
  A custom `model_providers` entry was considered and turned down: it
  stops the CLI treating the provider as OpenAI's, which changes the
  metadata, the compression, and the backend routes it uses.
- **ChatGPT sign-in can go through Janus.** The CLI sends the ChatGPT
  bearer token to whatever base URL is configured, and its workspace
  routing, the one mechanism that could move traffic to another host,
  stays off when the base URL is not ChatGPT's own. Sign-in itself, token
  refresh, and the ChatGPT account calls go to their own hosts directly,
  not through Janus. `janus codex` supports both logins.
- **Two upstreams, told apart by path.** `janus codex` runs
  `codex login status` to learn which login is in use, then sets the base
  URL to one of two paths on Janus:
  - `…/chatgpt/backend-api/codex` for a ChatGPT sign-in, passed to
    `https://chatgpt.com/backend-api/codex`. The path must end in
    `/backend-api/codex`, because the CLI uses that ending to decide that
    the Codex backend's own routes, such as its model list, are there.
  - `…/openai/v1` for an API key, passed to `https://api.openai.com/v1`.
- **HTTP only; WebSocket is refused with 426.** The CLI prefers a
  WebSocket to `/responses` and falls back to HTTP for the rest of the
  session as soon as an upgrade is answered `426 Upgrade Required`, with
  no retries. Janus answers every WebSocket upgrade that way. This changes
  how the CLI talks to OpenAI, so it is the Codex CLI path's one
  sanctioned exception to byte-for-byte pass-through, as narrowing
  `Accept-Encoding` is for Claude Code. Over HTTP, every request carries
  the whole history (`store: false`, no `previous_response_id`), which is
  what the guard and the ledger need. Proxying WebSocket frames is not
  built in v0.10.0.
- **Request bodies are compressed with zstd** when the CLI uses a ChatGPT
  sign-in. The standard library has no zstd decoder, so `janus codex`
  also passes `-c features.enable_request_compression=false`. A
  zstd-compressed body that reaches Janus anyway is relayed untouched when
  the guard is off, and refused with a `janus:` message saying how to turn
  compression off when the guard is on: the guard fails closed.
- **What each issue builds:**
  - **#387** reads Responses request bodies and HTTP SSE streams,
    including the `response.metadata` event and the
    `x-codex-turn-metadata` entry of `client_metadata`, and leaves zstd
    bodies opaque.
  - **#388** passes both upstreams through byte for byte over HTTP,
    answers WebSocket upgrades with 426, and keeps `Authorization`,
    `ChatGPT-Account-ID`, `OpenAI-Organization`, `OpenAI-Project`,
    `x-codex-installation-id`, and the `installation_id` metadata entry out
    of captures and the ledger.
  - **#389** classifies as in the table below and sets the tested version
    to **Codex CLI 0.163.0**.
  - **#390** guards instructions, input messages, and tool outputs, and
    restores secrets in `response.output_text.delta`,
    `response.function_call_arguments.delta`, and
    `response.custom_tool_call_input.delta`.
  - **#392** can use LM Studio's `/v1/responses`, after Janus rewrites
    the request (below), and reads the reset times from the
    `x-codex-*-reset-at` headers.
  - **#393** runs `codex login status`, then `codex` with the two `-c`
    settings and the user's arguments. It needs no API key for a ChatGPT
    sign-in.

## Source

Codex CLI **0.163.0**, the newest stable release on 2026-10-21: tag
`rust-v0.163.0`, commit
[`83086188`](https://github.com/openai/codex/tree/83086188ae0614bd882818ff1aa23f3194962012).
It was read in a scratch clone and never built or run. File references
below are to that commit, under `codex-rs/`. OpenAI's configuration
reference was read at
[learn.chatgpt.com](https://learn.chatgpt.com/docs/config-file/config-reference)
(redirected from developers.openai.com) and LM Studio's at
[lmstudio.ai](https://lmstudio.ai/docs/developer/openai-compat/responses),
both on 2026-10-21. Nothing was sent to OpenAI or ChatGPT.

## Findings

### Where requests go

The built-in `openai` provider
(`model-provider-info/src/lib.rs`, `create_openai_provider` and
`to_api_provider`) chooses its base URL from the login:

| Login | Default base URL |
| --- | --- |
| ChatGPT sign-in (and its token variants) | `https://chatgpt.com/backend-api/codex` |
| API key | `https://api.openai.com/v1` |

`openai_base_url` in `config.toml`, or `-c openai_base_url=…` on the
command line (`utils/cli/src/config_override.rs`), replaces that default
for either login. The reference describes it as the "Base URL override
for the built-in `openai` model provider", and says it is ignored in a
project's own `.codex/config.toml`. The `OPENAI_BASE_URL` environment
variable is not read for this; the source uses it only for the sandbox's
network proxy.

Requests go to the base URL plus a path: `/responses` for every model
turn (`codex-api/src/endpoint/responses.rs`), `/models` for the model
list, and `/memories/trace_summarize` for the memory feature
(`core/src/client.rs`).

**The ChatGPT token is not tied to ChatGPT's hosts.** Nothing checks the
base URL's host before the bearer token and `ChatGPT-Account-ID` are added
(`model-provider/src/bearer_auth_provider.rs`). The list of ChatGPT hosts
in `http-client/src/chatgpt_hosts.rs` governs only which Cloudflare
cookies are kept, and which providers get internal metadata.

**Workspace routing does not route around Janus.** With a ChatGPT sign-in,
the CLI can look up a workspace's backend origin and rewrite the
provider's host to it (`model-provider/src/workspace_routing.rs`,
`apply_workspace_routing`). It does so only when the base URL is
`https://chatgpt.com/backend-api/codex` itself, or has the same origin as
`chatgpt_base_url`
(`app-server/src/request_processors/account_processor/workspace_routing.rs`).
A loopback base URL matches neither, so routing is dropped and requests
stay on Janus. `janus codex` must therefore never set `chatgpt_base_url`.

What does not go through Janus: the sign-in flow and token refresh
(auth.openai.com), and calls made to `chatgpt_base_url`, such as reading
the account and its workspace. They carry no prompts.

### Transport

The built-in provider has `supports_websockets: true`, and `stream`
(`core/src/client.rs`) uses a WebSocket to `/responses` whenever it is
enabled, with the beta header `OpenAI-Beta: responses_websockets=2026-02-06`.
Over the WebSocket the CLI sends only the new items each turn, chained
with `previous_response_id`, and a "prewarm" request
(`generate: false`) before the first turn
(`core/src/session/startup_prewarm.rs`).

When the upgrade is answered `426 Upgrade Required`, the CLI returns
`FallbackToHttp` and calls `force_http_fallback`, which disables
WebSockets for the rest of the session (`core/src/client.rs`, around
lines 1616 and 2047). Other failures go through the normal retry budget
first. Over HTTP each turn is one `POST /responses` with `stream: true`,
answered as Server-Sent Events, and the body carries `store: false`, the
whole history, `include: ["reasoning.encrypted_content"]`, and a
`prompt_cache_key` (`build_responses_request`). There is no prewarm over
HTTP.

Responses are not compressed: reqwest is built without its decompression
features (it has no `async-compression` dependency in `Cargo.lock`), so
the CLI sends no `Accept-Encoding`. Request bodies
are compressed with zstd (`Content-Encoding: zstd`) when the
`enable_request_compression` feature is on, which it is by default, the
login is a ChatGPT sign-in, and the provider is the built-in one
(`responses_request_compression`). An API key never compresses.

### Kinds of request

Each turn's request carries a JSON blob in
`client_metadata["x-codex-turn-metadata"]` whose `request_kind` is
`turn`, `prewarm`, `compaction`, or `memory`
(`core/src/responses_metadata.rs`). The CLI also sets the header
`x-openai-subagent` for work that is not the main thread
(`codex-api/src/requests/headers.rs`, `core/src/guardian/`):

| Class for #389 | Signal |
| --- | --- |
| main | `POST …/responses`, `request_kind: turn`, no `x-openai-subagent` |
| compaction | `request_kind: compaction`, or `x-openai-subagent: compact` |
| review | `x-openai-subagent: review` (the `/review` command) |
| safety | `x-openai-subagent: guardian`: the automatic approval reviewer, the counterpart of Claude Code's auto-mode checks |
| subagent | `x-openai-subagent: collab_spawn`, or any other label (such as `worker`) |
| memory | `x-openai-subagent: memory_consolidation`, `request_kind: memory`, or `POST …/memories/trace_summarize` |
| models | `GET …/models` |
| unknown | anything else; passed through |

There is no separate request for a session title: the CLI takes a
thread's name from its own store.

### Headers

| Header | What it is | Keep out of captures and ledger |
| --- | --- | --- |
| `Authorization` | API key, or the ChatGPT access token | Yes |
| `ChatGPT-Account-ID` | The ChatGPT account | Yes |
| `OpenAI-Organization`, `OpenAI-Project` | From the environment, if set | Yes |
| `x-codex-installation-id` | One per installation | Yes |
| `session-id`, `thread-id`, `x-client-request-id` | The session and thread; the request id is the thread id | No; the session id groups a session's requests, as Claude Code's does |
| `x-codex-window-id` | The context window within a thread | No |
| `x-openai-subagent` | The kind of request, above | No |
| `x-codex-turn-state` | A sticky-routing token the server sends back and the CLI returns | No; passed through unchanged |
| `originator`, `version`, `User-Agent` | The client and its version | No |

The body's `client_metadata` also holds `installation_id`, which is kept
out of the ledger for the same reason.

Usage limits come back as response headers
(`codex-api/src/rate_limits.rs`): `x-codex-primary-used-percent`,
`x-codex-primary-window-minutes`, `x-codex-primary-reset-at` (a Unix
time), the same for `secondary`, and a `codex.rate_limits` event on the
stream. These are what #382 and #392 read for headroom and reset times.

### LM Studio standing in

LM Studio's documentation lists `POST /v1/responses` with streaming,
`previous_response_id`, reasoning effort, and remote MCP tools. The Codex
CLI itself ships an LM Studio provider (`--oss`, port 1234) that speaks
Responses over HTTP, with WebSockets off and internal metadata left out
(`create_oss_provider_with_base_url`).

Two hand-built requests were sent to `qwen/qwen3.8-27b`, which was
already loaded on studio.local; nothing was loaded for this spike.

1. **A first turn in the shape the CLI sends OpenAI**: `instructions`, a
   `function` tool and a `custom` (freeform) tool, `store: false`,
   `include`, `prompt_cache_key`, and `client_metadata`. It was accepted.
   The stream was in the order the CLI parses, and ended in a
   `function_call` with correct arguments. Reasoning came as
   `response.reasoning_text.delta` events, which the CLI's parser handles
   (`codex-api/src/sse/responses.rs`), and with no `encrypted_content`.
   **The `custom` tool was dropped without an error**: the response
   listed only the function tool.
2. **A later turn whose history held what OpenAI returns**: a `reasoning`
   item with `encrypted_content`, a `function_call`, and its
   `function_call_output`. It was accepted, the encrypted item was
   ignored, and the answer used the tool output correctly.

So for #392, Janus rewrites a request before LM Studio sees it: the model
name to the local model, `custom` tools to function tools taking one
string (and the calls back again), and `namespace`, `tool_search`, and
`web_search` tools, which LM Studio is not documented to support, left
out and named in the ledger. Whether the CLI uses a `custom` tool depends
on the model it was given (`apply_patch_tool_type` in the model list), so
a stand-in for a Codex model will usually meet one, for `apply_patch`.

## What this does not show

- **Any real Codex CLI traffic.** There is no OpenAI account, so the CLI
  was read, not run. Every header, event, and path above is from the
  source at one commit; the server may send things the source ignores,
  and a new release may change any of it.
- **That ChatGPT's servers accept requests relayed by a proxy.** Nothing in
  the client prevents it, but Cloudflare in front of chatgpt.com may treat
  a relayed request differently, and the CLI keeps Cloudflare cookies only
  for ChatGPT's hosts, so it will not keep them for a loopback base URL.
- **What the 426 fallback costs.** One refused upgrade per session was
  read in the source, not timed, and the CLI may log a warning about it.
- **Whether OpenAI's terms allow a ChatGPT plan's traffic to pass through
  a local proxy.** The CLI makes it possible; that is not the same thing.
- **The quality of LM Studio's answers to real Codex turns.** Two tiny
  requests were sent, one turn each, with one function tool. The CLI's
  full system prompt and tool set were not tried.
- **The model list from the Codex backend**, which decides
  `apply_patch_tool_type` and other per-model behaviour, was not seen.
