# Contributing

Thanks for wanting to help. Janus is small and opinionated, so a short read
here saves a round trip on a pull request.

- **Start from an issue.** Every change has one, in the repository's format:
  Goal, Done when, Traces to, Depends on. The issue templates set it out.
- **One branch and one pull request per issue.** The PR says `Closes #N`,
  what changed and why, and how you checked it.
- **Keep history linear.** `main` accepts no merge commits: rebase your
  branch on `main` to bring it up to date.
- **Run the checks** CI runs: `gofmt -l .` (prints nothing), `go vet ./...`,
  and `go test -race ./...`. The suite is offline.
- **Update the docs** that describe any behaviour you change, in the same
  pull request. They are Markdown in `docs/`, published as the
  [docs site](https://risadams.github.io/Janus/); to preview it,
  `cd docs-site && npm ci && npm start`. Its build fails on a broken link.
- **Never commit captures, logs, or a config.** They hold prompts, code, and
  credentials.

Releases are cut by the owner with `scripts/tag-release.sh`, which picks
the next version (`rc`, `final`, or `patch`/`minor`/`major`, with `--rc`
for a first candidate), checks that CI passed on `main`, and pushes a
signed tag. It asks before pushing; `--yes` skips that, and with `-m` lets
it run where there is no terminal, such as `!` in Claude Code. The release
workflow builds the binaries and publishes the release from that tag.
Don't create the release on GitHub by hand: the workflow cannot publish
over one that already exists.

[AGENTS.md](AGENTS.md) has the full conventions: the rules that keep Janus
safe to change, how comments and tests are written, and how experiments
against the real API and local models are run. It's written for coding
agents and reads just as well for people.

Everyone taking part is expected to follow the
[code of conduct](CODE_OF_CONDUCT.md). Security problems go through
[SECURITY.md](SECURITY.md), not an issue.
