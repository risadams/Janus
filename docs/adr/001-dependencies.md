# ADR-001: Dependency policy

Status: accepted · Date: 2026-10-11

## Context

blush, which Janus ports from, uses only the Go standard library. That gave
it a single static binary, trivial cross-compilation, an offline test suite,
and nothing to audit or update.

Janus does more than blush: it parses and rewrites request bodies, speaks
MCP, and keeps a ledger. Some of that is tedious without libraries. But
Janus also sits in the path of every request and sees credentials and
source code, so each dependency is code that runs in a sensitive position.

## Decision

Standard library first. A third-party module may be added only when all of
these hold:

1. The standard library cannot do the job in a few hundred lines of clear
   code.
2. The module is pure Go (no cgo), so the single static binary and
   cross-compilation are kept.
3. An ADR records what it is for, what was considered instead, and what it
   pulls in transitively.

Test-only dependencies follow the same rule. The test suite stays runnable
offline with `go test ./...`.

Consequences already accepted by this decision:

- Config is JSON (`encoding/json`), not YAML or TOML.
- The ledger is JSON Lines in a file, not SQLite.
- The MCP server implements the small part of JSON-RPC it needs directly,
  unless the M4 work shows that is a poor trade, in which case that gets its
  own ADR.

## Consequences

Easier: building, releasing, auditing, and trusting the binary.

Harder: JSON with comments in config is not available, so the starter
config must be explained by the docs and by `janus doctor` rather than
inline. Ledger queries are scans, not SQL; acceptable at single-user volume
and worth revisiting if `janus stats` becomes slow.

Revisit if a dependency would remove a class of bugs in the request path
(for example a proven SSE or JSON-schema implementation) rather than merely
save typing.
