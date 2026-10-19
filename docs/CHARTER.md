# Janus — Project Charter

Status: draft for review · Owner: Ris Adams · Last updated: 2026-10-11

## 1. One-line definition

Janus is a local proxy, written in Go, that sits between Claude Code and the
Anthropic API and uses a local model running in LM Studio to do as much of
the work as it safely can, so that Claude is only paid for the thinking that
needs Claude.

The name is the design: one face toward the local model, one toward the
upstream model, and a single doorway that every request passes through.

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

```
                    ┌──────────────────────── Janus ────────────────────────┐
                    │                                                       │
 Claude Code ──────▶│ classify ─▶ guard ─▶ route ─┬─▶ compress ─▶ upstream ─┼──▶ Anthropic API
 (Messages API)     │                             │                         │
        ▲           │                             └─▶ local ─▶ validate ────┼──▶ LM Studio
        │           │                                    │ fail             │
        │           │                                    └──▶ escalate ─────┼──▶ Anthropic API
        │           │                                                       │
        │           │ ledger: every decision, token, and dollar recorded    │
        │           └───────────────────────────────────────────────────────┘
        │
        └── janus mcp: worker tools Claude calls to hand subtasks to the local model
```

Four mechanisms, combined:

| Mechanism | What it does | Where the saving comes from |
| --- | --- | --- |
| **Route** | Whole request classes (utility calls, pinned subagents) go to the local model and never reach Claude. | Requests avoided entirely. |
| **Escalate** | Eligible requests are tried locally first; Janus validates the answer and falls back to Claude before the client sees anything. | Requests avoided when local is good enough. |
| **Delegate** | Claude, as orchestrator, calls Janus worker tools (digest these files, search and summarize, draft this) that run on the local model. | Bulk content never enters Claude's context. |
| **Compress** | Before an upstream call, the local model shrinks what Claude doesn't need verbatim (large tool results, stale context). | Fewer input tokens per upstream request. |

## 4. Goals and success measures

G1's target was restated from the baseline
([docs/research/baseline.md](research/baseline.md)): the 30% first proposed
is out of reach without breaking the prompt cache, since the cost is the
conversation reading its own cache. The other targets stand as proposed.

| # | Goal | Measured by | Proposed target |
| --- | --- | --- | --- |
| G1 | Cut upstream spend | Median cache-weighted upstream cost per working session (a Claude Code session of 20 or more main-loop turns), against the baseline of $23.20 | Lower by what the baseline shows is in reach, about 9%, with no drop in task success; no mechanism ever costs more than it saves |
| G2 | Keep private data local | Count of protected items found in upstream request bodies, over a test corpus and in real-use audit | Zero. Fails closed. |
| G3 | Keep working when upstream doesn't | A session continues through a simulated outage / 429 storm / no network | Degraded but usable, clearly labelled |
| G4 | Learn | Every request has a decision record explaining what Janus did and why; local and upstream answers can be compared offline | 100% of requests explained |
| G5 | Never be worse than no Janus | With every feature off: byte-equivalent pass-through. Added latency on pass-through. | Equivalent; p50 overhead < 10 ms |

## 5. Design principles

1. **Precedence when goals conflict: privacy → correctness → availability →
   savings.** A protected request is never sent upstream to keep a session
   alive. A saving is never taken at the cost of a broken turn.
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
5. **Transparent to the client.** Claude Code is not patched or wrapped. It
   is pointed at Janus with environment variables and sees a normal
   Anthropic API.
6. **Boring to run.** One static binary, one config file, loopback only,
   standard library first.

## 6. Scope

**In scope**

- Inbound: Anthropic Messages API, as Claude Code speaks it (streaming, tool
  use, thinking, prompt caching, token counting).
- Backends: Anthropic API upstream; LM Studio locally.
- Single user, single machine, macOS first; Linux and Windows builds.
- Routing, local-first escalation, delegation tools, context compression,
  redaction and local-only policy, fallback, capture logs, cost ledger,
  shadow comparison and replay.

**Out of scope (for 1.0)**

- Multi-user gateway, hosted service, API keys of its own, billing.
- Inbound OpenAI-format clients. (A backend interface keeps the door open;
  nothing is built for it.)
- Other local runtimes (Ollama, llama.cpp server, vLLM) and other upstream
  providers (Bedrock, Vertex).
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

## 9. Open decisions

These change details, not direction. Defaults are assumed until decided.

| # | Decision | Default assumed |
| --- | --- | --- |
| D1 | Claude Code auth: API key or subscription (OAuth)? Decides whether "spend" is dollars or rate-limit headroom. | Support both by passing auth through untouched; ledger reports tokens and estimated dollars |
| D2 | Hardware and candidate local models | Decided by the M0 benchmark spike: Apple M5 Max, 36 GB; `qwen/qwen3.8-27b` (MLX, 4-bit) for subagent conversations only ([models](research/models.md)) |
| D3 | Keep blush's zero-dependency rule? | Stdlib first; each exception recorded as an ADR |
| D4 | License | MIT, matching blush |
| D5 | Module path casing (`risadams/Janus` vs `janus`) | Lowercase `github.com/risadams/janus` |

## 10. Related documents

- [Requirements](REQUIREMENTS.md)
- [Roadmap](ROADMAP.md)
- Work breakdown: [issues by milestone](https://github.com/risadams/Janus/milestones) · [project board](https://github.com/users/risadams/projects/3)
