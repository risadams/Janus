# Janus — Roadmap

Status: draft for review · Last updated: 2026-10-21

The roadmap is a sequence, not a schedule. Each milestone ends in something
usable and has an exit test. Issues for each milestone are on the
[milestones page](https://github.com/risadams/Janus/milestones).

## Shape

![How the milestones depend on each other: M0 to M5 lead in order to M7, the 1.0 release; M6 Guard needs only M2 and leads to 1.0; M8 Pace needs M3 and, like M9 Distribution, runs beside 1.0; M10 Providers needs M3 and M8 and leads to M11 Codex CLI and M12 Triage, which make v0.10.0](diagrams/roadmap.svg#gh-light-mode-only)
![How the milestones depend on each other: M0 to M5 lead in order to M7, the 1.0 release; M6 Guard needs only M2 and leads to 1.0; M8 Pace needs M3 and, like M9 Distribution, runs beside 1.0; M10 Providers needs M3 and M8 and leads to M11 Codex CLI and M12 Triage, which make v0.10.0](diagrams/roadmap-dark.svg#gh-dark-mode-only)

M6 depends only on M2, so it can be pulled forward if privacy becomes the
pressing goal. M8 depends on M3's fallback and runs beside M7: it does not
hold up 1.0, and its first piece, recording the limits, starts at once so
that the 1.0 report has the data. M9 depends on nothing but the release
pipeline, so its command-line work starts at once; what it publishes to
package managers starts with the first stable tag. Everything else is
ordered by two rules: measure before changing anything, and take the
cheapest, safest savings first.

M10 to M12 are v0.10.0: Claude Code with Claude, a Codex model, or the
local model behind it, and the Codex CLI through Janus. M10 builds on M3's
fallback and M8's reading of usage limits. M11 and M12 each need M10's
list of providers. The release is cut as `v0.10.0-rc.N` builds as the
pieces land, and is tagged stable after the owner's sessions with real
OpenAI traffic (#399), which wait for an OpenAI account.

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

Met on 2026-10-19 ([docs/research/baseline.md](research/baseline.md)):
the main conversation reading its own cache is nearly all the cost, so
routing, escalation, and compression each reach a percent or two of it.
What is left in reach is showing when a session's context makes turns
expensive (#335). Keeping the cache warm through idle gaps would work, but
it needs Janus to hold your credential and send requests of its own, so it
is not built ([keepwarm.md](research/keepwarm.md), #334).

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
how G1–G5 came out. The report is
[docs/research/results-1.0.md](research/results-1.0.md).

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

### M9 — Distribution

Janus is installed by unpacking an archive and is updated by noticing a
new release by hand. Every release so far is a pre-release, and a macOS
binary downloaded in a browser is quarantined. M9 is how Janus reaches
people and stays current, without giving up what the charter promises: no
traffic nobody asked for, and nothing that makes Janus worse than no Janus.

- Colour in every command, not only the console, decided the same way
  everywhere and never in JSON or piped output.
- `janus version -check` and a line in `janus doctor`, with stable
  releases by default and pre-releases on request; a once-a-day notice at
  startup for those who turn it on.
- `janus update`, run by hand, which checks the download against signed
  checksums before replacing the binary and restarts the service if
  Janus runs as one.
- Homebrew, Scoop and winget; signed and notarized macOS binaries once
  the Apple Developer ID arrives; an SBOM; builds anyone can reproduce
  from the tag.
- `doctor` warning when Claude Code is newer than the versions Janus was
  tested with; shell completions; `-json` on `doctor` and `audit`; old
  configs and ledgers still read after a jump of several versions.

Not in M9: Janus never updates itself unasked, and sends nothing to check
for updates unless a command or the config asks it to.

Issues: #254 (colour), #255 (check, with pre-releases), #256 (notice),
#257 (signed checksums), #258 (update), #259 (GoReleaser decision), #260
(Homebrew), #261 (Scoop and winget), #262 (notarization), #263 (SBOM),
#264 (reproducible builds), #265 (release policy), #266 (Scorecard), #267
(Claude Code version), #268 (completions), #269 (config and ledger
versions), #270 (`-json`).

**Exit:** a stable release installed with Homebrew on macOS, without a
Gatekeeper warning, and with Scoop or winget on Windows; an older install brought
to it with `janus update`, which refuses a tampered download; and a CI job
that rebuilds the release and gets the same bytes.

### M10 — Providers

Claude Code keeps its app and gains a choice of brains. Each rule names
its providers in order (Claude, a Codex model through OpenAI's API, or
the local model) and a conversation moves down the list, with everything
it has said, when the one in use cannot answer.

- Map Claude Code's Messages traffic onto OpenAI's Responses API, from
  the published docs, before building (#368); an offline fake OpenAI
  server for the suite (#369).
- Providers in the config, with the OpenAI key from the environment or
  the OS keychain (#370, #371, #405, ADR-005).
- Translation in both directions, the same bytes every turn so OpenAI's
  cache holds; model and effort mapping; OpenAI's reasoning kept between
  its own turns (#372, #373, #374, #410).
- The visible history carried across providers, fitted to the next
  one's context; Claude Code run with no Anthropic account behind it
  (#375, #407, #376).
- The ordered list, the return when a limit resets, and a manual switch
  (#377, #378, #379).
- The guard on every remote provider, and a policy per provider (#380,
  #406).
- Every existing feature given a stated behaviour on the OpenAI path, and
  pass-through still byte for byte (#408, #409).
- Ledger, limits, launchers, doctor, and docs (#381, #382, #383, #384,
  #385); the docs site showing the stable release's docs, with main as
  Next (#404).

**Exit:** on fakes, a Claude Code session moves to OpenAI at a limit and
back after the reset with no lost context, Codex only and local only both
work, and pass-through is unchanged with OpenAI configured.

### M11 — Codex CLI

The Codex CLI goes through Janus as Claude Code does, on its way to
OpenAI. It is a second client, not a second way into Claude.

- What the Codex CLI sends, and whether its ChatGPT login can go through
  a proxy (#386).
- Reading Responses traffic untouched, and passing it through byte for
  byte with its own credential (#387, #388).
- Classes, the guard, the ledger and console, and LM Studio standing in
  (#389, #390, #391, #392).
- `janus codex`, and its docs (#393, #394).

**Exit:** on fakes, the Codex CLI's traffic is byte for byte with every
feature off, guarded with the guard on, and answered locally when OpenAI
cannot answer; with an account, a day of Codex CLI work through Janus
with no behavioural difference.

### M12 — Triage

#32 was closed because rules were enough to choose between Claude and
the local model. Choosing among three providers is a new question.

- A spike on replayed captures, with LM Studio only (#395).
- Triage of a conversation's first turn, once, under a latency budget;
  its decisions recorded and replayable; docs (#396, #397, #398).
- The owner's validation sessions and the release (#399, #400).

**Exit:** triage beats the provider list alone on replayed traffic, by
the numbers, or ships off with the numbers that say why; v0.10.0 tagged
stable once #399 passes.

## What is deliberately late or absent

- **Compression comes after delegation.** It carries the highest risk of
  costing more than it saves (charter R1), and delegation attacks the same
  tokens with none of the cache risk.
- **Model triage is optional.** Rules are expected to cover most traffic;
  the classifier model is added only where the baseline shows rules are not
  enough.
- **No second local runtime, and no OpenAI-format client but the Codex
  CLI**, before 1.0. The Codex CLI's requests never go to Claude.
- **No ChatGPT plan behind Claude Code.** The OpenAI backend uses an API
  key (ADR-005); only the Codex CLI's own login passes through.
