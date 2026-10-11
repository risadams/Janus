# Janus

[![test](https://github.com/risadams/Janus/actions/workflows/test.yml/badge.svg)](https://github.com/risadams/Janus/actions/workflows/test.yml)

A local proxy, written in Go, that bridges Claude Code to both the Anthropic
API and a local model in LM Studio. Claude is the brain, the local model is
the workhorse, and Janus decides which one each piece of work needs,
local-first, to keep upstream token spend down.

**Status:** M3 in progress. Janus is a transparent proxy that keeps the
books and can send chosen requests to the local model by rule. Claude
Code's subagents can run on LM Studio while the main conversation stays
with Claude, with no change to Claude Code: see the
[recipe](docs/recipes/claude-code.md).

There are three levels of trust in the local model, each a rule: `shadow`
runs it alongside Claude and only compares; `local-then-escalate` checks
each local answer and asks Claude instead if it is broken; `local` uses it
outright. If LM Studio is down, requests go to Claude. The checks catch
broken answers, not wrong ones.

- [Charter](docs/CHARTER.md): what Janus is, goals, scope, risks
- [Requirements](docs/REQUIREMENTS.md)
- [Roadmap](docs/ROADMAP.md)
- [Decisions](docs/adr/) and [research notes](docs/research/)
- [Milestones and issues](https://github.com/risadams/Janus/milestones)

## Running

```bash
go build ./cmd/janus
./janus doctor   # check the setup first
./janus
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
