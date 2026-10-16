# AGENTS.md

Janus is a Go proxy that sits between Claude Code and the Anthropic API,
and can hand some work to a local model in LM Studio. What it is for, and
what it must never do, is in [docs/CHARTER.md](docs/CHARTER.md); where it
is going is in [docs/ROADMAP.md](docs/ROADMAP.md).

## Checks

CI runs `gofmt -l .` (must print nothing), `go vet ./...`, and
`go test ./...` on Linux, macOS, and Windows, and
`go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...` and
`go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...`; all of them must
pass before a merge. Run them all before pushing, with `go test -race ./...`
locally. The suite is offline: fake upstreams and a fake LM Studio are
`httptest` servers.

Some tests run only when pointed at real traffic or asked for:

| Variable | Runs |
| --- | --- |
| `JANUS_CAPTURES=~/logs/captures` | Parsing, classification, and prompt-cache stability over every captured request |
| `JANUS_PERF=1` | Overhead benchmarks and the soak test in `internal/route` |
| `JANUS_DIGEST_EVAL` and friends | The digest evaluation; see `internal/digest/eval_test.go` |

To loop tests on CI's runners, for a hang or a flake seen only there, run
the `stress` workflow by hand rather than adding a job to a PR, so the
PR's checks stay the two-minute set:
`gh workflow run stress.yml --ref <branch> -f packages=./internal/proxy -f flags="-count=200 -cpu=1 -v"`.
The whole output is kept as the run's `stress-log` artifact
(`gh run download <run id> -n stress-log`).

Fuzz targets (`Fuzz*`) cover the code that reads untrusted input: request
parsing, response streams, the secret-restoring stream, the MCP loop,
release tags, and `tool_output`. CI runs each for 30 seconds; run one longer with
`go test ./internal/guard -run '^$' -fuzz FuzzRestoreAnyCut -fuzztime 5m`.
A failure lands in `testdata/fuzz/`; commit it with the fix, as a
regression case. Seeds are built by hand, never taken from captures.

## Rules that keep Janus safe to change

- **Pass-through is byte-for-byte.** With every feature off, what reaches
  the API and what comes back must match a direct connection.
  `internal/proxy/fidelity_test.go` holds the line; the one sanctioned
  exception is narrowing `Accept-Encoding` on Messages requests.
- **A block once sent upstream is sent in the same bytes on every later
  turn.** Claude Code's turns read nearly everything from the prompt cache,
  and changing anything already sent makes the rest of the conversation a
  fresh cache write at many times the price. Rewrite requests only through
  `internal/transform`, whose memo and net-savings guard enforce this.
  Thinking blocks are never changed: their signatures are checked.
- **Credentials pass through untouched** and are never logged, captured, or
  stored.
- **The guard fails closed.** A request that cannot be checked for secrets
  or local-only content is refused, not sent.
- **Captured traffic, configs, and logs stay out of git.** They hold
  prompts, code, account identifiers, and secrets. Test fixtures are built
  by hand in the shape of real requests.
- **Old configs and ledgers keep working.** People skip versions.
  Renaming, moving, or changing the meaning of a config setting adds a
  migration in `internal/config/version.go`, with a note saying what
  changed; adding one does not. Changing what a ledger field means, or
  removing one, raises `ledger.SchemaVersion`, upgrades older records in
  `ledger.Read`, and adds a fixture in the old shape to
  `internal/ledger/testdata/releases`.
- **Standard library first.** A third-party module needs an ADR
  ([docs/adr/001-dependencies.md](docs/adr/001-dependencies.md)).

## Experiments

- **Upstream:** there is no API key. Run headless Claude Code
  (`claude -p`) with `ANTHROPIC_BASE_URL` pointed at a scratch Janus with
  its own config and log directory. It spends the owner's Pro usage, so
  keep sessions few and short, and say what they will cost first.
- **Local models:** LM Studio answers at `http://studio.local:1234`, on the
  owner's working Mac (36 GB). Load and unload over its REST API
  (`/api/v1/models/load`, `/api/v1/models/unload`), with the smallest
  context the job needs, and watch wired memory (`vm_stat`): a model
  reported as loaded may be running on another machine, and a 27B model
  can take most of this one. Unload what you loaded.
- **Findings go in `docs/research/`**, as a note that ends in a decision
  and says what it does not show.

## Code and docs

- Comments say what code is for and why it is so, in plain full sentences.
  Every package and exported identifier has a doc comment. A choice that
  rests on a measurement cites the research note it came from.
- Errors Janus returns to a client are in the Anthropic API's error shape,
  with messages that begin `janus:` and say what to do.
- Tests are named for the behaviour they pin down
  (`TestUpstreamUnreachableIsExplained`). A fix comes with a test that
  fails without it; check that it does.
- The user docs are in `docs/`: CONFIG, LEDGER, GUARD,
  WHAT-JANUS-CHANGES, TROUBLESHOOTING, and `recipes/`. A change in
  behaviour updates the doc that describes it, in the same PR. The picture
  of the console in the README and the user guide is made by
  `go run ./scripts/consoleshot`, from made-up requests; a change to what
  the console prints reruns it.

## Tracking work

- **Every piece of work has a GitHub issue** in the repo's format (Goal,
  Done when, Traces to, Depends on), a type label (`bug`, `enhancement`,
  `chore`, `spike`, `documentation`) and an `area:` label, a milestone,
  and a card on the Janus project board (owner `risadams`, number 3) moved
  from Todo to In Progress to Done. Large issues get sub-issues.
- **One branch and one PR per issue.** The PR body says `Closes #N`, what
  changed and why, and how it was checked. PRs are squashed into `main`;
  history is linear, so a branch is rebased on `main`, never merged with
  it.
- **Agents may stack PRs and set them to auto-merge**, with
  `gh pr merge <n> --auto --squash`, which waits for the required checks.
  Fix a failing check rather than waiting for the owner. Before a parent
  in a stack merges, retarget each child to `main`
  (`gh pr edit <child> --base main`): deleting the parent's branch closes
  its children instead of retargeting them. After it merges, rebase each
  child with `git rebase --onto main <old parent tip>`. `main` takes a
  PR only once it is up to date, so after anything merges, bring the
  others up with `gh pr update-branch <n> --rebase`; the ruleset binds
  the owner too, so nothing merges past a failing check.
- **Commit titles are plain sentences** saying what the change does
  ("Plan a local attempt's upstream fallback only when it is needed"); the
  body says why. Commits are SSH-signed; if signing asks for a passphrase,
  the key is not in the agent, and the owner has to load it.
