# Janus

A local proxy, written in Go, that bridges Claude Code to both the Anthropic
API and a local model in LM Studio. Claude is the brain, the local model is
the workhorse, and Janus decides which one each piece of work needs,
local-first, to keep upstream token spend down.

**Status:** planning. Nothing runs yet.

- [Charter](docs/CHARTER.md): what Janus is, goals, scope, risks
- [Requirements](docs/REQUIREMENTS.md)
- [Roadmap](docs/ROADMAP.md)
- [Milestones and issues](https://github.com/risadams/Janus/milestones)
