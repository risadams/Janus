# Configuring Janus

Everything Janus does beyond keeping the books is set in one file,
`janus.config.json`, and everything in it is off until you turn it on.
That way a new Janus changes nothing about how Claude Code works, and
each feature you add is one you chose and can check in the ledger. This page explains where it is, how it is laid out,
and how to change it, then gives complete configs for the common setups.
For every field, see the [configuration reference](../CONFIG.md).

## Where the file is

Janus reads `janus.config.json` from the folder you start it in, or the
file you name with `-config`:

```bash
janus -config ~/janus/janus.config.json
```

If the file does not exist, Janus writes one with the defaults and carries
on. Every command (`doctor`, `stats`, `audit`, and the rest) takes the same
`-config`, and reads the same logs, wherever you run it from: a relative
`logging.dir` is taken relative to the config file, not to the current
folder.

## What is in it

The file Janus writes on its first run:

```json
{
  "version": 1,
  "listen": "127.0.0.1:8787",
  "allowRemote": false,
  "upstream": { "baseUrl": "https://api.anthropic.com" },
  "local": { "baseUrl": "http://studio.local:1234" },
  "features": { "route": false, "escalate": false, "compress": false, "guard": false },
  "logging": { "dir": "/Users/you/Library/Application Support/janus", "capture": true, "retainDays": 7, "maxMegabytes": 1024 }
}
```

> **`studio.local` is a placeholder.** `http://studio.local:1234` is the
> Janus developer's own LM Studio machine, and the default only because
> Janus was built against it. Set `local.baseUrl` to where your LM Studio
> runs: `http://127.0.0.1:1234` on the same machine, or
> `http://<its address>:1234` on another. `janus doctor` says whether it
> answers.

| Section | What it is for |
| --- | --- |
| `listen`, `allowRemote` | The address Claude Code connects to. Loopback only, unless you set `allowRemote`. |
| `upstream` | Where Claude is: the Anthropic API, or a gateway in front of it. |
| `local` | Where LM Studio is (`baseUrl`), and the models Janus may use there (`models`). |
| `features` | The switches. `route` turns on aliases and routing rules; `guard` the secret and private-file protection; `compress` the digests. `escalate` is reserved and must stay `false`. |
| `routing` | Rules that decide which requests a local model answers. |
| `guard` | What the guard does: replace secrets, and which files never leave. |
| `compress` | Settings for digests. |
| `logging` | Where the ledger and captures go, and how long captures are kept. |
| `pricing` | Prices to add or correct, for the cost estimates. |

A field you leave out keeps its default, so a config only needs what you
change. A field Janus does not recognise stops it starting, so a typo is
caught rather than ignored:

```
janus: /Users/you/janus/janus.config.json: json: unknown field "lisen"
```

## Changing it

1. Edit the file.
2. Run `janus doctor`. It reads the config as Janus will, reports every
   problem it finds, and checks what the config points at: that LM Studio
   answers, that each local model exists there, and how much context it
   is loaded with.
3. Restart Janus. It reads the config only when it starts.

When Janus starts, it prints what it will do with each request. Check
that it says what you meant:

```
  Routing     model "janus-local" -> local qwen/qwen3.8-27b
              rule main: class main -> upstream, or local qwen/qwen3.8-27b if Claude cannot answer
              rule subagents: class subagent -> upstream, or local qwen/qwen3.8-27b if Claude cannot answer
```

For one-off changes, three settings can be overridden without editing the
file, by flag or environment variable: `-listen` or `JANUS_LISTEN`,
`-upstream` or `JANUS_UPSTREAM_URL`, `-local` or `JANUS_LOCAL_URL`.

## How a request is routed

With `features.route` on, Janus decides where each request goes in this
order:

