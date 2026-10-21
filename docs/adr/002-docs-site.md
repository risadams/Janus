# ADR-002: A documentation website built with Docusaurus

Status: accepted · Date: 2026-10-14 · Amended 2026-10-21 (#404, #417)

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
- The sidebar is written by hand (#417): every page is listed in
  `sidebars.js`, under a short label, in the order a reader meets it.
  Research notes are grouped by topic, each group in the order its
  questions were asked, and decisions by number. Docusaurus's generated
  lists follow file names, which put unrelated notes side by side and
  showed each under its full title. A small site plugin,
  `src/plugins/sidebar-complete.js`, fails the build if a page of main's
  docs is in no sidebar.
- Broken links and anchors fail the build. The `docs` workflow builds the
  site on pull requests that touch it and publishes it to the `gh-pages`
  branch on pushes to `main` and on release tags, so the version it shows
  is the newest release (#212).

## Versions

Added 2026-10-21 (#404). The site published main's docs/ under a badge
for the newest tag, which could be a release candidate. Every pull request
documents its change as it merges, so once v0.10.0's work began, the live
site would have described features the stable release lacks.

- **The default docs are the newest stable release's.** At each stable
  tag, `scripts/tag-release.sh` moves `docs/vX.Y` to it (creating it for a
  new minor version), before pushing the tag. The site's default version
  is docs/ on the newest such branch. main's docs/ is "Next", at
  `/docs/next`, under Docusaurus's banner for unreleased docs, and a
  dropdown switches between the two.
- **docs/ on main stays the only committed copy.**
  `docs-site/scripts/versions.js` writes the branch's docs/, and the
  sidebar as that release had it, into Docusaurus's `versioned_docs/` at
  build time; none of it is committed. A clone with no `docs/v*` branch
  builds main's docs alone, as before.
- **A fix reaches the stable docs by cherry-pick.** It is made on main,
  then cherry-picked onto `docs/vX.Y` and pushed; the push rebuilds the
  site, which publishes only if it builds. The next stable tag's docs
  include the fix already, so moving the branch to the tag loses nothing:
  where the branch cannot fast-forward, the release script adds a commit
  on top of it that takes the tag's files.
- **The badge and footer show the newest stable tag**, never a release
  candidate.
- **`docs/v*` branches are protected** by the "Protect docs branches"
  ruleset: they cannot be deleted or force-pushed, and their history stays
  linear. A pull request is not required, so that the release script can
  move them.

Only the newest stable release and main are built. Older releases' docs
are on their branches on GitHub; building every version would make each
publish slower for pages hardly anyone reads.

## Consequences

Easier: reading the docs, linking to a page, and finding what a page is
for. A broken link in `docs/` is caught by CI.

Harder: the repository now has a Node toolchain to keep current, and a
page that renders on GitHub can fail the site's stricter link check. The
site's look is shared with another repository, so a change to the theme is
best made in both.

Revisit if Docusaurus becomes a burden to keep current, or if GitHub's own
rendering of `docs/` becomes enough.
