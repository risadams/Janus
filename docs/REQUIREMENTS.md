# Janus — Requirements

Status: draft for review · Last updated: 2026-10-21

Requirement IDs are stable and referenced from issues. Priority uses MoSCoW
for the 1.0 release: **M**ust, **S**hould, **C**ould. The milestone column
points at the [roadmap](ROADMAP.md).

## Functional requirements

### FR-1 Transparent proxy

| ID | Requirement | Pri | MS |
| --- | --- | --- | --- |
| FR-1.1 | Accept the Anthropic Messages API on a local port: `POST /v1/messages` (streaming and non-streaming) and `POST /v1/messages/count_tokens`. Any other path is forwarded upstream untouched. | M | M1 |
| FR-1.2 | With all features disabled, the upstream request and the client response are byte-equivalent to a direct connection, apart from hop-by-hop headers and one named exception: on Messages requests Janus narrows `Accept-Encoding` to gzip (or to none, if the client did not offer gzip), so that every response can be read for the ledger. The client only ever receives an encoding it asked for. | M | M1 |
| FR-1.3 | Client credentials (API key or OAuth bearer) pass through unchanged. Janus never stores, logs, or substitutes them. | M | M1 |
| FR-1.4 | Streaming responses are relayed as they arrive. Buffering happens only where a feature explicitly needs it (FR-5.1). | M | M1 |
| FR-1.5 | Unknown request fields, beta headers, and content block types are preserved, not dropped. | M | M1 |

### FR-2 Observability and learning

| ID | Requirement | Pri | MS |
| --- | --- | --- | --- |
| FR-2.1 | Per-request capture (readable Markdown plus raw request and response), switchable, with secrets redacted in the readable copy. | M | M1 |
| FR-2.2 | A decision record for every request: request class, route taken, the rule or reason, stages applied, backend, latency, token usage split by uncached input / cache write / cache read / output. | M | M1 |
| FR-2.3 | An append-only ledger of decision records, and a `janus stats` command reporting cache-weighted cost, cost by request class, and estimated savings against the baseline. | M | M1 |
| FR-2.4 | Shadow mode: serve the upstream answer, also run the request locally, record both for comparison. Never affects what the client receives. | S | M2 |
| FR-2.5 | Replay: re-run captured requests against a chosen backend or model offline and report differences. | S | M2 |
| FR-2.6 | `janus audit`: show exactly what left the machine for a given request or session. | S | M6 |

### FR-3 Local backend (LM Studio)

| ID | Requirement | Pri | MS |
| --- | --- | --- | --- |
| FR-3.1 | Health check, model discovery, and loaded-model detection against LM Studio. | M | M2 |
| FR-3.2 | Send Messages-shaped requests to LM Studio: natively where LM Studio offers an Anthropic-compatible endpoint, otherwise by translating to and from OpenAI chat completions, including tool calls and streaming. | M | M2 |
| FR-3.3 | Configurable mapping from requested model names to local models, with per-model context limit and timeout. | M | M2 |
| FR-3.4 | Requests that exceed the local model's context window are never sent to it. | M | M2 |
| FR-3.5 | The LM Studio base URL is a config setting, overridable by flag and environment, and is never hard-coded. The default is `http://studio.local:1234`, not `localhost`, so that Janus always talks to the intended LM Studio instance whichever machine it runs on. | M | M1 |

### FR-4 Classification and routing

| ID | Requirement | Pri | MS |
| --- | --- | --- | --- |
| FR-4.1 | Classify each request from deterministic signals (requested model, tools present, size, system prompt fingerprint, endpoint) into a named class. Unrecognised requests are class `unknown` and pass through. | M | M2 |
| FR-4.2 | A rule-based policy maps class and attributes to a route: `upstream`, `local`, or `local-then-escalate`. Rules live in config. | M | M2 |
| FR-4.3 | Model-alias routing: a request for a configured alias (for example `janus-local`) always goes local, so Claude Code's small/fast model and subagents can be pinned to the local model from its own settings. | M | M2 |
| FR-4.4 | Conversation integrity: Janus does not change backend inside a conversation where doing so would produce a history the upstream API rejects. | M | M3 |

### FR-5 Triage and escalation

