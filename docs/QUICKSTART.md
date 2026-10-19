# Quickstart

From nothing to Claude Code running through Janus, then the first saving.
About ten minutes, most of it LM Studio loading a model.

You need Claude Code working already, and for the second half,
[LM Studio](https://lmstudio.ai) with a model downloaded.
[Before you start](guide/before-you-start.md) says what each part needs
and which settings differ from their defaults.

## 1. Install

On macOS or Linux, `brew install risadams/tap/janus`; on Windows,
`winget install RisAdams.Janus`, or `scoop install risadams/janus` after
`scoop bucket add risadams https://github.com/risadams/scoop-bucket` (all
from v0.9.0 on).
Otherwise download the archive for your machine from the
[releases page](https://github.com/risadams/Janus/releases) and unpack it,
or build it with `go install github.com/risadams/janus/cmd/janus@latest`.
[Installing](guide/install.md) has the commands for each platform and how
to check the download.

```bash
janus version
```

## 2. Check, then run

```bash
janus doctor
```

`doctor` checks the config, the port, that Anthropic is reachable, LM
Studio, and the log directory, and says what to fix. It changes nothing.
Warnings about LM Studio, and about Claude Code not being pointed at Janus
yet, do not matter now.

```bash
janus
```

On its first run Janus writes `janus.config.json` in the directory it was
started from, and prints the address it is listening on and the command
for the next step. Leave it running.

## 3. Point Claude Code at it

In another terminal, start Claude Code through Janus:

```bash
janus claude
```

Use Claude Code as usual. Nothing is different: your login is passed
through, and every request goes to Anthropic as before. Janus prints a
line for each one. Claude Code's own arguments go after it, as in
`janus claude --resume`.

`janus claude` checks first that Janus is answering, then starts
`claude` with two variables set for that session:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8787 ENABLE_TOOL_SEARCH=true claude
```

The first sends Claude Code's requests to Janus. The second is there
because Claude Code turns tool search off when it is pointed anywhere but
Anthropic, and then sends every tool's definition with every request;
setting it keeps requests the size they would be without Janus. With the
settings Janus wrote, `janus claude` works from any folder. Once you
change `listen` or turn on the guard, run it from Janus's folder or give
it `-config`; [installing](guide/install.md#making-it-stick) says more,
including what it does when Janus isn't running.

## 4. See where the tokens go

```bash
janus stats -since 24h
```

This is the first thing Janus is for: tokens, cache use, and cost at list
price, split by the kind of request, the model, and the backend. Use it
for a day before changing anything, so that you know what there is to
save. [The ledger](LEDGER.md) explains the figures.

## 5. The first saving: subagents on the local model

Claude Code's subagents, the helpers it starts to search and read, are the
part of its work a local model does best. Sending them to LM Studio is the
change that saved the most in testing.

Start LM Studio's server, load a model, and note its id. Then in
`janus.config.json`:

```json
"local": {
  "baseUrl": "http://127.0.0.1:1234",
  "models": { "janus-local": { "model": "qwen/qwen3.8-27b" } }
},
"routing": [
  { "name": "subagents", "class": "subagent", "route": "local-then-escalate", "local": "janus-local" }
],
"features": { "route": true, "escalate": false, "compress": false, "guard": false }
```

Set `local.baseUrl` to wherever your LM Studio runs; `http://127.0.0.1:1234`
is right when it is on this machine. The default, `http://studio.local:1234`,
is the Janus developer's own machine and a placeholder only. Run `janus doctor` again: it
now checks that LM Studio has the model and how much context it is loaded
with. Restart Janus.

With `local-then-escalate` the local model answers each subagent request,
Janus checks the answer, and anything broken goes to Claude instead. The
main conversation stays with Claude throughout. Console lines for local
answers name the model with `(local)`, and `janus stats` shows them at no upstream
cost.

Expect subagents to be slower: seconds per turn become tens of seconds.
[The recipe](recipes/claude-code.md) has what else to expect and how to
try the local model in shadow first, without trusting it.

## 6. Keep secrets on your machine

```json
"features": { "route": true, "escalate": false, "compress": false, "guard": true },
"guard": {
  "redact": true,
  "localOnly": { "paths": [".env*", "secrets/**", "*.pem"] }
}
```

With this, credentials Janus recognises are replaced with placeholders
before a request leaves and put back in the answer, and the files named
are withheld from Anthropic whatever is in them. Then:

```bash
janus audit -verify
```

shows what went to Anthropic and re-reads what was sent to check it.
[The privacy recipe](recipes/privacy.md) goes through it.

## Where next

| To | Read |
| --- | --- |
| Run subagents locally, in detail | [Recipe: subagents](recipes/claude-code.md) |
| Try locally first with Claude as the net | [Recipe: local first](recipes/local-first.md) |
| Keep working when Claude is down | [Recipe: outages](recipes/outage.md) |
| Keep secrets and files from leaving | [Recipe: privacy](recipes/privacy.md) |
| Have Claude hand reading to the local model | [Recipe: workers](recipes/workers.md) |
| Know exactly what Janus changes | [What Janus changes](WHAT-JANUS-CHANGES.md) |
| Install, configure, and use Janus in detail | [User guide](guide/README.md) |
| Fix something | [Troubleshooting](TROUBLESHOOTING.md) |
| Look up a setting | [Configuration](CONFIG.md) |
