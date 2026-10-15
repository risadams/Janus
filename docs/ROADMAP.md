# Janus — Roadmap

Status: draft for review · Last updated: 2026-10-15

The roadmap is a sequence, not a schedule. Each milestone ends in something
usable and has an exit test. Issues for each milestone are on the
[milestones page](https://github.com/risadams/Janus/milestones).

## Shape

```
M0 Foundations ─▶ M1 Observe ─▶ M2 Route ─▶ M3 Escalate ─▶ M4 Delegate ─▶ M5 Compress ─▶ M7 1.0
                                    │                                                      ▲
                                    └──────────────▶ M6 Guard ─────────────────────────────┘
                                                    │
                                                    └──────▶ M8 Pace (beside M7)
```

M6 depends only on M2, so it can be pulled forward if privacy becomes the
pressing goal. M8 depends on M3's fallback and runs beside M7: it does not
hold up 1.0, and its first piece, recording the limits, starts at once so
that the 1.0 report has the data. Everything else is ordered by two rules: measure before
changing anything, and take the cheapest, safest savings first.

## Milestones

### M0 — Foundations

Answer the questions the design rests on, and stand up the repo.

- Go module, package layout, CI, license, dependency ADR.
- Spike: catalogue what Claude Code actually sends (request classes, sizes,
  cache behaviour), using blush.
- Spike: what LM Studio's API really supports (Anthropic-compatible
  endpoint, tool calls, streaming).
- Spike: can a conversation mix local and upstream turns? (thinking
  signatures, tool-use ids)
- Spike: which local models, on this hardware, can do which jobs.

**Exit:** four short research notes in `docs/research/`, each ending in a
decision. Charter risks R2 and R3 are either retired or have a plan.

### M1 — Observe

Janus replaces blush: a transparent proxy that also keeps the books.

- Proxy core, config, typed Messages and SSE model that relays bytes
  untouched while reading usage.
- Capture logs, decision records, ledger, pricing model, `janus stats`,
  `janus doctor`.
- Pass-through fidelity suite.

**Exit:** a full day of real Claude Code work through Janus with no
behavioural difference, and a baseline report showing where the cost goes by
request class. The G1 target is restated from that report.

### M2 — Route

The local model starts answering. Lowest-risk requests first.

- Backend interface; LM Studio backend with health, discovery, translation
  as needed.
- Classifier and rule-based routing policy.
- Model-alias routing, with a recipe for pinning Claude Code's small/fast
  model and chosen subagents to local.
- Shadow mode and replay, so a class is proven before it is switched.

**Exit:** at least one request class served locally in daily use, chosen
from shadow data; `janus stats` shows the saving.

### M3 — Escalate

Local-first with a safety net, and survival when upstream is down.

- Buffered local attempt, validators, escalation.
- Optional model triage under a latency budget; explicit override.
- Upstream-failure fallback, local-failure fallback, circuit breakers.
- Conversation integrity guard.

**Exit:** a `local-then-escalate` class in daily use with its escalation
rate tracked; a session survives a simulated upstream outage.

### M4 — Delegate

Claude becomes the orchestrator of local work.

- `janus mcp` server and three worker tools (file Q&A, search-and-summarize,
  draft/transform).
- Guidance that gets Claude to use them; avoided-token accounting.

**Exit:** on a set of representative tasks, Claude delegates bulk reading to
the workers and the ledger shows the upstream input avoided.

### M5 — Compress

Shrink what still goes upstream, without breaking the cache.

- Transform pipeline with the determinism contract and memo store.
- Net-savings guard.
- Tool-result digestion; stale-context pruning; local compaction.

**Exit:** each transform shows a positive cache-weighted saving on replayed
real sessions, or ships disabled with the numbers that explain why.

### M6 — Guard

Control what leaves the machine.

- Secret detectors; reversible redaction with streaming rehydration.
- Local-only policy that fails closed.
- Leak-test corpus in CI; `janus audit`.

**Exit:** zero protected items upstream across the corpus; an end-to-end
session edits a file containing a redacted secret correctly.

### M7 — 1.0

- Release pipeline, user docs, benchmarks and soak test, threat model.
- Results report against the charter's goals.

**Exit:** tagged 1.0 with binaries, and a report that states, with data,
how G1–G5 came out.

### M8 — Pace

On a subscription the limit, not the price, is what runs out. In the
first three days of real use the five-hour limit was reached twice, and
the local model answered main turns for an hour and a half. Every
response from Claude says how much of each limit is used; Janus can read
that and act before the limit is reached rather than after.

- Record the five-hour and weekly utilization, and when each resets, on
  every response.
- Show the headroom in the console and in `janus stats`, and which kinds
  of request use it up.
- A rule condition that sends a class local once utilization passes a
  threshold, so the cheap work moves first and the main conversation
  keeps Claude for longer.
- Hand back after a stand-in: when Claude answers again, the session is
  marked and the turns the local model gave are listed, and what Claude
  did with them is measured.

Issues: #238 (record), #239 (show), #240 (route by headroom), #241 (hand
back).

**Exit:** a week of real use with utilization in the ledger; a threshold
rule in daily use that, on the ledger's evidence, delays or avoids a limit
without a regression; and stand-in turns accounted for when Claude
returns.

### Also in M7

Found in the first days of real use:

- A default log folder per user, not per working folder, so the ledger
  is in one place however Janus is started; `janus stats` reading more
  than one ledger.
- `janus service install` and `uninstall`, writing and loading the
  launchd or systemd unit that the install guide now has people write by
  hand.
- `janus stats -watch`: today's figures and the headroom, redrawn in the
  terminal while a session runs.

Issues: #242 (log folder), #243 (service), #244 (watch).

## What is deliberately late or absent

- **Compression comes after delegation.** It carries the highest risk of
  costing more than it saves (charter R1), and delegation attacks the same
  tokens with none of the cache risk.
- **Model triage is optional.** Rules are expected to cover most traffic;
  the classifier model is added only where the baseline shows rules are not
  enough.
- **No second local runtime or inbound OpenAI format** before 1.0.
