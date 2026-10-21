# Janus — Project Charter

Status: draft for review · Owner: Ris Adams · Last updated: 2026-10-21

## 1. One-line definition

Janus is a local proxy, written in Go, that sits between Claude Code and the
Anthropic API and uses a local model running in LM Studio to do as much of
the work as it safely can, so that Claude is only paid for the thinking that
needs Claude.

The name is the design: one face toward the local model, one toward the
upstream model, and a single doorway that every request passes through.

From v0.10.0 there is more than one model upstream and more than one
client at the door. Claude Code's requests can be answered by Claude, by
an OpenAI Codex model, or by the local model, and a conversation can move
between them without losing what it has said. The Codex CLI can come
through the same door on its way to OpenAI. The definition stands: the
expensive model is kept for the work that needs it.

## 2. Problem

Claude Code sends everything upstream: the hard reasoning, but also the
session titles, the summaries, the 4,000-line test logs, the boilerplate
subagent work, and whatever secrets happen to sit in the files it reads.
A capable local model is idle on the same machine. Nothing connects the two.

[blush](https://github.com/risadams/blush) proved the proxy position works
(Claude Code runs unmodified through `ANTHROPIC_BASE_URL`), but blush only
watches. Janus acts.

## 3. Operating model

Claude is the brain. The local model is the workhorse. Janus decides which
one a piece of work needs, local-first.

![Claude is the brain, the local model the workhorse: Claude Code sends every request to Janus, which classifies, guards, and routes it to Anthropic, compressed, or to the local model, escalating to Claude when a local answer fails; Claude can delegate reading to the local model through janus mcp; every decision goes in the ledger](diagrams/operating-model.svg#gh-light-mode-only)
![Claude is the brain, the local model the workhorse: Claude Code sends every request to Janus, which classifies, guards, and routes it to Anthropic, compressed, or to the local model, escalating to Claude when a local answer fails; Claude can delegate reading to the local model through janus mcp; every decision goes in the ledger](diagrams/operating-model-dark.svg#gh-dark-mode-only)

Four mechanisms, combined:

| Mechanism | What it does | Where the saving comes from |
| --- | --- | --- |
| **Route** | Whole request classes (utility calls, pinned subagents) go to the local model and never reach Claude. | Requests avoided entirely. |
| **Escalate** | Eligible requests are tried locally first; Janus validates the answer and falls back to Claude before the client sees anything. | Requests avoided when local is good enough. |
| **Delegate** | Claude, as orchestrator, calls Janus worker tools (digest these files, search and summarize, draft this) that run on the local model. | Bulk content never enters Claude's context. |
| **Compress** | Before an upstream call, the local model shrinks what Claude doesn't need verbatim (large tool results, stale context). | Fewer input tokens per upstream request. |

### Providers

Claude is the brain by default, not the only one. Each rule names the
providers it may use, in order: Claude, a Codex model through OpenAI's
API, or the local model. A list of one is Claude only, Codex only, or
local only.

- **A conversation moves** down its list when the provider in use is
  down, has reached a usage limit, or is switched by hand, and when a
  rule asks the local model to choose a provider for a new conversation.
  It never moves for a single turn on a whim: each move is a full cache
  write on the side it moves to.
- **What moves with it** is everything the user and the tools can see:
  every message, tool call, and tool result. Each provider's hidden
  reasoning stays with that provider, because the other cannot check or
  read it.
- **It moves back** when the limit that moved it resets, which each
  provider says in its responses, or when an outage ends.
- **Claude Code never knows.** Janus translates to and from OpenAI's API,
  and Claude Code sees the Anthropic API whichever provider answered.

The Codex CLI is a second client, not a second way into Claude. Its
requests go to OpenAI as it sent them, with its own login. Janus guards
them, records them, and lets the local model stand in when OpenAI cannot
answer. They are not sent to Claude, and a conversation does not move
between Claude Code and the Codex CLI.

## 4. Goals and success measures

G1's target was restated from the baseline
([docs/research/baseline.md](research/baseline.md)): the 30% first proposed
is out of reach without breaking the prompt cache, since the cost is the
conversation reading its own cache. The other targets stand as proposed.

| # | Goal | Measured by | Proposed target |
| --- | --- | --- | --- |
| G1 | Cut upstream spend | Median cache-weighted upstream cost per working session (a Claude Code session of 20 or more main-loop turns), against the baseline of $23.20 | Lower by what the baseline shows is in reach, about 9%, with no drop in task success; no mechanism ever costs more than it saves |
| G2 | Keep private data local | Count of protected items found in upstream request bodies, over a test corpus and in real-use audit | Zero. Fails closed. |
| G3 | Keep working when upstream doesn't | A session continues through a simulated outage / 429 storm / no network, on the next provider in its list | Degraded but usable, clearly labelled |
| G4 | Learn | Every request has a decision record explaining what Janus did and why; local and upstream answers can be compared offline | 100% of requests explained |
| G5 | Never be worse than no Janus | With every feature off: byte-equivalent pass-through. Added latency on pass-through. | Equivalent; p50 overhead < 10 ms |

## 5. Design principles

1. **Precedence when goals conflict: privacy → correctness → availability →
   savings.** A protected request is never sent upstream to keep a session
   alive. A saving is never taken at the cost of a broken turn. What may
   go to one provider may be kept from another; a provider a request may
   not reach is skipped, and a request no allowed provider can take is
   refused.
2. **Measure before optimizing.** Every feature ships behind a switch and
   reports its own effect in the ledger. A feature that can't show a net
   saving stays off.
3. **Cache-aware or not at all.** Claude Code leans on prompt caching, where
   cached input is billed at a tenth of the base rate. A transform that
   rewrites an already-cached prefix can raise the bill. Savings are always
   counted in cache-weighted cost, never raw tokens.
4. **Fail open to pass-through.** If Janus, the local model, or any stage
   misbehaves, the request goes upstream untouched (unless principle 1
   forbids it).
5. **Transparent to the client.** Claude Code and the Codex CLI are not
   patched or wrapped. Each is pointed at Janus with environment variables
   and sees the API it was built for. Claude Code sees the Anthropic API
   even when another provider answered.
6. **Boring to run.** One static binary, one config file, loopback only,
   standard library first.
7. **One credential of its own, and no more.** Janus passes a client's
   credential to that client's provider untouched and keeps it no longer
   than the request. The one credential Janus holds is the OpenAI key that
   lets Claude Code's requests be answered by OpenAI. It is read from the
   environment or the OS keychain at startup, and never written anywhere
   ([ADR-005](adr/005-openai-credential.md)).

## 6. Scope

**In scope**

- Inbound: Anthropic Messages API, as Claude Code speaks it (streaming, tool
  use, thinking, prompt caching, token counting); OpenAI's Responses API,
  as the Codex CLI speaks it.
- Backends: Anthropic API upstream; OpenAI's Responses API upstream, for
  Claude Code's requests translated and for the Codex CLI's as sent; LM
  Studio locally.
- An ordered list of providers per rule, moving a Claude Code conversation
  between them with its visible history, and returning when a limit
  resets. Choosing a new conversation's provider by rule or by local
  triage.
- Single user, single machine, macOS first; Linux and Windows builds.
- Routing, local-first escalation, delegation tools, context compression,
  redaction and local-only policy, fallback, capture logs, cost ledger,
  shadow comparison and replay.

**Out of scope (for 1.0)**

- Multi-user gateway, hosted service, API keys of its own for clients to
  use, billing.
- Inbound OpenAI-format clients other than the Codex CLI.
- Other local runtimes (Ollama, llama.cpp server, vLLM) and other upstream
  providers (Bedrock, Vertex, Gemini).
- Sending the Codex CLI's requests to Claude, or moving a conversation
  between Claude Code and the Codex CLI.
- Answering Claude Code's requests with a ChatGPT plan: the OpenAI backend
  uses an API key. The Codex CLI's own ChatGPT login passes through as it
  is.
- Holding a client's credential beyond its request
  ([keepwarm.md](research/keepwarm.md)).
- Loading, downloading, or tuning local models. Janus uses what LM Studio has
  loaded.
- A GUI. The interface is the CLI, the config file, and the logs.

## 7. Relationship to blush

Janus is a new codebase that ports from blush rather than forking it.

| From blush | Disposition |
| --- | --- |
| Streaming pass-through, hop-by-hop header handling, auth pass-through | Port into `internal/proxy` |
| Markdown capture logs with redacted headers | Port; add 0600 permissions and retention |
| Config file + flag + env layering, first-run starter config | Port the pattern; new multi-backend schema |
| Startup banner that prints the right env vars | Port; grows into `janus doctor` |
| CI (gofmt, vet, build, test matrix), stdlib-only, `httptest` style | Port as-is |
| Single upstream, flat `package main`, body treated as opaque bytes | Replace: multiple backends, staged pipeline, typed Messages/SSE model |

## 8. Risks

| # | Risk | Why it matters | Mitigation |
| --- | --- | --- | --- |
| R1 | Transforms break prompt caching | Could make Janus cost more than it saves | Deterministic, memoized transforms; net-savings guard; cache-weighted ledger (M5) |
| R2 | Mixed-origin conversations are rejected upstream | Thinking blocks carry signatures; a locally produced turn inside a Claude conversation may not replay | Spike in M0; route whole request classes and whole subagent conversations rather than individual turns; integrity guard (M3) |
| R3 | Local models can't drive Claude Code's tool loop | Dozens of tool schemas and a very large system prompt strain local context, speed, and tool-call accuracy | Spike in M0 on real hardware; start with tool-free request classes; validators + escalation |
| R4 | Triage costs more time than it saves money | A local classification pass on every request adds latency | Deterministic rules first; model triage only for ambiguous cases, under a latency budget |
| R5 | Savings are smaller than hoped | Most of Claude Code's input is already cached at a tenth of the price | Baseline first (M1); pick targets by measured share of cost, not intuition |
| R6 | Claude Code's traffic changes shape | Classification by fingerprint is brittle across releases | Classify on stable signals where possible; unknown classes pass through; fixture suite refreshed per release |
| R7 | Rehydrating redacted values corrupts a tool call | A placeholder split across stream chunks, or altered by the model, yields a wrong edit | Streaming-aware rehydration; unresolved placeholders block the turn rather than pass silently |
| R8 | Translation breaks a turn | Claude Code expects the Anthropic API's shapes exactly; a translated OpenAI answer that differs can stop a session | Map the APIs field by field before building (#368); fuzzed translators; a failed translation is an Anthropic-shaped error that says what to do |
| R9 | Moving between providers costs more than it saves | Each move is a full cache write on the side it moves to, and the main conversation's cost is nearly all cache | Move only at a limit, an outage, by hand, or on a conversation's first turn; return at the reset; the ledger prices every move |
| R10 | Two clients' traffic changes shape | R6, twice: Claude Code and the Codex CLI each change between releases | A tested version for each; doctor warns when either is newer; unknown requests pass through |
| R11 | Janus holds a credential | An OpenAI key in a log, capture, config, or service unit is a leak | ADR-005: from the environment or the keychain only; tests prove it reaches nothing but OpenAI |
| R12 | Built without real OpenAI traffic | There is no OpenAI account yet, so the backend and the Codex CLI path are built from published docs and the open-source CLI | Offline fakes made from the docs; release candidates only until the owner's validation sessions (#399) pass |

## 9. Open decisions

These change details, not direction. Defaults are assumed until decided.

| # | Decision | Default assumed |
| --- | --- | --- |
| D1 | Claude Code auth: API key or subscription (OAuth)? Decides whether "spend" is dollars or rate-limit headroom. | Support both by passing auth through untouched; ledger reports tokens and estimated dollars |
| D2 | Hardware and candidate local models | Decided by the M0 benchmark spike: Apple M5 Max, 36 GB; `qwen/qwen3.8-27b` (MLX, 4-bit) for subagent conversations only ([models](research/models.md)) |
| D3 | Keep blush's zero-dependency rule? | Stdlib first; each exception recorded as an ADR |
| D4 | License | MIT, matching blush |
| D5 | Module path casing (`risadams/Janus` vs `janus`) | Lowercase `github.com/risadams/janus` |
| D6 | What the config calls Anthropic, now there is more than one upstream | The config keeps `upstream`, so old configs need no migration; commands, the console, and the docs say Claude |
| D7 | Real OpenAI traffic while v0.10.0 is built | None until the owner has an account; v0.10.0 stays a release candidate until then |

## 10. Related documents

- [Requirements](REQUIREMENTS.md)
- [Roadmap](ROADMAP.md)
- Work breakdown: [issues by milestone](https://github.com/risadams/Janus/milestones) · [project board](https://github.com/users/risadams/projects/3)
