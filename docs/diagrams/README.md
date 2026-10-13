# Diagrams

The main [README](../../README.md) draws these in Mermaid, which GitHub
renders and which follow light and dark mode. These are the same two
diagrams in an editorial style, for slides, the web, or print.

| Diagram | Open | Embed |
| --- | --- | --- |
| Where Janus sits | [architecture.html](architecture.html) | [architecture.svg](architecture.svg) |
| What happens to one request | [request-flow.html](request-flow.html) | [request-flow.svg](request-flow.svg) |

In the request flow, a request the local-only policy keeps local never
takes a path to Anthropic: if the local model cannot answer it, Janus
refuses it rather than following the "no / down" arrow.

The HTML files are the source. They were made with the
[diagram-design](https://github.com/cathrynlavery/diagram-design) skill in
its default style. Each SVG is exported from its HTML with that skill's
`export_svg.py --system-fonts`, so it loads no web fonts and makes no
network request. Edit the HTML, then export again.
