# Configuration

This is the reference: every setting, what it does, and why it works the
way it does. To set Janus up for the first time, [configuring
Janus](guide/configure.md) is shorter and has whole configs to start
from.

Janus reads `janus.config.json` from the directory it is run in, or the
file named by `-config`. If the file does not exist, Janus writes one with
the defaults below and carries on.

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

| Field | Meaning |
| --- | --- |
| `version` | The config's format. Leave it as Janus wrote it; see [older and newer configs](#older-and-newer-configs). |
| `listen` | The address Janus accepts connections on. Loopback unless `allowRemote` is true. |
| `allowRemote` | Allow a `listen` address other than loopback. Leave it off; see [addresses](#addresses). |
| `upstream.baseUrl` | The Anthropic API, or a gateway in front of it. |
| `upstream.off` | `true` when there is no Anthropic account behind Janus: nothing is sent to Anthropic. See [no Anthropic account](#no-anthropic-account). |
| `local.baseUrl` | The LM Studio server. Change the default to yours. |
| `local.models` | Optional. Model names answered locally; see [local models](#local-models). |
| `openai` | Optional. The OpenAI API, its key's source, and its models; see [OpenAI](#openai). Left out, OpenAI is not used. |
| `routing` | Optional. Rules that decide which requests go local; see [routing rules](#routing-rules). |
| `features.route` | Switch on aliases and routing rules. Off by default. |
| `features.guard` | Look for secrets in requests going upstream, and record them. See [the guard](GUARD.md). |
| `guard` | Optional. Redaction, your own patterns, the allow list, the entropy search, `localOnly`, and `keepFrom`, content for one provider and not another. See [the guard](GUARD.md). |
| `features.compress` | Digest large tool results before they go upstream. See [compression](#compression). |
| `compress` | Optional. Settings for compression. |
| `features.escalate` | Reserved. Janus refuses to start if it is true: local-first is a rule, not a switch. |
| `logging.dir` | Where the ledger and captures go: a folder of your own by default. See [logs](#logs). |
| `logging.capture` | Save every request and response in full. On by default. Captures hold your prompts and code. |
| `logging.retainDays` | Delete captures older than this. `0` keeps them forever. |
| `logging.maxMegabytes` | Delete the oldest captures once they pass this size. `0` means no limit. |
| `pricing` | Optional. Prices to add or correct; see [pricing](#pricing). |
| `update.check` | Look for a newer release when Janus starts. Off by default; see [release checks](#release-checks). |
| `update.prerelease` | Count pre-releases, such as `v1.0.0-rc.2`, as newer releases. Off by default. |
| `console.contextWarnTokens` | When to say a conversation has grown expensive. Default 400,000; see [the context warning](#the-context-warning). |

A field left out of the file keeps its default. A field Janus does not
recognise is an error, so typos are caught.

## Older and newer configs

A config without a `version` is version 1. One written for an older Janus
works as it is: Janus reads it in its own form each time, says what that
changed (on the console and in `janus doctor`), and leaves the file alone
until you run `janus config upgrade`, which rewrites it and keeps the old
one as `.bak`. People skip versions, so an old config is never a reason
for Janus not to start.

A config written for a newer Janus, or with a setting this Janus does not
know, is refused with a message saying so, since guessing what a setting
means is worse than stopping.

## Addresses

`listen` must be a loopback address unless `allowRemote` is true. Janus
forwards whatever credential a client sends, so anything that can reach
its port can use your login through it. While `allowRemote` is off, Janus
also refuses requests from web pages: any whose `Host` is not this
machine or whose `Origin` is a page from elsewhere. The
[threat model](THREAT-MODEL.md#the-listening-port) says why.

Janus refuses to start if `upstream.baseUrl`, `local.baseUrl`, or
`openai.baseUrl` is its own `listen` address, where it would send each
request back to itself without end.

## OpenAI

An `openai` section lets an OpenAI model answer Claude Code's requests in
Claude's place. This release reads and checks the section; nothing is
sent to OpenAI until the provider lists that choose it arrive later in
v0.10.0.

```json
"openai": {
  "key": "env:OPENAI_API_KEY",
  "models": { "sonnet": "gpt-6-luna" }
}
```

| Field | Meaning |
| --- | --- |
| `openai.key` | Where the API key is kept: `env:NAME`, the environment variable `NAME`, or `keychain:NAME`, the entry `NAME` in your operating system's credential store. Required. |
| `openai.baseUrl` | The OpenAI API, or a gateway in front of it, without `/v1`. Default `https://api.openai.com`. |
| `openai.models` | Optional. Which OpenAI model answers when Claude Code asks for a Claude model: a model's full name, a family (`fable`, `opus`, `sonnet`, `haiku`), or `default` for any other, mapped to an OpenAI model id. A full name is looked up first, then the family, then `default`. Your entries replace the defaults one by one. |
| `openai.contextTokens` | Optional. A model's context window, by its OpenAI name, for one Janus does not know or whose window has changed: `{"gpt-5.3-codex": 400000}`. Janus knows the windows in [the research note](research/openai.md#models). |

The default models are `gpt-6.1-sol` for `fable`, `opus`, `sonnet`, and
`default`, and the smaller `gpt-6-luna` for `haiku`
([the mapping](research/openai.md#models)).

The reasoning effort follows Claude Code's: its `effort` setting goes
across as it is, and a thinking budget picks one (under 4,000 tokens
`low`, under 16,000 `medium`, under 32,000 `high`, otherwise `xhigh`).
Thinking switched off becomes the model's lowest effort, and so does any
request that caps its output below 4,000 tokens, such as auto mode's safety
checks and status lines: OpenAI counts reasoning against the cap, so a
model reasoning at its default can spend all of a small one and answer
nothing. (4,000 is a starting point, not a measurement.) An effort a model
does not take is moved to the nearest one it does: `max` on
`gpt-5.3-codex` becomes `xhigh`. The ledger records the Claude model asked
for as `request.model` and the OpenAI model that answered as
`response.model`.

**The key is never in this file.** A config file gets copied between
machines, pasted into issues, and backed up, and `janus config upgrade`
leaves a `.bak` copy beside it, so Janus refuses a key written here, and
its error does not repeat it. It reads the key once, at startup, from the
source `openai.key` names, keeps it in memory only, and sends it to OpenAI
and nowhere else ([ADR-005](adr/005-openai-credential.md)).

**Use `keychain:` when Janus runs as a service** (`janus service
install`), which does not see your shell's environment. `janus service
install` never copies a key into the service's unit. Store the key once,
with your platform's own tool, then set `"key": "keychain:janus-openai"`:

| Platform | Store it | Janus reads it with |
| --- | --- | --- |
| macOS | `security add-generic-password -a "$USER" -s janus-openai -w` (it asks for the key) | `security find-generic-password -s janus-openai -w` |
| Linux | `secret-tool store --label="janus OpenAI key" service janus-openai` (it asks for the key) | `secret-tool lookup service janus-openai`, from libsecret |
| Windows | `cmdkey /generic:janus-openai /user:janus /pass` (it asks for the key) | `CredReadW`, the Credential Manager's own call |

Janus runs the tool with a fixed list of arguments, never through a
shell. If the entry is missing, the error says the command that adds it.
On macOS, the first read may ask whether Janus may use the entry; choose
"Always Allow" so a service can read it unattended.

`janus doctor` says where the key comes from and whether it was found,
never what it is. It asks OpenAI only whether it answers, without the
key, so a wrong key shows up only when OpenAI refuses it.

## Logs

By default the ledger and captures go to a folder per user, the same
whichever folder Janus starts in: macOS `~/Library/Application
Support/janus`, Linux `$XDG_STATE_HOME/janus` (or `~/.local/state/janus`),
Windows `%LocalAppData%\janus`. A config written by Janus names it in
full.

A relative `logging.dir` is relative to the folder holding the config
file, so every `janus` command finds the same logs wherever it is run.
Only one Janus runs per log folder; a second stops and names the first,
so that two never write the same ledger. See
[the ledger and captures](LEDGER.md).

## Release checks

With `update.check` on, Janus looks for a newer release when it starts
and, if there is one, says so in a line on the console. It asks GitHub at
most once a day, keeping the answer in `update-check.json` in the log
folder, and in the background, so startup and requests never wait for
it. A check that fails, offline or rate-limited, prints nothing and is
not tried again until the next day. A development build has nothing to
compare, so it asks nothing.

It is off by default because a proxy that sees your prompts should not
make connections you did not ask for: with it off, Janus looks for
releases only when `janus version -check` or `janus doctor` asks.
`update.prerelease`, or `-pre` for one check, counts pre-releases too;
otherwise only stable releases are offered.

## The context warning

Each turn reads the whole conversation from the prompt cache, so a turn
in a conversation of 600k tokens costs about four times one under 200k
([the baseline](research/baseline.md)). Once a conversation passes
`console.contextWarnTokens`, the console says so, once, with what a turn
costs now and what it cost when the conversation was small. A `/compact`
or a new session starts it small again. The default is 400,000; a
negative number turns the note off.

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
    "baseUrl": "http://127.0.0.1:1234",
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
| `contextTokens` | Largest prompt to send, and the context Janus has LM Studio load the model with when a request finds it unloaded. `0`, the default, uses the context the model is loaded with, and loads it with 65,536 tokens (or its maximum, if smaller). A model already loaded is never reloaded: a request too big for its context is refused, or goes to Claude. |
| `timeoutSeconds` | Longest one request may take. `0` means 900. |
| `maxConcurrent` | How many requests routing rules may have in flight to this model at once. `0` means 1. See below. |

**Why `maxConcurrent` defaults to 1.** A local model runs requests side by
side in your machine's memory: on a 36 GB Mac, two parallel subagents
took a 27B model past 31 GB within four minutes. Past the limit, a rule
sends the request on, and the ledger says why. A local model last on a
rule's `fallback` list, which has nowhere else to send it, waits for a
place instead, for up to `timeoutSeconds`. Model aliases and the local-only policy are not
limited, since they have nowhere else to send a request either. Raise it
only with memory to spare.

Janus has no tokenizer, so it sizes a request at three bytes per token,
which over-counts. A request over the limit is refused with an error; it is
not sent upstream, because an alias has no upstream model.

What changes on the way to LM Studio: the model name, `tool_addition`
blocks (lifted into `tools`), tools marked `defer_loading` (dropped),
system messages mid-conversation (folded into reminders), and a tool
search's `tool_reference` results (turned into text).
Your Anthropic credentials are removed from the request. On the way back,
the model name is changed to the one the client asked for.

## Routing rules

`routing` is a list of rules, tried in order. The first rule a request
matches decides where it goes; a request that matches none goes upstream
untouched. Rules only apply to Messages requests, and only with
`features.route` on. A route header or a model alias, if a request has
one, decides before any rule:

![How Janus chooses where a request goes: a route header first, then a model alias, then the rules in order, the first match choosing upstream, shadow, local-then-escalate, or local; a request nothing matches goes to Claude untouched](diagrams/route-decision.svg#gh-light-mode-only)
![How Janus chooses where a request goes: a route header first, then a model alias, then the rules in order, the first match choosing upstream, shadow, local-then-escalate, or local; a request nothing matches goes to Claude untouched](diagrams/route-decision-dark.svg#gh-dark-mode-only)

First match wins, so a list of rules reads top to bottom: put narrow rules
above broad ones.

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
| `class` | The request is `main`, `subagent`, `status`, `safety`, or `unknown`; see the classes below |
| `model` | The client asked for exactly this model |
| `maxTokens` | The request is estimated at no more than this many tokens |
| `tools` | `true`: the request offers tools. `false`: it offers none. |

Outcome.

| Field | Meaning |
| --- | --- |
| `route` | `upstream`, `local`, `shadow`, or `local-then-escalate`. The last two are described below. Left out when `fallback` is the route. |
| `local` | Which entry of `local.models` to use, for `local`, `shadow`, and `local-then-escalate`. |
| `name` | Shown in the ledger and in `janus route`. Optional. |
| `skipChecks` | For `local-then-escalate`: checks to leave out. See below. |
| `maxSeconds` | For `local-then-escalate`: how long the local model gets before Claude is asked. Default 120. |
| `fallback` | Where the request may go, in order: `upstream`, `openai`, or entries of `local.models`. With `route` left out the list is the route; with `upstream` or `shadow` it starts with `upstream`, and the rest answer when Claude cannot. See below. |
| `strict` | With a `fallback` list naming `openai`: pass over OpenAI for a request that asks for something it would drop (`stop_sequences`, `temperature`, `top_p`, `top_k`), saying what. See [a list of providers](#a-list-of-providers). |
| `maxUsd` | With a `fallback` list: pass over a provider whose most the request could cost is above this many US dollars. See [a list of providers](#a-list-of-providers). |
| `ifUsageAbove` | Match only once a subscription usage limit is used past a share, as Claude last reported: `{"5h": 0.8}` for the five-hour limit, `"7d"` for the weekly one, `"openai.gpt-6.1-sol.tokens"` or `"openai.gpt-6.1-sol.requests"` for one OpenAI model's per-minute limits (any model's name), `"openai.tokens"` and `"openai.requests"` for the fullest of any model's; any one past its share is enough. Until the provider has answered, and after a limit's reset, it does not match. Not for `upstream`. See [the pacing recipe](recipes/pace.md). |

The classes:

- **`main`** is Claude Code's main conversation, and **`subagent`** a
  helper it started.
- **`status`** is Claude Code asking for a few words on what a subagent is
  doing, for the line it shows while it runs. It carries the whole
  conversation for a five-word answer, so leave it upstream.
- **`safety`** is auto mode's classifier grading an action before it
  runs. It decides whether the agent may act, so **only a rule that names
  `class: "safety"` matches it**: rules without `class`, and `fallback`
  lists on other rules, leave it with Claude.
- Claude Code's start-up quota check and its compaction requests are not
  `main`: only a rule without `class` matches them.

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

### A list of providers

`fallback` names where a request may go, in order, and Janus sends it to
the first that can take it. When that one cannot answer, the request goes
to the next, and so on down the list:

```json
{ "name": "main", "class": "main", "fallback": ["upstream", "openai", "janus-local"] }
```

- **`upstream`** is Claude, through `upstream.baseUrl`.
- **`openai`** is the OpenAI API, through the `openai` section, whose key
  Janus reads when it starts: a rule that names `openai` without a key
  set stops Janus from starting, saying which variable to set. What
  OpenAI is sent, and how its answers come back, is in
  [the OpenAI research note](research/openai.md).
- **An entry of `local.models`** is that local model.

A list of one is one provider and nothing else: `["openai"]` sends the
rule's requests to OpenAI only, and `["janus-local"]` to the local model
only, refusing what it cannot take. Two entries can't name the same
provider.

**Two checks on a rule's list**, borrowed from OpenRouter's routing:

- **`"strict": true`** passes over OpenAI for a request that asks for
  something OpenAI would drop. Without it, Janus sends the request
  anyway, drops those settings, and names them in the ledger's
  `request.dropped`. With it, the next provider gets the request instead,
  and `tried` says which settings.
- **`"maxUsd": 0.50`** passes over a provider whose most the request
  could cost is above that. The figure is a **ceiling at list price, not a
  forecast**: the whole prompt at the uncached rate, which ignores the
  cache reads most of a Claude Code turn is, plus an answer as long as
  `max_tokens` allows (up to 128,000 tokens), with the prompt's size
  estimated high at three bytes a token. Set it well above a typical turn
  of the model you mean to allow: Opus with `max_tokens` of 32,000 can
  come to about $0.64 in output alone. A local model is free, and a model
  Janus has no price for is not passed over. Prices are the ones
  [`pricing`](#pricing) holds.

An entry that cannot take the request at all is passed over, and the
ledger's `reason` says why: a request OpenAI's API has no way to express,
OpenAI out (below), a local model too busy or too small for it, LM Studio
unreachable. If no entry can take it, the request is refused with an
error naming each reason, and nothing is sent.

When the provider in use fails, whether the request moves down the list
depends on the provider and the failure:

| Provider | Moves on at once | Moves on once it is an outage | Never moves on |
| --- | --- | --- | --- |
| Claude | A rate limit whose headers say a minute or more | Any other 429, 5xx, or 529, or no answer, on the third within two minutes | Any other error, such as a bad request or an expired login |
| OpenAI | A conversation too long for the model; a request OpenAI refused (most likely janus translated it wrongly); a refused key; a used-up quota or spend limit | A per-minute rate limit (at once if `Retry-After` says a minute or more), a server error, overload, or no answer, on the third within two minutes | — |
| A local model | Any error, or no answer | | |

"Once it is an outage" is the rule described under
[Keep working when Claude is down](#keep-working-when-claude-is-down):
until then the failure goes to Claude Code, which retries, and usually
gets through. A refused OpenAI key and a used-up quota need you to act,
so OpenAI is then left out of every list until Janus restarts, rather
than asked again. Whatever moves on, the next provider is sent the
request as Claude Code sent it.

**Coming back.** When a provider's refusal says when its limit resets
(Claude's usage limit, or an OpenAI rate limit of a minute or more),
every session moves down its list until then without asking that
provider again, and goes back to it on the first turn after the reset.
OpenAI's limits vary by model, so its rate limits, resets and outage
backoffs are kept for the model a request would go to: a limit on the
model Haiku requests go to leaves the one Opus requests go to answering.
A refused key and a used-up quota are the account's, and take OpenAI out
whole. Janus keeps these reset times in `limits.json` under `logging.dir`, so a
restart doesn't send sessions back early. An outage has no reset time:
the provider is left alone for 30 seconds, then asked again, the wait
doubling, up to 10 minutes, while it keeps failing; its first answer
ends it. The ledger's `tried` says when a provider was passed over for
either, and until when.

**A conversation larger than the OpenAI model's window** has older turns
left out for that model, as for a local stand-in: Janus keeps the system
prompt, the tools, the first message, and the latest turns that fit,
leaving room for the answer, and tells the model how many were left out.
The cut stays where it is from turn to turn, so OpenAI's prompt cache
holds, and moves on, far enough to stay put again for a while, only when
the conversation no longer fits. If even the first and latest turns
don't fit, the next provider on the list gets the request, and the
ledger's `reason` says why. Claude's copy is never cut. The windows of
the default models are a million tokens, so this matters mainly for
`gpt-5.3-codex`, at 400,000.

### What each feature does on the OpenAI path

| Feature | With OpenAI answering |
| --- | --- |
| Compression and digests | Applied before the request is translated, through the same record of what each conversation has sent, so a block goes in the same rewritten bytes whichever provider has the turn. The net-savings guard prices it at Claude's rates. |
| `local-then-escalate` and `local` | Give the rule a `fallback` list and it escalates along it, in order, instead of always to Claude: `"fallback": ["openai", "upstream"]`. Without one, it escalates to Claude as before. |
| The answer checks | Judge local answers only. An answer from OpenAI, like Claude's, is never judged; the checks read a translated answer as they read any other, should one be given them. |
| Shadow runs and `janus replay` | Claude and the local model only, in v0.10.0. A shadow rule sends its request to Claude; replay sends captures to the local model. |
| The MCP worker tools (`janus mcp`) | Unchanged. Their guidance is in the tools' own descriptions, which a Codex model reads as Claude does. |
| Auto mode's safety checks | Follow the `safety` rule's list, or stay with Claude. When one fails on whichever provider it was on, while another model is carrying the conversation, the console says how to keep working, as it does for Claude. |
| The guard | Covers OpenAI as it covers Claude; see [the guard](GUARD.md). |

### No Anthropic account

With `"upstream": {"off": true}`, Janus never contacts Anthropic, and
Claude Code runs on OpenAI, a local model, or both:

```json
"upstream": { "off": true },
"openai": { "key": "env:OPENAI_API_KEY" },
"routing": [ { "name": "everything", "fallback": ["openai", "janus-local"] } ],
"features": { "route": true }
```

- **Every rule is a `fallback` list without `upstream`**, since every
  other route falls back to Claude. Janus refuses a config with any other
  kind of rule.
- **A request no rule matches is refused** with a `janus:` error, so end
  the rules with one that has no conditions.
- **Auto mode's safety checks** are matched by a rule with no `class`,
  since there is no Claude to leave them with. A rule naming
  `class: "safety"` still decides them first.
- **Janus answers the rest of what Claude Code sends itself**, with
  nothing leaving the machine. Its connectivity checks (`GET /` and
  `/api/hello`) get an empty answer. Its start-up quota check gets an
  answer of no tokens, since there are no Anthropic limits to report.
  `count_tokens` gets Janus's own estimate, at three bytes a token, which
  over-counts. Anything else is refused with a `404`. The ledger records
  these with `backend` `janus`, and the token count's `reason` says it was
  estimated.
- **Claude Code needs a login to start.** `janus claude` gives it a
  placeholder, `ANTHROPIC_AUTH_TOKEN=janus-no-anthropic-account`, which
  Janus never sends anywhere. Run some other way, set the same variable.
  If Janus is not running, `janus claude` does not start Claude Code,
  since there is no Anthropic account to send it to.

Before config version 2 a rule named one local stand-in with
`"ifDown": "janus-local"`. Janus reads that as
`"fallback": ["upstream", "janus-local"]`, which means the same thing,
and `janus config upgrade` rewrites it so.

### Keep working when Claude is down

A rule that sends requests upstream can list what answers when Claude
cannot:

```json
{ "name": "main", "class": "main", "route": "upstream", "fallback": ["upstream", "janus-local"] }
```

Janus moves off Claude when it is down for a while, not on every
failure:

![When the local model stands in for Claude: an error about the request itself is passed on; a 429 lasting a minute or more brings the local model in at once; anything else goes to Claude Code to retry, and the third failure within two minutes brings the local model in](diagrams/stand-in.svg#gh-light-mode-only)
![When the local model stands in for Claude: an error about the request itself is passed on; a 429 lasting a minute or more brings the local model in at once; anything else goes to Claude Code to retry, and the third failure within two minutes brings the local model in](diagrams/stand-in-dark.svg#gh-dark-mode-only)

- **A usage limit, at once.** On a subscription, running out of usage is a
  rate limit (429) whose headers say it resets in an hour or more. When a
  429 says Claude won't answer for a minute or more, the local model
  answers straight away.
- **An outage, on the third failure.** Anything else that means Claude
  can't answer (a short rate limit, a server error such as "overloaded",
  no connection) is first passed to Claude Code, which retries with
  backoff and usually gets through a few seconds later. If Claude fails
  three times within two minutes without answering in between, it is
  down, and the local model answers. The next answer from Claude resets
  the count.

That way a passing blip doesn't put a local model's turn into the
conversation, where it would stay ([the stand-in study](research/standin.md)).

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
- **A conversation too long for the local model is cut to fit.** Its
  copy keeps the system prompt, the tools, the first message, and the
  latest turns that fit in three quarters of the model's context (the
  rest is room to answer), with a note saying how many messages were left
  out. The model works without the middle of the conversation, so it may
  repeat work or miss a decision made there. Claude's copy is never cut.
- **If nothing further down the list can help either** (LM Studio down,
  or even the latest turn too large), the client gets the last error
  exactly as it was sent.
- **It costs nothing while Claude is healthy.** The fallback is only worked
  out when it is needed; LM Studio is not contacted otherwise.
- The conversation goes back to Claude on the first turn after its limit
  resets, or after an outage once Claude answers again (see
  [coming back](#a-list-of-providers)).

#### Auto mode while Claude is out

In auto mode, Claude Code asks Claude whether each tool call is safe
before it runs it. Those checks are their own class, `safety`, and stay
with Claude: a `fallback` list on a `main` rule does not cover them. So
while the local model stands in for the conversation, Claude Code refuses
every tool call as "temporarily unavailable". The console says so, once per
conversation. Press Shift+Tab to leave auto mode; Claude Code then asks
you before each tool call, and the session goes on.

To have the local model grade the checks instead, give them a rule of
their own:

```json
{ "name": "safety", "class": "safety", "route": "upstream", "fallback": ["upstream", "janus-local"] }
```

That puts a security decision in the hands of a weaker model, so it is
not the default. It also helps less than it seems: each check carries the
conversation's transcript, so late in a long session it is too large for
a local model and Claude's error stands. A ~32k-token check took qwen3.8-27b
46 s, and its verdict was sound ([the stand-in study](research/standin.md)).

With this on the main-conversation rule and a `local` rule for subagents, a
whole Claude Code session runs with no connection to Anthropic at all. That
was tested; it is as good as the local model is, and much slower.

### Choosing the route yourself

A request can say where it wants to go with a header, which beats every
rule:

| `X-Janus-Route` | Effect |
| --- | --- |
| `upstream` | Send it to Claude, whatever the rules say |
| `openai` | Send it to OpenAI first; if OpenAI cannot answer, it goes on down the `fallback` list of the rule that would have matched |
| `local` | Send it to the local model, if exactly one is configured |
| `local=<name>` | Send it to that entry of `local.models` |

Claude Code sends extra headers from an environment variable, so this
works for a whole session. The launchers set it for you:

```bash
janus claude -provider claude   # X-Janus-Route: upstream; bypass local routing for this session
janus claude -provider openai   # X-Janus-Route: openai; "codex" means the same
janus local                     # X-Janus-Route: local; keep this session off Anthropic
```

Each session on one Janus can choose differently. Started another way,
set `ANTHROPIC_CUSTOM_HEADERS="X-Janus-Route: local"` and the like
yourself.

**`janus use` moves a running session without restarting it.** It asks
the running Janus, on its own listen address, and takes effect on the
next turn:

```bash
janus use codex                 # every session to OpenAI first, from the next turn
janus use local -session <id>   # one session, by the id the ledger records as "session"
janus use -list                 # the choices in force
janus use -clear                # back to the rules
```

A choice works as the header does, with two differences: a session's
own header beats it, and it ends by itself when its provider cannot
answer (OpenAI out, the local model unable to take the request), so the
rules decide again rather than the request being refused. The ledger's
`reason` starts `janus use` for every request it decided, and the console
says so once per conversation. Janus answers it only for programs on this
machine, never for a web page; choices are kept in memory, and a restart
clears them.

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

## Compression

Off by default, and best left off for now: on a day of ordinary use it
would have saved about 1.5% and added minutes of waiting. See
[the evaluation](research/digest.md).

Why so little, when tool output can be huge? Because most of what a turn
costs is reading the conversation so far from the prompt cache, and that
part cannot be made smaller without writing it all to the cache again
([why](WHAT-JANUS-CHANGES.md#why-the-list-is-short)). A digest can only
shrink a result the first time it is sent. On the day measured, cache
reads were more than half the cost, and large tool results were a small
part of what was read.

With `features.compress` on, a tool result going upstream that is larger
than a threshold is replaced by a digest the local model writes: errors,
paths, identifiers, numbers, and the outcome kept, repetition and progress
noise dropped. The original is kept in `<logging.dir>/originals`, and the
digest tells Claude how to get it back with the `tool_output` tool of
`janus mcp`.

![How compression keeps the original: a large tool result is digested by the local model, the digest goes to Anthropic in the same bytes every turn, and Janus keeps the original for Claude to fetch with tool_output](diagrams/compress.svg#gh-light-mode-only)
![How compression keeps the original: a large tool result is digested by the local model, the digest goes to Anthropic in the same bytes every turn, and Janus keeps the original for Claude to fetch with tool_output](diagrams/compress-dark.svg#gh-dark-mode-only)

```json
{
  "features": { "compress": true },
  "local": { "models": { "janus-local": { "model": "qwen/qwen3.8-27b" } } },
  "compress": {
    "digest": { "local": "janus-local", "minBytes": 4000, "tools": ["Bash"], "maxSeconds": 30 }
  }
}
```

| Field | Meaning |
| --- | --- |
| `compress.digest.local` | The `local.models` entry that writes digests. May be left out when there is only one. |
| `compress.digest.minBytes` | The smallest tool result digested. Default 4,000 bytes, about 1,300 tokens. |
| `compress.digest.tools` | Digest only the results of these tools, such as `["Bash"]`. Default: any tool except Read, whose results are files Claude usually means to edit; name `Read` here to digest those too. |
| `compress.digest.maxSeconds` | How long the local model gets per digest. The turn waits meanwhile; past it the result goes as it is. Default 30. |

`janus doctor` checks that LM Studio has the digest model, and whether it
is loaded.

What it will and will not do:

- **Only a result Claude can get back is digested.** The request must offer
  a tool named `mcp__<server>__tool_output`, which it does once `janus mcp`
  is added to Claude Code, as in [the workers recipe](recipes/workers.md).
  Without it, results go as they are and the console says why.
- **Only new results.** A result is digested the first time it is sent, and
  sent in the same form on every later turn, so the prompt cache holds. See
  [the ledger](LEDGER.md#transforms) for how the savings are counted.
- **Not errors, images, or protected content.** A result marked as an error
  is what the next turn is about. Content the local-only policy covers is
  never digested, because a paraphrase could slip past the policy.
- **Not a file Claude read.** A Read result, or a Bash command that prints
  a file (`cat`, `sed -n`, `head`, `tail`, `nl`, `less`, `more`, or `bat`
  with a path), is usually a file Claude means to edit, and it needs the
  exact text, so the output goes as it is. Name `Read` in
  `compress.digest.tools` to digest Read results anyway. The same
  programs trimming another command's output, as in `go test | head -40`,
  do not count.
- **Originals hold tool output in full.** They are readable by their owner
  only, kept whether or not `logging.capture` is on, and deleted once
  unused for `logging.retainDays`, checked at most hourly while Janus runs.
  They don't count towards `logging.maxMegabytes`.
- **It costs time.** Each digest holds its turn up while the local model
  writes it; `maxSeconds` bounds that.

Without `features.route`, compression still works: every request goes
upstream, and only the digest applies.

## Pricing

`janus stats` ships with Anthropic's and OpenAI's list prices. To correct one, or to
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
