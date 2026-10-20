# Diagrams

The README, the docs site, and the pages in `docs/` draw Janus with these
diagrams, each in a light and a dark version that follow the reader's
colour scheme. A diagram is here when a picture says something better
than a paragraph: how the parts fit, what happens over time, or why a
rule is the way it is.

## Where Janus sits

![Where Janus sits](architecture.svg#gh-light-mode-only)
![Where Janus sits](architecture-dark.svg#gh-dark-mode-only)

Claude Code talks only to Janus; Janus sends most requests to Anthropic,
the ones its rules choose to LM Studio, and records each in the ledger.

Shown in the [README](../../README.md) and [the user
guide](../guide/README.md). Source:
[architecture.html](architecture.html),
[architecture-dark.html](architecture-dark.html).

## What happens to one request

![What happens to one request](request-flow.svg#gh-light-mode-only)
![What happens to one request](request-flow-dark.svg#gh-dark-mode-only)

The path one request takes, from the local-only check to the ledger. A
request the local-only policy keeps local never takes a path to
Anthropic: if the local model cannot answer it, Janus refuses it rather
than following the "no / down" arrow.

Shown in the [README](../../README.md#what-happens-to-a-request).
Source: [request-flow.html](request-flow.html),
[request-flow-dark.html](request-flow-dark.html).

## Why a block once sent never changes

![Why a block once sent never changes](prompt-cache.svg#gh-light-mode-only)
![Why a block once sent never changes](prompt-cache-dark.svg#gh-dark-mode-only)

Each turn resends the whole conversation, and what was sent before is
read from the prompt cache cheaply. Change an earlier block and it, and
everything after it, is written to the cache again at twenty times the
price, which is why Janus never does.

Shown in [What Janus
changes](../WHAT-JANUS-CHANGES.md#why-the-list-is-short). Source:
[prompt-cache.html](prompt-cache.html),
[prompt-cache-dark.html](prompt-cache-dark.html).

## A secret’s round trip through the guard

![A secret’s round trip through the guard](redaction.svg#gh-light-mode-only)
![A secret’s round trip through the guard](redaction-dark.svg#gh-dark-mode-only)

With redaction on, Claude sees only a placeholder, uses it in a tool
call, and Janus puts the real secret back before Claude Code acts.

Shown in [The guard](../GUARD.md#redaction). Source:
[redaction.html](redaction.html),
[redaction-dark.html](redaction-dark.html).

## One five-hour window, with pacing and a stand-in

![One five-hour window, with pacing and a stand-in](usage-window.svg#gh-light-mode-only)
![One five-hour window, with pacing and a stand-in](usage-window-dark.svg#gh-dark-mode-only)

As a usage limit fills, a pacing rule moves subagents to the local model
first, the outage stand-in takes the main conversation once the limit is
reached, and Janus notes when Claude takes over again.

Shown in [Pacing](../recipes/pace.md). Source:
[usage-window.html](usage-window.html),
[usage-window-dark.html](usage-window-dark.html).

## What local-first costs in time

![What local-first costs in time](local-first.svg#gh-light-mode-only)
![What local-first costs in time](local-first-dark.svg#gh-dark-mode-only)

A local answer is held back while it is checked, and a rejected one
makes Claude Code wait for both models.

Shown in [Local first](../recipes/local-first.md#what-it-costs). Source:
[local-first.html](local-first.html),
[local-first-dark.html](local-first-dark.html).

## Where Janus keeps things

![Where Janus keeps things](where-things-live.svg#gh-light-mode-only)
![Where Janus keeps things](where-things-live-dark.svg#gh-dark-mode-only)

What points at what: Claude Code at Janus, the service at the binary and
the config, the config at the log folder, and what is in that folder.

Shown in [Installing](../guide/install.md#2-give-janus-a-folder).
Source: [where-things-live.html](where-things-live.html),
[where-things-live-dark.html](where-things-live-dark.html).

## Where a problem can be

![Where a problem can be](where-it-fails.svg#gh-light-mode-only)
![Where a problem can be](where-it-fails-dark.svg#gh-dark-mode-only)

The four links a request depends on, numbered, with the check that looks
at each. Finding which one failed comes before fixing anything.

Shown in [Troubleshooting](../TROUBLESHOOTING.md). Source:
[where-it-fails.html](where-it-fails.html),
[where-it-fails-dark.html](where-it-fails-dark.html).

## What a turn costs as the conversation grows

![What a turn costs as the conversation grows](context-cost.svg#gh-light-mode-only)
![What a turn costs as the conversation grows](context-cost-dark.svg#gh-dark-mode-only)

The median cost of a main-conversation turn by context size, over a week
of real use. Turns over 600k tokens were an eighth of the turns and a
third of the cost, which is why a /compact between tasks pays.

Shown in [Using
Janus](../guide/use.md#where-the-tokens-went-janus-stats). Source:
[context-cost.html](context-cost.html),
[context-cost-dark.html](context-cost-dark.html).

## Where Janus’s trust boundaries are

![Where Janus’s trust boundaries are](trust-boundaries.svg#gh-light-mode-only)
![Where Janus’s trust boundaries are](trust-boundaries-dark.svg#gh-dark-mode-only)

Your machine, the internet, and your network, and what crosses between
them: the credential to Anthropic over HTTPS, and, if LM Studio runs
elsewhere, plain HTTP to it.

Shown in [The threat model](../THREAT-MODEL.md#who-it-defends-against).
Source: [trust-boundaries.html](trust-boundaries.html),
[trust-boundaries-dark.html](trust-boundaries-dark.html).

## How Janus chooses where a request goes

![How Janus chooses where a request goes](route-decision.svg#gh-light-mode-only)
![How Janus chooses where a request goes](route-decision-dark.svg#gh-dark-mode-only)

A route header first, then a model alias, then the routing rules in
order; the first rule that matches picks one of four routes, and a
request nothing matches goes to Claude untouched.

Shown in [the configuration reference](../CONFIG.md#routing-rules) and
[configuring](../guide/configure.md#how-a-request-is-routed). Source:
[route-decision.html](route-decision.html),
[route-decision-dark.html](route-decision-dark.html).

## When the local model stands in for Claude

![When the local model stands in for Claude](stand-in.svg#gh-light-mode-only)
![When the local model stands in for Claude](stand-in-dark.svg#gh-dark-mode-only)

Which of Claude’s failures bring the local model in at once (a usage
limit), which after three in two minutes (an outage), and which are
passed on as they are.

Shown in [the configuration
reference](../CONFIG.md#keep-working-when-claude-is-down) and [the
outage recipe](../recipes/outage.md#what-happens). Source:
[stand-in.html](stand-in.html),
[stand-in-dark.html](stand-in-dark.html).

## Trusting a local model a step at a time

![Trusting a local model a step at a time](trust-ladder.svg#gh-light-mode-only)
![Trusting a local model a step at a time](trust-ladder-dark.svg#gh-dark-mode-only)

Replay, shadow, local first, local: what the client sees at each step,
and what is at risk.

Shown in [the subagents
recipe](../recipes/claude-code.md#before-you-switch-shadow-it). Source:
[trust-ladder.html](trust-ladder.html),
[trust-ladder-dark.html](trust-ladder-dark.html).

## Handing reading to the local model

![Handing reading to the local model](ask-files.svg#gh-light-mode-only)
![Handing reading to the local model](ask-files-dark.svg#gh-dark-mode-only)

The `ask_files` round trip: Claude gets a short answer, and the files
never enter its context.

Shown in [the workers recipe](../recipes/workers.md). Source:
[ask-files.html](ask-files.html),
[ask-files-dark.html](ask-files-dark.html).

## One machine or two

![One machine or two](machines.svg#gh-light-mode-only)
![One machine or two](machines-dark.svg#gh-dark-mode-only)

Janus always beside Claude Code; LM Studio on the same machine or on
another with more memory.

Shown in [before you
start](../guide/before-you-start.md#where-things-run). Source:
[machines.html](machines.html),
[machines-dark.html](machines-dark.html).

## Where the money goes, by kind of token

![Where the money goes, by kind of token](token-cost.svg#gh-light-mode-only)
![Where the money goes, by kind of token](token-cost-dark.svg#gh-dark-mode-only)

A week of cost split by kind of token: cache reads 63%, one-hour cache
writes 23%, output 14%.

Shown in [the ledger](../LEDGER.md#how-cost-is-worked-out). Source:
[token-cost.html](token-cost.html),
[token-cost-dark.html](token-cost-dark.html).

## Three levels of keeping things in

![Three levels of keeping things in](privacy-levels.svg#gh-light-mode-only)
![Three levels of keeping things in](privacy-levels-dark.svg#gh-dark-mode-only)

Find out what leaves, replace secrets with placeholders, keep named
files local.

Shown in [the privacy recipe](../recipes/privacy.md) and [the
guard](../GUARD.md#two-levels). Source:
[privacy-levels.html](privacy-levels.html),
[privacy-levels-dark.html](privacy-levels-dark.html).

## How compression keeps the original

![How compression keeps the original](compress.svg#gh-light-mode-only)
![How compression keeps the original](compress-dark.svg#gh-dark-mode-only)

A large tool result goes up as a digest the local model wrote, in the
same bytes every turn, while Janus keeps the original for `tool_output`.

Shown in [the configuration reference](../CONFIG.md#compression).
Source: [compress.html](compress.html),
[compress-dark.html](compress-dark.html).

## Claude is the brain, the local model the workhorse

![Claude is the brain, the local model the workhorse](operating-model.svg#gh-light-mode-only)
![Claude is the brain, the local model the workhorse](operating-model-dark.svg#gh-dark-mode-only)

The charter’s four mechanisms (route, escalate, delegate, compress)
around one proxy and its ledger.

Shown in [the charter](../CHARTER.md#3-operating-model). Source:
[operating-model.html](operating-model.html),
[operating-model-dark.html](operating-model-dark.html).

## How the milestones depend on each other

![How the milestones depend on each other](roadmap.svg#gh-light-mode-only)
![How the milestones depend on each other](roadmap-dark.svg#gh-dark-mode-only)

M0 to M5 in order to 1.0; Guard after Route; Pace and Distribution
beside 1.0.

Shown in [the roadmap](../ROADMAP.md#shape). Source:
[roadmap.html](roadmap.html), [roadmap-dark.html](roadmap-dark.html).

## How they are made

The HTML files are the source. They were made with the
[diagram-design](https://github.com/cathrynlavery/diagram-design) skill in
the **Ris Ink** style: warm paper, ink, a single rose accent, teal for
traffic to Anthropic, set in Fraunces and JetBrains Mono. The repo's
`.diagram-design` file (`profile: ris-ink`) makes the skill use that style
for every diagram drawn here; the profile itself lives in
`~/.diagram-design/profiles/ris-ink.md`, copied from the Ris-Ink design
system on claude.ai. Each dark version is its light one with the
profile's light-to-dark rule applied: ink and paper swap, and rose and
teal lift to `#e24a8a` and `#5ba3b8`. In full, every colour in the
light file becomes its dark one, and nothing else changes but the
`-dark` in the `<title>` and `<desc>` ids:

| Light | Dark |
| --- | --- |
| `#faf6ef` paper | `#1a1614` |
| `#fffdf8` raised paper | `#241c19` |
| `#1a1614` ink, and `rgba(26,22,20,x)` | `#f4ecdd`, and `rgba(244,236,221,x)` |
| `#6b625a` muted, and `rgba(107,98,90,x)` | `#a89e8f`, and `rgba(168,158,143,x)` |
| `#776e65` soft, and `rgba(119,110,101,x)` | `#948b7d`, and `rgba(148,139,125,x)` |
| `#CB0162` rose, and `rgba(203,1,98,x)` | `#e24a8a`, and `rgba(226,74,138,x)`; a `0.08` tint becomes `0.10` |
| `#1a5b6e` teal, and `rgba(26,91,110,x)` | `#5ba3b8`, and `rgba(91,163,184,x)` |
| `#6b3a6e` plum, and `rgba(107,58,110,x)` (charts) | `#b183b5`, and `rgba(177,131,181,x)`; a `0.18` fill becomes `0.22` |
| `#8a5a3c` sepia, and `rgba(138,90,60,x)` (charts) | `#b8875f`, and `rgba(184,135,95,x)`; a `0.18` fill becomes `0.22` |

Each SVG is exported from its HTML with that skill's `export_svg.py
--system-fonts`, so it loads no web fonts
and makes no network request. Edit the light HTML, make the dark one
from it again, then export both.

Draw new diagrams the same way, not in Mermaid, so that every picture in
the docs is in the same style.
