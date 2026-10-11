# Janus

[![test](https://github.com/risadams/Janus/actions/workflows/test.yml/badge.svg)](https://github.com/risadams/Janus/actions/workflows/test.yml)

A local proxy, written in Go, that bridges Claude Code to both the Anthropic
API and a local model in LM Studio. Claude is the brain, the local model is
the workhorse, and Janus decides which one each piece of work needs,
local-first, to keep upstream token spend down.

**Status:** M0, foundations. The binary builds and reports its version;
nothing proxies yet.

- [Charter](docs/CHARTER.md): what Janus is, goals, scope, risks
- [Requirements](docs/REQUIREMENTS.md)
- [Roadmap](docs/ROADMAP.md)
- [Decisions](docs/adr/) and [research notes](docs/research/)
- [Milestones and issues](https://github.com/risadams/Janus/milestones)

## Building

```bash
go build ./cmd/janus
go test ./...
```

## License

[MIT](LICENSE)