![How Janus chooses where a request goes: a route header first, then a model alias, then the rules in order, the first match choosing upstream, shadow, local-then-escalate, or local; a request nothing matches goes to Claude untouched](../diagrams/route-decision.svg#gh-light-mode-only)
![How Janus chooses where a request goes: a route header first, then a model alias, then the rules in order, the first match choosing upstream, shadow, local-then-escalate, or local; a request nothing matches goes to Claude untouched](../diagrams/route-decision-dark.svg#gh-dark-mode-only)

1. **A route header.** `X-Janus-Route`, if a request carries it, beats
   everything below ([choosing the route
   yourself](use.md#choosing-the-route-yourself)).
2. **An alias.** A request for a model named in `local.models` (such as
   `janus-local`) goes to that local model.
3. **The rules**, in the order they are listed. The first rule a request
   matches decides. Rules match on the request's class (`main`,
   `subagent`, and a few of Claude Code's background requests), the model
   asked for, its size, and whether it offers tools.
4. **Anything else** goes to Claude untouched.

A rule's `route` says what happens to the requests it matches:

| `route` | What happens |
| --- | --- |
| `upstream` | Claude answers. With a `fallback` list, the next on it answers instead when Claude cannot. |
| `shadow` | Claude answers, and the local model answers the same request on the side, for comparison only. |
| `local-then-escalate` | The local model answers first. Janus checks the answer, and if it is broken, or takes longer than `maxSeconds`, Claude answers instead. |
| `local` | The local model answers. If it cannot take the request at all, Claude does. |

A rule never makes a request fail: whenever the local model cannot help,
the request goes to Claude. To see where a particular request would go,
use [`janus route`](use.md#where-would-this-go-janus-route).

## Complete configs

Each of these is a whole `janus.config.json`. Start from the one closest
to what you want. All of them assume LM Studio on the same machine, with
`qwen/qwen3.8-27b`; change `baseUrl` and `model` to yours.

### Just keep the books

Janus forwards everything untouched and writes a ledger line per request.
This one also turns captures off, so no prompts or code are kept on disk:

```json
{
  "listen": "127.0.0.1:8787",
  "logging": { "capture": false }
}
```

Use this for a day or two before changing anything, so that
`janus stats` can show you where the tokens go.

### Subagents on the local model, with Claude as the net

Claude Code's subagents, the helpers it starts to search and read, are
the work a local model does best. Here the local model answers them
first, and Claude takes over whenever an answer is broken or slow. The
main conversation stays with Claude.

```json
{
  "local": {
    "baseUrl": "http://127.0.0.1:1234",
    "models": { "janus-local": { "model": "qwen/qwen3.8-27b" } }
  },
  "features": { "route": true },
  "routing": [
    { "name": "subagents", "class": "subagent", "route": "local-then-escalate", "local": "janus-local" }
  ]
}
```

Expect subagents to be slower: a local turn takes tens of seconds where
Claude takes a few. Watch the "Local first" table in `janus stats`: if
most attempts are rejected, the model is not up to the work, and you are
paying for both. The [subagents recipe](../recipes/claude-code.md) has
more, including how to shadow the model first.

### Keep working when Claude is down

Claude answers everything as usual. When Claude cannot, because it is
unreachable, overloaded, or you have used up your plan's limit, the local
model answers instead:

```json
{
  "local": {
    "baseUrl": "http://127.0.0.1:1234",
    "models": { "janus-local": { "model": "qwen/qwen3.8-27b" } }
  },
  "features": { "route": true },
  "routing": [
    { "name": "main", "class": "main", "route": "upstream", "fallback": ["upstream", "janus-local"] },
    { "name": "subagents", "class": "subagent", "route": "upstream", "fallback": ["upstream", "janus-local"] }
  ]
}
```

This costs nothing while Claude is healthy: LM Studio is not even asked.
During an outage Janus loads the model if it needs to, queues requests
for it, and cuts a conversation too long for it down to its start and its
latest turns. It is slower and less capable than Claude, but the session
keeps going. In auto mode, press Shift+Tab to approve tool calls yourself
while it lasts; [using Janus](use.md#when-claude-is-down) shows what an
outage looks like. The [outage recipe](../recipes/outage.md) has the
details.

### Keep secrets and private files on your machine

The guard replaces credentials it recognises with placeholders before a
request leaves, and puts them back in the answer. The files named under
`localOnly` never reach Anthropic, whatever is in them:

```json
{
  "features": { "guard": true },
  "guard": {
    "redact": true,
    "localOnly": { "paths": [".env*", "secrets/**", "*.pem"], "action": "withhold" }
  }
}
```

With `"action": "withhold"`, the protected content is cut out of the
request and the rest goes to Claude; Claude is told it was withheld. With
`"action": "local"`, a request holding protected content is answered by
the local model instead, or refused. Then run `janus audit -verify` to
see what left your machine. The [privacy recipe](../recipes/privacy.md)
and [the guard](../GUARD.md) go further.

### All of it

The setup Janus is developed with: subagents tried locally first, a
stand-in for the main conversation when Claude is down, and the guard on.

```json
{
  "listen": "127.0.0.1:8787",
  "local": {
    "baseUrl": "http://127.0.0.1:1234",
    "models": {
      "janus-local": { "model": "qwen/qwen3.8-27b", "contextTokens": 65536, "maxConcurrent": 1 }
    }
  },
  "features": { "route": true, "guard": true },
  "routing": [
    { "name": "main", "class": "main", "route": "upstream", "fallback": ["upstream", "janus-local"] },
    { "name": "subagents", "class": "subagent", "route": "local-then-escalate", "local": "janus-local", "maxSeconds": 90 }
  ],
  "guard": {
    "redact": true,
    "localOnly": { "paths": [".env*", "secrets/**", "*.pem"] }
  },
  "logging": { "capture": true, "retainDays": 7, "maxMegabytes": 1024 }
}
```

## Sizing the local model

Three settings on a `local.models` entry decide how hard Janus works the
model:

| Setting | Default | What to know |
| --- | --- | --- |
| `contextTokens` | the loaded context, or 65,536 when Janus loads it | The largest request Janus sends the model, and the context it loads the model with. A bigger context takes more memory. A model already loaded is never reloaded. |
| `maxConcurrent` | 1 | How many requests the model gets at once. Each runs in your machine's memory; on a 36 GB Mac, two at once took a 27B model past 31 GB. |
| `timeoutSeconds` | 900 | The longest one request may take. |

Claude Code's main conversation starts at 25,000 to 35,000 tokens before
any work, and grows. A context much under 65,536 tokens leaves little
room.

## Logs and captures

Janus writes to `logging.dir`, by default a folder per user: macOS
`~/Library/Application Support/janus`, Linux `$XDG_STATE_HOME/janus` (or
`~/.local/state/janus`), Windows `%LocalAppData%\janus`. Below, and in the
examples elsewhere, `logs` stands for that folder:

| What | Where | Kept |
| --- | --- | --- |
| The ledger: one line per request, no content | `logs/ledger/` | Forever; it is small |
| Captures: every request and response in full | `logs/captures/` | `retainDays` (7) days, and at most `maxMegabytes` (1,024 MB) |
| Originals of digested tool output | `logs/originals/` | `retainDays` days |
| The guard's placeholder key | `logs/redact.key` | Until you delete it |

Captures hold your prompts and code, readable by your user only. Set
`logging.capture` to `false` if you don't want them; `janus stats` works
without them, but `janus replay` and `janus route` need them.

Next: [using Janus](use.md).
