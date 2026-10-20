# Recipe: let Claude hand reading to the local model

`janus mcp` is an MCP server. It gives Claude a tool, `ask_files`, that has
the local model read files and answer a question about them. Claude gets a
short answer with `file:line` references, and the files themselves never
enter its context.

![Handing reading to the local model: Claude calls ask_files, janus mcp has the local model read the files in passes, and Claude gets a short answer instead of the files](../diagrams/ask-files.svg#gh-light-mode-only)
![Handing reading to the local model: Claude calls ask_files, janus mcp has the local model read the files in passes, and Claude gets a short answer instead of the files](../diagrams/ask-files-dark.svg#gh-dark-mode-only)

Why would that help? Everything Claude reads stays in its context and is
read again, from the cache, on every later turn. Twelve files read once
are paid for on every turn after. A few hundred words about them cost
far less to carry.

**Read this first.** It trades time for upstream usage, and the rate is
steep. Measured on two tasks:

| Task | Upstream saving | Slowdown |
| --- | --- | --- |
| Summarise 164 kB of source code | 41% | 12 times slower (8 minutes against 39 seconds) |
| Find one failing line in a 250 kB log | 23% | 27 times slower |

And Claude does not choose it on its own: with the tool available and no
instruction to use it, Claude read the files its own way. You have to ask
for it. See [the measurements](../research/delegation.md).

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
   { "local": { "baseUrl": "http://127.0.0.1:1234",
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

4. Ask for it. In a prompt, or in a project's `CLAUDE.md` if you want it
   used as a matter of course:

   ```
   When a task needs you to read a lot of text you do not need verbatim
   (summarising, explaining, surveying), use the janus ask_files tool to
   read it. Keep searching for specific strings yourself.
   ```

   Only do this where waiting several minutes is acceptable.

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
