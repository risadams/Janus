# Recipe: run Claude Code's subagents on the local model

Claude stays the brain: the main conversation goes to Anthropic as usual.
Subagents, which do the reading and the legwork, run on the model in LM
Studio and cost nothing upstream.

Checked on 2026-10-11 with Claude Code 2.1.296 and `qwen/qwen3.8-27b`. All
three ways of sending subagents local described below were run end to end.

## 1. Tell Janus about the local model

In `janus.config.json`, name an alias, point it at a model LM Studio has,
and switch routing on:

```json
{
  "local": {
    "baseUrl": "http://studio.local:1234",
    "models": {
      "janus-local": { "model": "qwen/qwen3.8-27b" }
    }
  },
  "features": { "route": true }
}
```

The alias can be any name. A request for it is answered by the local model;
every other request goes upstream untouched.

Run `janus doctor`. It checks that LM Studio has the model and says whether
it is loaded. Then start `janus`.

## Before you switch: shadow it

To see how the local model would do on your own subagent traffic without
it answering anything, start with a shadow rule:

```json
{
  "routing": [
    { "name": "watch-subagents", "class": "subagent", "route": "shadow", "local": "janus-local" }
  ]
}
```

Work as normal for a while, then run `janus stats` and read the "Shadow
runs" section. See [Configuration](../CONFIG.md#shadow-try-the-local-model-without-trusting-it)
for how to read it.

When you are satisfied, there are two ways to switch on:

- `"route": "local-then-escalate"` checks each local answer before the
  client sees it and asks Claude instead if it is broken. Safer, and
  slower to first output, because the answer is held back while it is
  checked. See [Configuration](../CONFIG.md#local-first-with-claude-as-the-safety-net).
- `"route": "local"` streams the local answer as it is produced, with no
  check on what it says.

Neither can tell a wrong answer from a right one. That is what the shadow
period is for.

## 2. Choose how requests get there

There are two ways. The first needs no change to Claude Code at all.

### With a rule (simplest)

Add a rule that sends the subagent class local:

```json
{
  "routing": [
    { "name": "subagents", "class": "subagent", "route": "local", "local": "janus-local" }
  ]
}
```

Start Claude Code as usual. Janus recognises subagent requests by a header
Claude Code sets, and sends them to the local model. Claude Code still
believes it is talking to the model it asked for, so there is no warning
and nothing to configure on its side.

If the local model cannot take a request (too large, LM Studio down), it
goes to Claude instead. Add `"maxTokens"` to the rule to keep large
subagent turns upstream on purpose.

The same happens while the local model is busy. Claude Code runs
subagents in parallel, and a local model shares your machine's memory, so
Janus sends a model one request at a time by default (`maxConcurrent` in
[`local.models`](../CONFIG.md#local-models)); the other subagent turns go
to Claude meanwhile.

### With the alias

This makes Claude Code itself ask for the local model, which is useful to
pin one particular subagent.

Either of these works. Both were tested.

**All subagents**, with an environment variable:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8787 ENABLE_TOOL_SEARCH=true \
  CLAUDE_CODE_SUBAGENT_MODEL=janus-local claude
```

**One subagent**, in its definition (`.claude/agents/<name>.md`):

```markdown
---
name: line-counter
description: Counts the lines in files.
model: janus-local
tools: Read, Bash
---
You count lines in the files you are given and report one count per file.
```

## Two more things worth setting up

**Keep working when Claude is down.** Add `ifDown` to a rule for the main
conversation, and the local model answers when Claude is unreachable, rate
limited, or failing:

```json
{ "name": "main", "class": "main", "route": "upstream", "ifDown": "janus-local" }
```

**Override for one session.** To bypass local routing, or to keep a session
off Anthropic entirely, start Claude Code with a header:

```bash
ANTHROPIC_CUSTOM_HEADERS="X-Janus-Route: upstream" claude
ANTHROPIC_CUSTOM_HEADERS="X-Janus-Route: local" claude
```

Both are described in [Configuration](../CONFIG.md#keep-working-when-claude-is-down).

## What to expect

- **It is slower.** In the test runs a local turn took 7 to 24 seconds,
  against 1 to 3 seconds for Claude. The first turn of a subagent is the
  slowest; later turns reuse LM Studio's prompt cache.
- **The first request after LM Studio starts waits for the model to load.**
  Load it beforehand to avoid that.
- **With the alias, Claude Code logs `unrecognized_model`.** It is a
  warning and the session carries on. With a rule there is no warning.
- **Claude Code's own cost figure is no longer right.** With the alias it
  does not know what `janus-local` costs; with a rule it prices local
  answers as if Claude had given them. Use `janus stats`, which counts
  local answers as zero upstream cost.
- **With the alias, a request too large for the local model is refused,
  not rerouted.** An alias has no upstream model behind it, so Janus
  returns an error saying how large the request was and what the model's
  context is. With a rule, the same request goes to Claude instead.
- **Quality is the local model's.** A subagent that reads files and
  reports back is within reach of a good local model; one that must make
  hard judgements may not be. To have Janus check each local answer and
  ask Claude when one is broken, use a
  [`local-then-escalate` rule](local-first.md).

In the test, the same small task cost $0.24 at list price with everything
upstream and $0.05 to $0.11 with the subagent local. That is two runs of a
toy task, enough to show the mechanism works and nothing more.

## Not verified

- Pinning Claude Code's small, fast model to the alias
  (`ANTHROPIC_DEFAULT_HAIKU_MODEL`). Headless sessions make no such calls,
  so there was nothing to observe.
- Long subagent conversations, or ones that outgrow the loaded context.
- Any local model other than `qwen/qwen3.8-27b`.

## Checking that it worked

The console line for a locally answered request is marked `[local]`:

```
POST /v1/messages -> 200 (12.106s) [local] qwen/qwen3.8-27b tool_use in=3468 cache_read=2048 out=250
```

and `janus stats` shows the split under "By backend".
