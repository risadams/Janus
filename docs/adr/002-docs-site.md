# ADR-002: A documentation website built with Docusaurus

Status: accepted · Date: 2026-10-14

## Context

Janus's documentation is Markdown in `docs/`, written to be read on GitHub:
a user guide, recipes, a reference, and the project's charter, research,
and decisions. On GitHub it has no navigation between pages, no search,
and no front page, and a newcomer has to know which file to open.

The owner's other projects publish their docs as a Docusaurus site on
GitHub Pages (risadams/docker-registry.helm, `docs-site/`), in the Ris Ink
look of risadams.com. Doing the same here keeps one way of working and one
theme across them.

ADR-001 keeps third-party Go modules out of the binary. A docs site is a
different kind of dependency: it is built in CI and never ships in or
links into Janus.

## Decision

`docs-site/` holds a Docusaurus 3 site, copied in structure and theme from
docker-registry.helm's: the same `custom.css`, a homepage, a navbar logo,
and the classic preset reading `../docs` directly, so the Markdown in
`docs/` stays the only copy of the docs and still reads correctly on
GitHub.

- Its npm dependencies are pinned by `package-lock.json` and kept current
  by Dependabot. Nothing under `docs-site/` is imported by Go code, and the
  Go test suite does not need Node.
- `.md` files are read as CommonMark, not MDX, so the docs need nothing
  Docusaurus-specific.
- Links that leave `docs/` (the README, SECURITY, source files) are
  rewritten to GitHub by a small remark plugin, `src/remark/repo-links.js`,
  rather than edited in the Markdown.
- Broken links and anchors fail the build. The `docs` workflow builds the
  site on pull requests that touch it and publishes it to the `gh-pages`
  branch on pushes to `main`.

## Consequences

Easier: reading the docs, linking to a page, and finding what a page is
for. A broken link in `docs/` is caught by CI.

Harder: the repository now has a Node toolchain to keep current, and a
page that renders on GitHub can fail the site's stricter link check. The
site's look is shared with another repository, so a change to the theme is
best made in both.

Revisit if Docusaurus becomes a burden to keep current, or if GitHub's own
rendering of `docs/` becomes enough.