| ID | Requirement | Pri | MS |
| --- | --- | --- | --- |
| FR-5.1 | For `local-then-escalate` routes, hold the local response until it passes validation; on failure, send the original request upstream. The client sees one clean response. | M | M3 |
| FR-5.2 | Validators: well-formed response, tool calls match the declared schema, not empty, not truncated, no refusal or confusion patterns. Each is individually switchable. | M | M3 |
| FR-5.3 | Optional local-model triage for requests the rules can't place, under a configurable latency budget; over budget means upstream. | S | M3 |
| FR-5.4 | An explicit override to force a request or session upstream or local. | S | M3 |
| FR-5.5 | Escalation rate per class is recorded; a class that escalates above a threshold is routed upstream directly. | C | M3 |

### FR-6 Delegation

| ID | Requirement | Pri | MS |
| --- | --- | --- | --- |
| FR-6.1 | `janus mcp`: an MCP server exposing worker tools backed by the local model. | M | M4 |
| FR-6.2 | Worker tools for: answering a question about one or more files, searching a tree and summarizing the findings, and drafting or transforming text and code to a spec. | M | M4 |
| FR-6.3 | Worker results are compact and bounded in size; the bulk input stays local. | M | M4 |
| FR-6.4 | Delegated work appears in the ledger with an estimate of upstream tokens avoided. | S | M4 |
| FR-6.5 | Ships guidance (CLAUDE.md snippet, subagent definitions) that makes Claude reach for the workers. | S | M4 |

### FR-7 Context economy

| ID | Requirement | Pri | MS |
| --- | --- | --- | --- |
| FR-7.1 | A transform pipeline applied to upstream-bound requests. Every transform is deterministic and memoized: the same input block always yields the same output bytes for the life of a conversation. | M | M5 |
| FR-7.2 | A net-savings guard estimates each transform's cache-weighted effect and skips it when the effect is not positive. | M | M5 |
| FR-7.3 | Tool-result digestion: oversized tool results are replaced by a local-model digest; the original stays retrievable. | S | M5 |
| FR-7.4 | Stale-context pruning: superseded content (for example repeated reads of the same file) is collapsed without a model. | C | M5 |
| FR-7.5 | Conversation compaction requests can be served by the local model. | C | M5 |

### FR-8 Privacy

| ID | Requirement | Pri | MS |
| --- | --- | --- | --- |
| FR-8.1 | Detect secrets (key formats, high-entropy strings, configured patterns) in upstream-bound requests. | M | M6 |
| FR-8.2 | Replace detected secrets with stable placeholders and restore them in the response, including inside streamed tool-call arguments. An unresolvable placeholder blocks the turn. | M | M6 |
| FR-8.3 | Local-only policy: content matching configured path globs or markers is never sent upstream. The request is served locally or refused. Fails closed. | M | M6 |
| FR-8.4 | Capture and ledger files are created owner-readable only, with configurable retention. | M | M1 |
| FR-8.5 | Listen on loopback by default; binding elsewhere needs an explicit setting. | M | M1 |

### FR-9 Resilience

| ID | Requirement | Pri | MS |
| --- | --- | --- | --- |
| FR-9.1 | On upstream failure (network error, 429, 5xx, overloaded), fall back to the local model where policy allows, and mark the response as degraded. | M | M3 |
| FR-9.2 | On local failure or timeout, fall back to upstream pass-through, except for local-only content. | M | M3 |
| FR-9.3 | Circuit breakers and health checks per backend so a dead backend costs one timeout, not one per request. | S | M3 |

### FR-10 Configuration and operation

| ID | Requirement | Pri | MS |
| --- | --- | --- | --- |
| FR-10.1 | One config file, overridable by flags and environment; validated at startup with actionable errors. | M | M1 |
| FR-10.2 | Single static binary for macOS, Linux, and Windows. | M | M7 |
| FR-10.3 | `janus doctor`: checks upstream reachability, LM Studio, loaded models, and prints the exact environment needed to point Claude Code at Janus. | S | M1 |
| FR-10.4 | Every feature has an off switch, and off is the default until the feature has shown a net benefit. | M | all |

### FR-11 Providers

