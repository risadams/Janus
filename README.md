# Janus

[![test](https://github.com/risadams/Janus/actions/workflows/test.yml/badge.svg)](https://github.com/risadams/Janus/actions/workflows/test.yml)

A local proxy, written in Go, that bridges Claude Code to both the Anthropic
API and a local model in LM Studio. Claude is the brain, the local model is
the workhorse, and Janus decides which one each piece of work needs,
local-first, to keep upstream token spend down.

**Status:** M1 in progress. Janus runs as a transparent proxy that keeps
the books: Claude Code works through it unchanged, and every request is
recorded with its token usage. It does not use the local model yet.

- [Charter](docs/CHARTER.md): what Janus is, goals, scope, risks
- [Requirements](docs/REQUIREMENTS.md)
- [Roadmap](docs/ROADMAP.md)
- [Decisions](docs/adr/) and [research notes](docs/research/)
- [Milestones and issues](https://github.com/risadams/Janus/milestones)

## Running

```bash
go build ./cmd/janus
./janus
```

Janus prints the address it is listening on and the exact command that
points Claude Code at it:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8787 ENABLE_TOOL_SEARCH=true claude
```

Your credentials pass through untouched. Settings live in
`janus.config.json`, written on first run; see
[Configuration](docs/CONFIG.md).

Janus writes a [ledger and captures](docs/LEDGER.md) under `logs/`. The
ledger is one line per request with token usage and no content. Captures
are the full requests and responses, including your prompts and code; they
are kept for a week by default and can be switched off.

## Testing

```bash
go test ./...
```

## License

[MIT](LICENSE)
