# Configuration

Janus reads `janus.config.json` from the directory it is run in, or the
file named by `-config`. If the file does not exist, Janus writes one with
the defaults below and carries on.

```json
{
  "listen": "127.0.0.1:8787",
  "allowRemote": false,
  "upstream": { "baseUrl": "https://api.anthropic.com" },
  "local": { "baseUrl": "http://studio.local:1234" },
  "features": { "route": false, "escalate": false, "compress": false, "guard": false },
  "logging": { "dir": "logs", "capture": true, "retainDays": 7, "maxMegabytes": 1024 }
}
```

| Field | Meaning |
| --- | --- |
| `listen` | Address Janus accepts connections on. Must be loopback unless `allowRemote` is true. |
| `allowRemote` | Permit a non-loopback `listen`. Janus forwards whatever credentials clients send, so leave this off unless you mean it. While it is off, Janus also refuses requests from web pages: any whose `Host` is not this machine or whose `Origin` is a page from elsewhere. |
| `upstream.baseUrl` | The Anthropic API, or a gateway in front of it. |
| `local.baseUrl` | The LM Studio server. |
| `local.models` | Optional. Model names that are answered locally; see below. |
| `routing` | Optional. Rules that decide which requests go local; see below. |
| `features.route` | Switch routing on: aliases in `local.models` and the rules in `routing`. Off by default. |
| `features.guard` | Look for secrets in requests going upstream, and record what is found. See [the guard](GUARD.md). |
| `guard` | Optional. `redact` to replace secrets before they leave; your own patterns, an allow list, and the entropy search; and `localOnly`, which keeps named files, directories, and marked content from going upstream at all. See [the guard](GUARD.md). |
| `features.escalate`, `features.compress` | Reserved. Janus refuses to start if either is true. Local-first routing is a rule, not a feature switch. |
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
    "baseUrl": "http://studio.local:1234",
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
| `route` | `upstream`, `local`, `shadow`, or `local-then-escalate`. The last two are described below. |
| `local` | Which entry of `local.models` to use. Required unless `route` is `upstream`. |
| `name` | Shown in the ledger and in `janus route`. Optional. |
| `skipChecks` | For `local-then-escalate`: checks to leave out. See below. |
| `maxSeconds` | For `local-then-escalate`: how long the local model gets before Claude is asked. Default 120. |
| `ifDown` | For `upstream` and `shadow`: an entry of `local.models` to answer instead when Claude cannot. See below. |

**A rule never fails a request.** If a rule sends a request local and the
local model cannot take it, the request goes to Claude instead and the
ledger records why. That covers LM Studio being down, the model missing,
the request too large, the local model timing out before it answers, and
the local model answering with an error. This is the difference from an
alias in `local.models`, which has no upstream model to fall back to and is
refused.

There is one limit. With `"route": "local"` the answer is streamed to the
client as it is produced, so once any of it has been sent, a failure
part-way cannot be undone. `local-then-escalate` does not have that limit,
because it holds the answer back.

After three failures in a row to reach the local model, rules stop trying
it for thirty seconds and send everything to Claude, so that a dead LM
Studio costs a few requests a short delay and not every request a long
one. The next request after that tries again.

The client is told the model it asked for answered. The ledger records
which model really did.

An alias always wins over the rules.

### Local first, with Claude as the safety net

A rule with `"route": "local-then-escalate"` sends the request to the local
model, holds its whole answer back, and checks it. If it passes, the client
gets it. If it fails, the request goes to Claude and the client gets
Claude's answer. Either way the client receives one normal response and
cannot tell which happened.

```json
{ "name": "subagents", "class": "subagent", "route": "local-then-escalate", "local": "janus-local", "maxSeconds": 60 }
```

The checks, by the names `skipChecks` takes:

| Check | Fails an answer that |
| --- | --- |
| `empty` | Has no text and no tool call. Small models do this when their tool call is lost on the way out. |
| `truncated` | Ran out of tokens |
| `tools` | Calls a tool that was not offered |
| `schema` | Passes a tool arguments that are not valid JSON, or that do not fit the tool's schema |
| `refusal` | Is a short refusal or apology and not an attempt |

Always checked, whatever is skipped: the answer finished, was not an error,
and could be read.

**What the checks cannot do.** They catch answers that are broken. They do
not catch answers that are wrong. A long, well-formed, useless answer
passes every one of them. In testing, a small model had three of four
answers accepted and still left the task undone, so that Claude had to do
the work itself afterwards. Run a `shadow` rule or `janus replay` on the
model first, and read `.shadow.md` files, before trusting this route with
it.

**What it costs.** The client waits for the local model's whole answer
before seeing anything, where a `local` rule streams it. When the answer is
rejected, the time spent on it is lost and Claude's time is added. So:

- `maxSeconds` bounds the wait. A local model that rambles is cut off and
  Claude is asked. The default is 120; set it nearer to how long a good
  answer from your model takes.
- The schema check understands `type`, `properties`, `required`,
  `additionalProperties: false`, `items`, and `enum`. Anything else in a
  schema is ignored, so it can pass arguments a full validator would
  reject, and never the reverse.

`janus stats` reports how it is going:

```
Local first (the local model tried, with Claude as the fallback)
       class  tried  accepted  rejected  failed  accepted %  attempt time
    subagent      4         4         0       0        100%         10.2s
```

Rejected answers failed a check; failed ones found the local model
unavailable. A rejected answer is saved beside the request's capture as
`.attempt.response.txt`, so you can see what was thrown away.

A conversation can move between the local model and Claude turn by turn.
Janus removes the local model's thinking from the history before Claude
sees it, because Claude rejects thinking it did not produce.

