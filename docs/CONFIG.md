# Configuration

Janus reads `janus.config.json` from the directory it is run in, or the
file named by `-config`. If the file does not exist, Janus writes one with
the defaults below and carries on.

```json
{
  "listen": "127.0.0.1:8787",
  "allowRemote": false,
  "upstream": { "baseUrl": "https://api.anthropic.com" },
  "local": { "baseUrl": "http://studio.tuesday.local:1234" },
  "features": { "route": false, "escalate": false, "compress": false, "guard": false },
  "logging": { "dir": "logs", "capture": true, "retainDays": 7, "maxMegabytes": 1024 }
}
```

| Field | Meaning |
| --- | --- |
| `listen` | Address Janus accepts connections on. Must be loopback unless `allowRemote` is true. |
| `allowRemote` | Permit a non-loopback `listen`. Janus forwards whatever credentials clients send, so leave this off unless you mean it. |
| `upstream.baseUrl` | The Anthropic API, or a gateway in front of it. |
| `local.baseUrl` | The LM Studio server. |
| `local.models` | Optional. Model names that are answered locally; see below. |
| `routing` | Optional. Rules that decide which requests go local; see below. |
| `features.route` | Switch routing on: aliases in `local.models` and the rules in `routing`. Off by default. |
| `features.escalate`, `compress`, `guard` | Not implemented yet. Janus refuses to start if any is true. |
| `logging.dir` | Where the ledger and captures are written. See [the ledger and captures](LEDGER.md). |
| `logging.capture` | Save every request and response in full. On by default. Captures contain your prompts and code. |
| `logging.retainDays` | Delete captures older than this. `0` keeps them forever. |
| `logging.maxMegabytes` | Delete the oldest captures once they exceed this. `0` means no limit. |

| `pricing` | Optional. Prices to add or correct, by model id. See below. |

A field left out of the file keeps its default. A field Janus does not
recognise is an error, so typos are caught.

## Overrides

Precedence, highest first: flag, environment variable, config file, default.

| Setting | Flag | Environment |
| --- | --- | --- |
| `listen` | `-listen` | `JANUS_LISTEN` |
| `upstream.baseUrl` | `-upstream` | `JANUS_UPSTREAM_URL` |
| `local.baseUrl` | `-local` | `JANUS_LOCAL_URL` |

Overrides apply to the running process only; they are not written back to
the file.

## Local models

`local.models` maps a model name a client asks for to a model in LM Studio.
With `features.route` on, a request for that name is answered locally.

```json
{
  "local": {
    "baseUrl": "http://studio.tuesday.local:1234",
    "models": {
      "janus-local": { "model": "qwen/qwen3.8-27b", "contextTokens": 0, "timeoutSeconds": 900 }
    }
  },
  "features": { "route": true }
}
```

| Field | Meaning |
| --- | --- |
| the key | The name to answer for. Usually a made-up alias; see the [Claude Code recipe](recipes/claude-code.md). |
| `model` | The model's id in LM Studio. Required. |
| `contextTokens` | Largest prompt to send. `0`, the default, uses the context LM Studio has the model loaded with. |
| `timeoutSeconds` | Longest one request may take. `0` means 900. |

Janus has no tokenizer, so it sizes a request at three bytes per token,
which over-counts. A request over the limit is refused with an error; it is
not sent upstream, because an alias has no upstream model.

What changes on the way to LM Studio: the model name, `tool_addition`
blocks (lifted into `tools`), and tools marked `defer_loading` (dropped).
Your Anthropic credentials are removed from the request. On the way back,
the model name is changed to the one the client asked for.

## Routing rules

`routing` is a list of rules, tried in order. The first rule a request
matches decides where it goes; a request that matches none goes upstream
untouched. Rules only apply to Messages requests, and only with
`features.route` on.

```json
{
  "routing": [
    { "name": "subagents", "class": "subagent", "maxTokens": 40000, "route": "local", "local": "janus-local" },
    { "name": "rest", "route": "upstream" }
  ]
}
```

Conditions. A request must meet every one that is set; a rule with none
matches everything.

| Field | Matches when |
| --- | --- |
| `class` | The request is `main` (Claude Code's main conversation), `subagent`, or `unknown` |
| `model` | The client asked for exactly this model |
| `maxTokens` | The request is estimated at no more than this many tokens |
| `tools` | `true`: the request offers tools. `false`: it offers none. |

Outcome.

| Field | Meaning |
| --- | --- |
| `route` | `upstream`, `local`, or `local-then-escalate`. The last is accepted so configs can be written ahead of it, and behaves as `upstream` until it is built. |
| `local` | Which entry of `local.models` to use. Required unless `route` is `upstream`. |
| `name` | Shown in the ledger and in `janus route`. Optional. |

**A rule never fails a request.** If a rule sends a request local and the
local model cannot take it (LM Studio is down, the model is missing, the
request is too large), the request goes upstream exactly as it arrived, and
the ledger records why. This is the difference from an alias in
`local.models`, which has no upstream model to fall back to and is refused.

The client is told the model it asked for answered. The ledger records
which model really did.

An alias always wins over the rules.

### Seeing what a rule would do

```bash
janus route logs/captures/2026-10-11/<id>_v1-messages.request.txt
```

```
Request   claude-opus-5-5, class subagent (from the request's first system block), about 17,289 tokens, 10 tools

  rule subagents: local, to qwen/qwen3.8-27b

Decision  local: qwen/qwen3.8-27b answers, reported to the client as claude-opus-5-5
          changes on the way: set_model, lift_tool_additions, drop_deferred_tools
```

It reads a saved request body and reports each rule tried, why it did or
did not match, and the decision. It asks LM Studio about the model, as a
live request would, and sends nothing else anywhere. A request body does
not carry its headers, so the class is read from the billing line Claude
Code puts at the top of the system prompt; `-class` overrides it.

## Pricing

`janus stats` ships with Anthropic's list prices. To correct one, or to
price a model it does not know, add it under `pricing`, in US dollars per
million tokens:

```json
{
  "pricing": {
    "claude-opus-5-5": {
      "input": 4, "output": 20, "cacheRead": 0.20,
      "cacheWrite5m": 5, "cacheWrite1h": 8
    }
  }
}
```

An entry replaces the shipped one for that model outright, so give all five
rates. See [how cost is worked out](LEDGER.md#how-cost-is-worked-out).
