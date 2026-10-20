# Diagrams

The README, the docs site, and the pages in `docs/` draw Janus with these
diagrams, each in a light and a dark version that follow the reader's
colour scheme. A diagram is here when a picture says something better
than a paragraph: how the parts fit, what happens over time, or why a
rule is the way it is.

![Where Janus sits](architecture.svg#gh-light-mode-only)
![Where Janus sits](architecture-dark.svg#gh-dark-mode-only)

![What happens to one request](request-flow.svg#gh-light-mode-only)
![What happens to one request](request-flow-dark.svg#gh-dark-mode-only)

| Diagram | Light | Dark |
| --- | --- | --- |
| Where Janus sits | [architecture.html](architecture.html), [.svg](architecture.svg) | [architecture-dark.html](architecture-dark.html), [.svg](architecture-dark.svg) |
| What happens to one request | [request-flow.html](request-flow.html), [.svg](request-flow.svg) | [request-flow-dark.html](request-flow-dark.html), [.svg](request-flow-dark.svg) |
| Why a block once sent never changes | [prompt-cache.html](prompt-cache.html), [.svg](prompt-cache.svg) | [prompt-cache-dark.html](prompt-cache-dark.html), [.svg](prompt-cache-dark.svg) |
| A secret's round trip through the guard | [redaction.html](redaction.html), [.svg](redaction.svg) | [redaction-dark.html](redaction-dark.html), [.svg](redaction-dark.svg) |
| One five-hour window, with pacing and a stand-in | [usage-window.html](usage-window.html), [.svg](usage-window.svg) | [usage-window-dark.html](usage-window-dark.html), [.svg](usage-window-dark.svg) |
| What local-first costs in time | [local-first.html](local-first.html), [.svg](local-first.svg) | [local-first-dark.html](local-first-dark.html), [.svg](local-first-dark.svg) |

| Diagram | Shown in |
| --- | --- |
| Where Janus sits | The README, [the user guide](../guide/README.md) |
| What happens to one request | The README |
| Why a block once sent never changes | [What Janus changes](../WHAT-JANUS-CHANGES.md#why-the-list-is-short) |
| A secret's round trip | [The guard](../GUARD.md#redaction) |
| One five-hour window | [Pacing](../recipes/pace.md) |
| What local-first costs in time | [Local first](../recipes/local-first.md#what-it-costs) |

In the request flow, a request the local-only policy keeps local never
takes a path to Anthropic: if the local model cannot answer it, Janus
refuses it rather than following the "no / down" arrow.

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

Each SVG is exported from its HTML
with that skill's `export_svg.py --system-fonts`, so it loads no web fonts
and makes no network request. Edit the light HTML, make the dark one
from it again, then export both.

Draw new diagrams the same way, not in Mermaid, so that every picture in
the docs is in the same style.