### Keep working when Claude is down

A rule that sends requests upstream can name a local model to stand in when
Claude cannot answer:

```json
{ "name": "main", "class": "main", "route": "upstream", "ifDown": "janus-local" }
```

The local model answers when Claude is unreachable, or responds with a rate
limit (429) or a server error (500 and above, which includes
"overloaded"). On a subscription, a rate limit is what running out of usage
looks like, so this also covers that.

Any other error, such as a bad request or an expired login, is about the
request and not about Claude being down. It reaches the client untouched
and the local model is not consulted.

- **It is degraded service.** The client is told the model it asked for
  answered, so nothing in the conversation shows the difference. What does
  show it: the response carries an `X-Janus-Fallback` header, the console
  line says so, the ledger records it, and `janus stats` prints a note
  with the count.
- **It only works before the answer starts.** If Claude fails part-way
  through a streamed answer, what was sent cannot be taken back.
- **If the local model cannot help either** (too large a request, LM Studio
  down), the client gets Claude's error exactly as Claude sent it.
- **It costs nothing while Claude is healthy.** The fallback is only worked
  out when it is needed; LM Studio is not contacted otherwise.
- The conversation can go back to Claude on the next turn.

With this on the main-conversation rule and a `local` rule for subagents, a
whole Claude Code session runs with no connection to Anthropic at all. That
was tested; it is as good as the local model is, and much slower.

### Choosing the route yourself

A request can say where it wants to go with a header, which beats every
rule:

| `X-Janus-Route` | Effect |
| --- | --- |
| `upstream` | Send it to Claude, whatever the rules say |
| `local` | Send it to the local model, if exactly one is configured |
| `local=<name>` | Send it to that entry of `local.models` |

Claude Code sends extra headers from an environment variable, so this
works for a whole session:

```bash
ANTHROPIC_CUSTOM_HEADERS="X-Janus-Route: upstream" claude   # bypass local routing for this session
ANTHROPIC_CUSTOM_HEADERS="X-Janus-Route: local" claude      # keep this session off Anthropic
```

**Forcing local is a promise that the request does not go upstream.** If
the local model cannot take it, the request is refused with an error
saying why. It is never quietly sent to Claude instead.

The header is an instruction to Janus and is removed before the request
goes anywhere. It needs `features.route` on.

### Shadow: try the local model without trusting it

A rule with `"route": "shadow"` changes nothing for the client. The request
goes upstream and Claude's answer comes back as usual. Afterwards, Janus
runs the same request on the local model and records how its answer
compares.

```json
{ "name": "watch-subagents", "class": "subagent", "route": "shadow", "local": "janus-local" }
```

Use it to judge a class of request on your own traffic before writing the
rule that sends it local. Run it for a while, then read the "Shadow runs"
section of `janus stats`:

```
Shadow runs (the local model answering alongside the real answer)
       class  runs  skipped  failed  compared  same stop  same tools  both  local  upstream
    subagent     4        0       0         4        75%         50%   50%  10.4s      2.3s
```

| Column | Meaning |
| --- | --- |
| runs | Shadows asked for |
| skipped | Never sent: the request was too large for the model, or the queue was full |
| failed | Sent, and came back as an error, cut off, or empty |
| compared | Came back whole |
| same stop | Of those, how many stopped for the same reason as Claude (finished, or asked for a tool) |
| same tools | How many called the same tools the same number of times, in any order |
| both | How many did both |
| local, upstream | Median time for the local answer and for Claude's |

These compare the shape of two answers, not whether the local one was
right. A local answer that calls one tool where Claude called three in
parallel "differs" and may still be fine; one that matches may still have
passed the wrong arguments. Open the captures to look closer: each shadow
is saved beside the real capture as `.shadow.md`.

Things to know:

- It costs nothing upstream, and it cannot slow or change a response: a
  shadow starts only after the real exchange has finished.
- Each turn is shadowed on its own, given the history Claude actually
  produced. It tests "would the local model have made this move", not
  "would it have finished the task".
- The local model answers one request at a time. Shadows queue, sixteen
  deep; beyond that they are dropped and counted as skipped. If a rule is
  also sending live requests local, shadows compete with them.
- Shadow runs are in none of the other figures `janus stats` reports.

### Replay: the same test on traffic you already have

Shadow mode judges the local model on requests as they happen. `janus
replay` does it on requests you have already captured, which is quicker
when you want to try a different model or a different context size on the
same traffic.

```bash
janus replay logs/captures                    # everything captured
janus replay -class subagent logs/captures    # one class
janus replay -limit 20 logs/captures/2026-10-11
janus replay -local janus-small logs/captures # a particular entry of local.models
```

```
20261011T160134.602-bf240e  subagent  9.9s  same: local tool_use (Bash); Claude tool_use (Bash)
20261011T160138.677-b0b93b  subagent  6.7s  differs: local tool_use (Bash); Claude tool_use (Bash, Read, Read)

Summary
       class  replayed  skipped  failed  same  differs  not compared  local time
    subagent         4        0       0     2        2             0        9.9s
```

Each captured Messages request is run on the local model and set against
the answer Claude gave at the time. The local answer is saved beside the
capture as `.replay.md` and `.replay.response.txt`; a later replay
overwrites them.

- **Nothing is sent upstream.** Replay talks to LM Studio only. Your
  captures stay on your machines.
- It needs captures, so `logging.capture` must have been on. It does not
  need `features.route`.
- It writes nothing to the ledger.
- "same" has the meaning it has in shadow mode: same stop reason and same
  tools called. It is about shape, not correctness.
- The request class is read from the request body, as `janus route` does.
- Ctrl+C stops it and still prints the summary so far.

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
