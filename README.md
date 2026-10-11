# Janus

[![test](https://github.com/risadams/Janus/actions/workflows/test.yml/badge.svg)](https://github.com/risadams/Janus/actions/workflows/test.yml)

A local proxy, written in Go, that bridges Claude Code to both the Anthropic
API and a local model in LM Studio. Claude is the brain, the local model is
the workhorse, and Janus decides which one each piece of work needs,
local-first, to keep upstream token spend down.

**Status:** M6 (privacy) complete; M5 and M7 to come. Janus is a transparent proxy that keeps the
books and can send chosen requests to the local model by rule. Claude
Code's subagents can run on LM Studio while the main conversation stays
with Claude, with no change to Claude Code: see the
[recipe](docs/recipes/claude-code.md).

There are three levels of trust in the local model, each a rule: `shadow`
runs it alongside Claude and only compares; `local-then-escalate` checks
each local answer and asks Claude instead if it is broken; `local` uses it
outright. If LM Studio is down, requests go to Claude; and a rule can have
the local model stand in when Claude is down. The checks catch broken
answers, not wrong ones.

With `features.guard` on, Janus looks for credentials in requests going
upstream and records what it finds. With `guard.redact` as well, it
replaces each one with a placeholder before the request leaves and puts it
back in the answer, so Claude can use a secret without seeing it.
A local-only policy goes further: name files, directories, or a marker,
and what is in them is withheld from Anthropic whatever it looks like, or
the request is answered by the local model instead. `janus audit` shows
what went to Anthropic and, with `-verify`, re-reads what was sent to
check it. See [the guard](docs/GUARD.md).

`janus mcp` is a separate piece: an MCP server giving Claude a tool that
has the local model read files and report back. It trades time for
upstream usage at a steep rate (41% less cost and 12 times slower, on the
task that suited it best), and Claude only uses it when asked; read
[the recipe](docs/recipes/workers.md) before using it.

**Using it**

- [Quickstart](docs/QUICKSTART.md): install, run, and the first saving
- Recipes: [subagents on the local model](docs/recipes/claude-code.md),
  [local first with Claude as the net](docs/recipes/local-first.md),
  [keep working when Claude is down](docs/recipes/outage.md),
  [keep secrets and files from leaving](docs/recipes/privacy.md),
  [hand reading to the local model](docs/recipes/workers.md)
- [Configuration](docs/CONFIG.md), [the guard](docs/GUARD.md), and
  [the ledger and captures](docs/LEDGER.md)
- [What Janus changes, and how to check](docs/WHAT-JANUS-CHANGES.md)
- [Troubleshooting](docs/TROUBLESHOOTING.md)
- [Threat model](docs/THREAT-MODEL.md): what it protects against, and what it does not

**About the project**

- [Charter](docs/CHARTER.md), [requirements](docs/REQUIREMENTS.md), and [roadmap](docs/ROADMAP.md)
- [Decisions](docs/adr/) and [research notes](docs/research/), including
  [performance](docs/research/performance.md)
- [Milestones and issues](https://github.com/risadams/Janus/milestones)

## Installing

Download the archive for your machine from the
[releases page](https://github.com/risadams/Janus/releases), check it, and
unpack it. There is one binary and nothing else to install.

```bash
# macOS on Apple silicon. Put the current version in place of 1.0.0; the
# other builds are darwin_amd64, linux_amd64, and linux_arm64.
curl -LO https://github.com/risadams/Janus/releases/download/v1.0.0/janus_1.0.0_darwin_arm64.tar.gz
curl -LO https://github.com/risadams/Janus/releases/download/v1.0.0/checksums.txt
shasum -a 256 -c checksums.txt --ignore-missing
tar -xzf janus_1.0.0_darwin_arm64.tar.gz
./janus_1.0.0_darwin_arm64/janus version
```

On Windows, download `janus_1.0.0_windows_amd64.zip`, unpack it, and run
`janus.exe` from a terminal. Compare its SHA-256 with `checksums.txt`
using `Get-FileHash`.

The binaries are not signed. A copy downloaded with a browser on macOS is
quarantined and will be refused; `curl` does not set the quarantine, or
clear it with `xattr -d com.apple.quarantine janus`.

With Go 1.25 or later you can build it instead:

```bash
go install github.com/risadams/janus/cmd/janus@latest
```

## Running

```bash
janus doctor   # check the setup first
janus
```

`janus doctor` checks the config, the port, the upstream, LM Studio, and
the log directory, and says what to fix. It writes nothing.

Janus prints the address it is listening on and the exact command that
points Claude Code at it:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8787 ENABLE_TOOL_SEARCH=true claude
```

Your credentials pass through untouched, and so does everything else, with
one exception: on Messages requests Janus asks the upstream to compress
with gzip rather than brotli or zstd, so that it can read the response. Settings live in
`janus.config.json`, written on first run; see
[Configuration](docs/CONFIG.md).

Janus writes a [ledger and captures](docs/LEDGER.md) under `logs/`. The
ledger is one line per request with token usage and no content. Captures
are the full requests and responses, including your prompts and code; they
are kept for a week by default and can be switched off.

```bash
./janus stats -since 24h
```

shows tokens, cache hit rate, and estimated cost by request class, model,
and backend.

## Testing

```bash
go test ./...
```

## License

[MIT](LICENSE)
