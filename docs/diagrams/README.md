# Diagrams

The README and the docs site draw Janus with these two diagrams, each in
a light and a dark version that follow the reader's colour scheme.

![Where Janus sits](architecture.svg#gh-light-mode-only)
![Where Janus sits](architecture-dark.svg#gh-dark-mode-only)

![What happens to one request](request-flow.svg#gh-light-mode-only)
![What happens to one request](request-flow-dark.svg#gh-dark-mode-only)

| Diagram | Light | Dark |
| --- | --- | --- |
| Where Janus sits | [architecture.html](architecture.html), [.svg](architecture.svg) | [architecture-dark.html](architecture-dark.html), [.svg](architecture-dark.svg) |
| What happens to one request | [request-flow.html](request-flow.html), [.svg](request-flow.svg) | [request-flow-dark.html](request-flow-dark.html), [.svg](request-flow-dark.svg) |

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
teal lift to `#e24a8a` and `#5ba3b8`. Each SVG is exported from its HTML
with that skill's `export_svg.py --system-fonts`, so it loads no web fonts
and makes no network request. Edit the light HTML, make the dark one
from it again, then export both.

Draw new diagrams the same way, not in Mermaid, so that every picture in
the docs is in the same style.