| ID | Requirement | Pri | MS |
| --- | --- | --- | --- |
| FR-11.1 | OpenAI backend: Claude Code's Messages requests are translated to OpenAI's Responses API, and the answers back, streaming and not, so Claude Code sees the Anthropic API whichever provider answered. | M | M10 |
| FR-11.2 | Translation is deterministic: the same history translates to the same bytes on every turn, so OpenAI's prompt cache holds. Requests are sent with `store: false` and no `previous_response_id`. | M | M10 |
| FR-11.3 | Each rule names an ordered list of providers (Claude, OpenAI, local). A request moves down the list when the provider in use is down, has reached a usage limit, or was switched away from by hand. A list of one is Claude only, Codex only, or local only. | M | M10 |
| FR-11.4 | A conversation that moves keeps its visible history: messages, tool calls, and tool results. Each provider's hidden reasoning is left out on the way to another, the same way on every turn. A history too long for the next provider's context is shortened as the local stand-in's is. | M | M10 |
| FR-11.5 | A conversation moved by a usage limit returns when that limit resets, as the provider reported in its responses. One moved by an outage returns through a circuit breaker. | M | M10 |
| FR-11.6 | `janus use` switches a running session's provider from its next turn. | S | M10 |
| FR-11.7 | The privacy policy can allow content to some providers and not others. A provider a request may not reach is skipped; if none is left, the request is refused. Fails closed. | M | M10 |
| FR-11.8 | The OpenAI key is read from an environment variable or the OS credential store, never from a literal in the config, and never written anywhere ([ADR-005](adr/005-openai-credential.md)). | M | M10 |
| FR-11.9 | The ledger records which provider answered and why, OpenAI's usage and limits, and what each move between providers cost. | M | M10 |
| FR-11.10 | `janus claude` and `janus local` start Claude Code with a chosen provider list for that session, on one running Janus. | S | M10 |
| FR-11.11 | Optional local-model triage chooses a new conversation's provider from its rule's list, once, on its first turn, under a latency budget; a timeout or unclear answer means the list's first provider. Never per turn. | M | M12 |

### FR-12 The Codex CLI as a client

| ID | Requirement | Pri | MS |
| --- | --- | --- | --- |
| FR-12.1 | The Codex CLI's requests reach OpenAI byte for byte, with its own credential, whether a ChatGPT login or an API key. Account identifiers in its headers are never logged, captured, or recorded. | M | M11 |
| FR-12.2 | The guard covers the Codex CLI's requests and fails closed, as it does for Claude Code's. | M | M11 |
| FR-12.3 | The ledger, `janus stats`, and the console record and show the Codex CLI's requests, by class and by client. | M | M11 |
| FR-12.4 | LM Studio stands in for OpenAI when it is down or a limit is reached, and the session returns when the limit resets. | S | M11 |
| FR-12.5 | `janus codex` runs the Codex CLI through Janus when Janus is up, and decides what to do when it is not, as `janus claude` does. | M | M11 |

## Non-functional requirements

| ID | Requirement |
| --- | --- |
| NFR-1 | **No worse than direct.** Pass-through overhead p50 under 10 ms and no added time-to-first-byte beyond that. |
| NFR-2 | **Precedence.** Where requirements conflict: privacy, then correctness, then availability, then savings. |
| NFR-3 | **Dependencies.** Standard library first. Each third-party dependency is justified in an ADR. |
| NFR-4 | **Testability.** Every backend, OpenAI included, is faked with `httptest`; the suite runs offline. Golden fixtures cover streaming, tool use, and thinking. |
| NFR-5 | **Compatibility.** Tested against current Claude Code, Codex CLI, and LM Studio releases; fixture suite refreshed when any of them changes its wire behaviour. |
| NFR-6 | **Explainability.** No request is altered or rerouted without a decision record that says why. |
| NFR-7 | **Safe defaults.** A fresh install is a pass-through proxy with logging; nothing reroutes or rewrites until configured. |

## Constraints and assumptions

- Claude Code and the Codex CLI are the only clients designed for. Claude
  Code is configured through `ANTHROPIC_BASE_URL` and its model-selection
  environment variables; the Codex CLI through its own base URL setting.
  The Codex CLI's requests go to OpenAI or the local model, never to
  Claude, and a conversation does not move between the two clients.
- The OpenAI backend is built from OpenAI's published API reference and
  the open-source Codex CLI. Until the owner has an OpenAI account, no
  real OpenAI traffic has been seen.
- LM Studio is running at the configured base URL (FR-3.5) with at least one
  model loaded and its server enabled. Reaching it by hostname rather than
  loopback requires LM Studio to serve on the local network.
- Local model capability is the limiting factor for G1 and is established
  empirically (roadmap M0), not assumed.
