# Recipe: let Claude hand reading to the local model

`janus mcp` is an MCP server. It gives Claude a tool, `ask_files`, that has
the local model read files and answer a question about them. Claude gets a
short answer with `file:line` references, and the files themselves never
enter its context.

**Read this first.** In the one case measured so far, delegating a read
saved 23% of a small cost and took 27 times as long, because Claude would
not have read the file in full anyway. The tool is for a narrower job than
it first appears. See [the measurement](../research/delegation.md).

## What it is for

Questions that can only be answered by reading all of a large text:

- "Summarise what these twelve files do."
- "What do these design documents say about retries?"
- "Explain how the modules in this directory fit together."

## What it is not for

- **Finding something.** A string, an error, a function name: Claude
  searches for those itself in seconds.
- **Anything needing exact text.** The answer is the local model's
  reading. To edit a file, Claude has to read it.
- **Anything in a hurry.** The local model reads at about a minute per
  45 kB on the hardware this was measured on.

## Set it up

1. Janus needs a config with one local model in it. If you already route
   requests locally, you have one:

   ```json
   { "local": { "baseUrl": "http://studio.tuesday.local:1234",
                "models": { "janus-local": { "model": "qwen/qwen3.8-27b" } } } }
   ```

2. Register the server with Claude Code, giving the config's full path.
   Claude Code starts the server in your project directory, so a relative
   path would not be found.

   ```bash
   claude mcp add janus -- /full/path/to/janus mcp -config /full/path/to/janus.config.json
   ```

   Or, for one session only, put this in a file and pass it with
   `--mcp-config`:

   ```json
   { "mcpServers": { "janus": {
       "command": "/full/path/to/janus",
       "args": ["mcp", "-config", "/full/path/to/janus.config.json"] } } }
   ```

3. Start Claude Code. The tool appears as `mcp__janus__ask_files`.

This is separate from the proxy. It works whether or not Claude Code is
pointed at Janus, and whether or not routing is on.

## Limits

| | |
| --- | --- |
| Where it reads | Only inside the directory it was started in (or `-root`). Paths that lead out, including through symlinks, are refused. |
| How much | At most 60 files and 1 MB per call |
| Globs | `*.go` and `logs/*/x.log` work; `**` does not |
| Binary files | Skipped |
| Answer length | 300 words unless Claude asks for more; never over 1,500 |
| Large inputs | Read in several passes: notes on each part, then an answer from the notes. Detail can be lost between passes. |

## Seeing what it did

Each call is in the ledger, and `janus stats` has a "Delegated work"
section:

```
Delegated work (tools Claude called that the local model carried out)
       tool  calls  failed  files read  size read  avoided at most    time
  ask_files      1       0           1     249 kB           63,733  336.6s
```

"avoided at most" is what reading those files in full would have cost
Claude. It is a ceiling. To know what was really saved, compare the cost
of sessions with and without the tool.

The server's own log goes to standard error, which Claude Code keeps with
its MCP logs.
