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

Each SVG is exported from its HTML with that skill's `export_svg.py
--system-fonts`, so it loads no web fonts
and makes no network request. Edit the light HTML, make the dark one
from it again, then export both.

Draw new diagrams the same way, not in Mermaid, so that every picture in
the docs is in the same style.
