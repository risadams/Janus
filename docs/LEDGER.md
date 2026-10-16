# The ledger and captures

Janus writes two kinds of record under `logging.dir` (default `logs/`,
next to the config file).

| | Ledger | Captures |
| --- | --- | --- |
| Where | `<logging.dir>/ledger/<day>.jsonl` | `<logging.dir>/captures/<day>/<id>_<path>.*` |
| Holds | One line per request: what it was, what Janus did, what it cost | The full request and response |
| Contains prompts or code | No | Yes |
| Contains credentials | No | No (headers are redacted in the render and not stored elsewhere) |
| Can be switched off | No | Yes, `logging.capture` |
| Pruned | Never | By age and total size |

Days are UTC. Every file is created readable by its owner only (0600, in
0700 directories). Windows does not have those modes: there the files
get whatever access the directory they are in allows, so keep `logging.dir`
somewhere only you can read, such as under your user profile.

## The console

While Janus runs it prints a line for every request, for a person
watching; the ledger is the record. The date is printed when it changes,
and each line starts with the time:

```
── Tuesday 13 October 2026 ──
16:35:50  main       Opus 5.5                ✓ 20.6s   tool call   151k in · 98% cached    2.3k out   $0.092
16:35:51  status     Sonnet 5                ✓ 967ms   stopped     84k in · 97% cached     9 out      $0.022
16:36:03  subagent   qwen3.8-27b (local)     ✓ 14.2s   answered    1.8k in                 212 out    free
16:36:20  main       Opus 5.5                ✓ 31.0s   answered    151k in · 0% cached     410 out    $1.22
16:59:29  main       qwen3.8-27b (local)     ✓ 3.2s    answered    22 in                   37 out     free
          ↳ Claude could not answer (upstream answered HTTP 429), so the local model stood in
```

| Column | Says |
| --- | --- |
| Kind | `main` for Claude Code's conversation, `subagent`, and its background requests: `status` (the one-line summary of what it is doing), `safety` (auto mode's safety classifier grading an action), `quota`, `compact`, `count` (token counts), `ping` (connectivity checks), `other` |
| Answered by | The model, `(local)` if LM Studio answered. Requests that are not Messages show their method and path instead |
| Outcome | `✓` and how long it took, or `✗` with the status and what the error means, or `✗ refused` |
| Stop | Why the model stopped: `answered`, `tool call`, `stopped` (a stop sequence), `max tokens`, `refused`, or `cut off` if the response ended early |
| In | The whole prompt, and the share read from the prompt cache |
| Out | Tokens generated |
| Cost | The list-price estimate, worked out as `janus stats` does; `free` for local answers |

Anything unusual goes on a `↳` line beneath: a local model standing in
for Claude or a local answer sent on to Claude, and why; a refusal; a
fault in Janus or the connection; what the guard or the local-only policy
did; a transform's decision.

A secret or a local-only file in the conversation is in every request
after it, and Janus deals with it every time; the ledger records each.
The console says so once per conversation, and again only when something
changes: another secret, another file, or a secret put back in an answer.
A secret that went to Claude because redaction is off is noted every
time. A conversation idle for an hour is forgotten, and told afresh.

In a terminal, color marks what matters: the main conversation in bold,
subagents in cyan and Claude Code's background requests dimmed; local
answers in magenta; failures in red. The cache share is green on a large
prompt that came from the cache, and yellow on one that did not, which is
the cache being written again at many times the price. A turn over half a
dollar is yellow for the same reason. Color is off when the output is not
a terminal, or with `NO_COLOR` set; `FORCE_COLOR` turns it on regardless.

The other commands follow the same rules, decided for standard output and
standard error separately, and use the same colors: `doctor` shows ok,
warn, and fail in green, yellow, and red; `stats` shows a usage limit in
yellow from 75% used and red from 90%, failures in red and local answers
in magenta; `audit` shows secrets sent upstream in red; `route` and
`replay` color where a request went and how a replay compared; `service
status` says running in green and not running in yellow. Output for
scripts, such as `-json`, is never colored.

## Ledger record, schema version 1

One JSON object per line. `v` goes up when a field changes meaning or is
removed; new fields may appear without a version change, so readers should
ignore fields they do not know.

| Field | Type | Meaning |
| --- | --- | --- |
| `v` | int | Schema version, `1` |
| `id` | string | Unique per request and sortable by time: `20261011T150013.033-02e426` |
| `time` | string | When the request arrived, RFC 3339, UTC |
| `session` | string | Claude Code's session id, from `X-Claude-Code-Session-Id`. Omitted if absent. |
| `agent` | string | Claude Code's subagent id, from `X-Claude-Code-Agent-Id`. Omitted for the main loop. |
| `method`, `path` | string | The request line |
| `class` | string | `main`, `subagent`, `connectivity`, `quota` (Claude Code reading its rate limits at start-up with a one-token request; a `429` answer to it is normal), `compaction` (Claude Code asking for a summary of the conversation to replace it, as `/compact` does), `status` (Claude Code asking for a few words on what a subagent is doing, for its progress line), `safety` (Claude Code's auto-mode safety classifier grading an action before it runs; recognised by its system prompt), `token_count` (`POST /v1/messages/count_tokens`), or `unknown` |
| `route` | string | Where the request was sent: `upstream`, `local`, or `shadow` for a shadow run |
| `reason` | string | What decided it: `passthrough` (nothing matched), `model alias <name>`, `rule <name>`, or `override <value>` for a request that chose its own route. When a rule's local route could not be used, the reason says why and ends `; sent upstream`. When Claude could not answer and a rule's `ifDown` stand-in did not answer either, it says what Claude did and why the local model did not stand in, e.g. `rule main: upstream answered HTTP 429; no local stand-in: this request is about 281000 tokens, …`. For a refused request, why. |
| `stages` | string[] | Changes made to the request on the way: `set_model`, `lift_tool_additions`, `drop_deferred_tools`, `fold_system_messages`, `tool_references_as_text`, `leave_out_messages`, `load_model` for a local request; `drop_unsigned_thinking` for an upstream one whose history a local model had a turn in |
| `backend` | string | Which backend answered: `upstream`, `local`, or `none` if Janus refused the request itself |
| `status` | int | HTTP status, or `0` if no response was started |
| `outcome` | string | See below |
| `error` | string | The API's error type, when `outcome` is `api_error` |
| `limits` | object | For a `429` or a `5xx`: the response's `retry-after`, `x-should-retry`, and `anthropic-ratelimit-*` headers, by lower-case name. What the API said about when it would answer again. |
| `ttfb_ms` | int | Time until the first byte of the response body |
| `duration_ms` | int | Time until the response ended |
| `request` | object | Messages requests only: `model`, `stream`, `bytes`, `messages`, `tools`, `cache_breakpoints` |
| `response` | object | Messages responses only: `model`, `stop_reason`, `blocks` (content block types in order), `tools` (names of the tools called, in order) |
| `usage` | object | Messages responses only; see below |
| `unread` | string | Why a Messages response has no `usage`, e.g. `content-encoding br` |
| `quota` | object | On a response from Claude through a subscription: how much of each usage limit is used. `windows` holds each limit by the API's name for its window (`5h`, `7d`), with `utilization` (the share used, from 0; above 1 is over), `status`, and `resets` (when it starts again); `status` is the account's overall status (`allowed`, `allowed_warning`, `rejected`), and `binding` the limit the API says counts now (`five_hour`, `seven_day`). Read from the `anthropic-ratelimit-unified-*` headers; nothing else in them is kept. When the local model stood in, it is what Claude's refusal said. Absent with an API key, and on local answers. |
| `handback` | object | On Claude's first answer to a conversation's main loop after the local model stood in for it: `local_turns`, how many main-loop turns the local model gave, and `since`, when the first of them was. `janus stats` counts them. |
| `fault` | string | What went wrong getting the response from the backend: it could not be reached or did not answer in time, with no fallback (Janus then answered with an error of its own), or its response broke off part-way, e.g. a dropped connection. Absent for a response that arrived whole, and for one the client abandoned. |
| `capture` | string | The capture's path under `captures/`, without extension |
| `attempt` | object | Present when one backend was tried and another may have answered: `backend`, `model`, `verdict`, `why`, `cause`, `check`, `duration_ms`, and for a rejected attempt its `usage`. See below. |
| `local_cause` | string | Present when a rule or a stand-in meant the request for the local model and it was never tried there: why, as one of the causes below. |
| `guard` | object | Present when secrets or local-only content were found in a request: `findings` (a count), `kinds` (count by kind), `where` (which parts of the request). When redacting, also `redacted` (distinct secrets replaced before sending; absent means the request went with its secrets in it), `restored` (placeholders put back in the answer), and `blocked` (why the exchange was stopped). Under a local-only policy, `local_only` (why the request held protected content, naming paths and markers) and `withheld` (pieces replaced by a notice in what was sent upstream; `local_only` without it means the request did not go upstream). It never holds a secret or protected content. See [the guard](GUARD.md). |
| `transforms` | array | Present when a transform rewrote the request on its way upstream, or declined to. One entry per transform: `name`; `blocks` carrying its output, of which `rewritten` were rewritten for the first time and `revised` were already sent and changed; `declined` (answers not used); `tokens` (about how many fewer tokens were sent); `saved_usd` (what those would have cost on this request); `cost_usd` (cache written again because of a revision); `expected_usd` (the net-savings guard's estimate, when it decided, of what this request's decisions will save over the rest of the conversation); and `unpriced` when the model had no price. See below. |
| `worker` | object | Present on a record of delegated work, where Claude called a tool that the local model carried out: `tool`, `files`, `read_bytes`, `model_calls`, `answer_bytes`, `avoided_tokens`. Such a record has `class` `worker` and `method` `TOOL`, is not a proxied request, and is left out of the request totals. |
| `shadow_of` | string | Present on a shadow run: the id of the real request it repeated on the local model. Shadow records have `route` `shadow` and an id ending `-shadow`, and are left out of every total. |
| `compare` | object | On a shadow run that came back whole: `upstream_stop`, `local_stop`, `upstream_tools`, `local_tools`, `stop_match`, `tools_match`, and `upstream_ms` (how long the real answer took) |

### `outcome`

| Value | Meaning |
| --- | --- |
| `ok` | The response completed |
| `api_error` | The API answered with an error object; see `error` |
| `http_error` | A 4xx or 5xx whose body Janus could not read as an API error |
| `incomplete` | The response ended before the protocol says it should |
| `cancelled` | The client disconnected first |
| `no_response` | Nothing came back from the backend |
| `skipped` | A shadow run that was not attempted; `reason` says why |
| `tool_error` | Delegated work that did not produce an answer; `reason` says why |

### `attempt`

| `verdict` | The local model's answer | Who answered the client |
| --- | --- | --- |
| `accepted` | Passed the checks | The local model. The record's `backend`, `response`, and `usage` are its own. |
| `rejected` | Came back whole and failed a check; `check` says which | Claude. The record is Claude's answer, and `attempt.usage` is what the local model spent first. |
| `failed` | None to judge: it could not be reached, ran out of time, broke off, or returned an error; `cause` says which | Claude |

`attempt.backend` is which backend was tried first. It is `local` for the
cases above. It is `upstream` when Claude could not answer and the local
model stood in: the verdict is then `failed`, the record is the local
model's answer, and `reason` ends `; sent local`. `attempt.limits` then
holds Claude's retry and rate-limit headers, as `limits` does above, and
with captures on, Claude's error body is saved as
`.attempt.response.txt`.

### Why the local model did not answer

`attempt.cause` (for a local attempt that was not accepted) and
`local_cause` (for a request passed over without trying) are one of:

| Cause | Meaning | Next step |
| --- | --- | --- |
| `unreachable` | No connection to LM Studio | Start its server; check `local.baseUrl` |
| `skipped` | Not tried: the local backend failed three times in a row and is left alone for 30 seconds | Fix what made it fail; it resumes by itself |
| `model_missing` | LM Studio does not have the model | Correct `model`; `janus doctor` lists the ids |
| `load_failed` | The model was not loaded, and loading it failed | Free memory, or lower `contextTokens` |
| `too_large` | The request does not fit the model's context, by Janus's estimate or by LM Studio's own error | Raise `contextTokens` |
| `busy` | The model already had `maxConcurrent` requests | Raise `maxConcurrent`, if memory allows |
| `slow` | It ran out of time: `maxSeconds` on a local-then-escalate rule, or `timeoutSeconds` | Raise them, or use a faster model |
| `server_error` | LM Studio answered with an error | LM Studio's log |
| `broke_off` | The answer stopped part way, not by a timeout | LM Studio's log |
| `rejected` | A whole answer that failed `check`: `empty`, `truncated`, `tools`, `schema`, `refusal`, `malformed`, or `oversize` | The model is not up to that work |
| `unsupported` | The request could not be adapted for the local model | Report it |

Records written before causes were recorded have none; `janus stats`
works them out from `why` and `reason`, which have always had a fixed
form. Such a record's `verdict` may say `rejected` for an error from LM
Studio; `janus stats` counts it as failed.

`attempt.duration_ms` is how long the local model was given before the
decision. For a rejected or failed attempt, that time was spent before
Claude was asked, and is included in the record's own `duration_ms`.

### `transforms`

A transform rewrites requests going upstream to send fewer tokens. A block
sent once is part of the cached prefix, so from then on it is sent in the
same form on every turn; the net-savings guard is what decides anything
else.

- **A block new to the cache** is rewritten only when that makes it smaller.
  It saves the tokens cut at the cache write rate now, then at the read
  rate on every later turn.
- **A block already sent** can be changed (`revised`) only when the guard
  expects it to pay: the cache is broken from that block on, and everything
  after it is written again. With cache reads at a tenth to a fortieth of a
  write, that takes many later turns, so most proposals are `declined`.
- **The guard estimates later turns** as half the turns so far. On the
  2026-10-12 ledger the full count was right on average but often far out
  for one conversation, and an overestimate costs more than an
  underestimate.
- **A transform whose revisions lose money stops revising.** A revision is
  settled when its conversation has gone an hour without a request. Once a
  transform has five settled revisions, if its last twenty (or all of them,
  if fewer) add up to a loss, it may no longer revise, and Janus says so.

`janus stats` totals these per transform, as below.

### `worker.avoided_tokens`

The input tokens Claude would have spent reading every one of the files in
full, less what the answer cost it. Files are counted at four bytes a token
and the answer at three, so the arithmetic errs low.

The assumption behind it does not. It supposes Claude would otherwise have
read every byte, and Claude often searches a file and reads a little of
it. In the one case measured, this figure was about 63,000 and the real
saving about 4,000. Read it as the most that could have been saved. The
real saving is the difference in cost between sessions with and without
the tool. See [the measurement](research/delegation.md).

### `usage`

Token counts, split the way they are priced.

| Field | Meaning |
| --- | --- |
| `input` | Uncached input tokens |
| `output` | Output tokens |
| `cache_read` | Input tokens read from the prompt cache |
| `cache_write` | Input tokens written to the prompt cache, in total |
| `cache_write_5m`, `cache_write_1h` | The same, split by cache lifetime |
| `thinking` | The part of `output` spent on thinking |
| `speed` | `fast` for a fast-mode request; left out otherwise |

Only `input` and `output` are always present. A field the server did not
report is left out, and that is not the same as zero: LM Studio, for
example, reports no cache writes at all.

Total input for a request is `input + cache_read + cache_write`.

LM Studio counts tokens read from its own prompt cache inside its input
figure. Janus takes them out, so `input` means uncached input whichever
backend answered. For a local answer, `request.model` is the name the
client asked for and `response.model` is the model that actually ran.

### Example

```json
{"v":1,"id":"20261011T150013.033-02e426","time":"2026-10-11T15:00:13.033Z","session":"…","agent":"…","method":"POST","path":"/v1/messages","class":"subagent","route":"upstream","reason":"passthrough","stages":[],"backend":"upstream","status":200,"outcome":"ok","ttfb_ms":654,"duration_ms":2101,"request":{"model":"claude-opus-5-5","stream":true,"bytes":47340,"messages":2,"tools":10,"cache_breakpoints":3},"response":{"model":"claude-opus-5-5","stop_reason":"tool_use","blocks":["tool_use"]},"usage":{"input":2,"output":203,"cache_read":0,"cache_write":17189,"cache_write_5m":17189,"cache_write_1h":0,"thinking":0},"capture":"2026-10-11/20261011T150013.033-02e426_v1-messages"}
```

## `janus stats`

Reads the ledger and reports where the tokens and the money went.

```bash
janus stats                      # everything
janus stats -since 24h           # an age: 90m, 24h, 7d
janus stats -since 2026-10-01    # a local date, or an RFC 3339 time
janus stats -since 7d -until 1d
janus stats -since 7d -daily     # with a row per day
janus stats -json                # the same report, for scripts
janus stats -watch               # today, redrawn every 5 seconds
janus stats -dir old/ledger -dir new/ledger   # several ledgers as one
```

```
By class
           class  requests  input  cache write  cache read  output  cache hit  est. cost
        subagent         4      8       19,693      54,938   1,118      73.6%    $0.1318
            main         2      4       11,309      33,772     455      74.9%    $0.1063
    connectivity         1      0            0           0       0          -    $0.0000
           total         7     12       31,002      88,710   1,573      74.1%    $0.2382
```

The same breakdown follows by model and by backend, then a count of
outcomes.

- **input**, **cache write**, and **cache read** together are everything the
  model was sent. **cache hit** is the share of that read from cache.
- **est. cost** is at Anthropic's first-party list prices. A subscription is
  not billed per token, so on one it is a measure of usage, not a bill.
- Rows are ordered by cost, most expensive first.
- A cost marked `*` leaves out requests that could not be priced, and the
  report lists how many and why. An unpriced request is never counted as
  free.

With `-daily`, a table follows with one row per local date: requests,
how many **failed** (an error, an answer that broke off, or none at all;
requests the client cancelled are not counted), cache hit, est. cost,
local-first attempts **tried** and **accepted**, what local answers
**saved** (below), and how many times the local model **stood in** for
Claude. Read it to see whether a change made one day worse than the ones
before. In `-json` it is `by_day`.

On a subscription, a section follows on its usage limits: each limit's
utilization and reset at the last record that had a `quota`, and each
kind of request's share of the five-hour limit used in the range. A
share is the rise in five-hour utilization reported after that kind's
requests; the API reports utilization in hundredths and for the whole
account, so over a few requests the shares are rough and over a day they
say which kinds use the limit. A fall is a reset and is not counted. In
`-json` it is `quota`: `at`, `latest`, and `by_class` (`class`, `added`,
`share`).

When the local model answered requests, a section says what they would
have cost had Claude answered them: each priced at the list price of the
Claude model it asked for (`request.model`), with the local model's token
counts. It counts answers the local model gave by choice, by a rule or an
accepted local-first attempt. Stand-ins are left out, since Claude could
not answer those. A request that asked for a model with no price, such as
the `janus-local` alias, is counted separately, never as free. The local
model counts tokens with its own tokenizer, and Claude would have cached a
different share of each prompt, so read the figure as the size of the
saving rather than an exact one. In `-json` it is `local_saving`:
`requests`, `usd`, and `unpriced`.

When Claude could not answer some requests, a section lists them as
outages: runs of such requests none more than 15 minutes after the one
before. Each gives when it began and ended, its most common cause
(`rate_limited` for a `429`, `overloaded`, `server_error` for another
`5xx`, `unreachable`), how many requests it held, how many the local model
**stood in** for (the rest reached the client as errors), and, for a rate
limit, the subscription limit reached and when Claude said it would answer
again, from `limits` or `attempt.limits`. Claude Code's start-up `quota`
check is left out, since a `429` is its normal answer. In `-json` it is
`outages`. After it, a line counts the times Claude took a conversation
back after the local model stood in (`handbacks` and `handed_back_turns`
in `-json`).

When the guard was on, sections follow for what it did: secrets found and
replaced, and content the local-only policy kept from Anthropic, by how
(withheld with a notice, answered locally, or refused) and by its most
frequent reasons. Both count a conversation's content again on every turn,
since it is sent again on every turn.

When transforms ran, a table follows of what each did: requests touched,
blocks rewritten, revised, and declined, tokens cut, what was **saved** (the
tokens cut, priced at what they would have cost on those requests, less the
cache revisions broke), and what the guard **expected** its decisions in the
range to save. The two are measured differently: a decision's whole
expected value is counted on the request that makes it, while it saves a
little on each request after. Saved catches up with expected as
conversations go on. If it stays well below, the guard is too hopeful.

It reads the ledger directory from the config file, without creating one;
`-dir` points it elsewhere. Give `-dir` more than once to read several
ledgers as one, such as one from before the default log folder moved and
one from after; a record in both is counted once.

### How cost is worked out

Cost is computed when you run `janus stats`, not stored, so a corrected
price applies to old records too. Per response:

```
cost = input          x input price
     + output         x output price
     + cache_read     x cache read price
     + cache_write_5m x 1.25 x input price
     + cache_write_1h x 2    x input price
```

The cache read price is listed per model because its discount varies: a
tenth of the input price on most models, a twentieth on Claude Opus 5.5, a
fortieth on Claude Fable 5.1. On Claude Code traffic nearly all input is
cached, so this one number moves the total more than any other.

The table was checked against a real session: Janus's figure matched the
cost Claude Code itself reported to the last digit ($0.238187).

Not priced, and reported as such:

- models with no entry in the table, which includes local models answering
  through the upstream route
- fast mode requests (`usage.speed` is `fast`)
- Claude Haiku 5.5 requests with a prompt over 100,000 tokens, which are on
  a different schedule

Requests answered by the local backend count as zero upstream cost.

Prices change. The shipped table carries the date it was last checked,
printed in every report, and any entry can be corrected or added under
`pricing` in the [config file](CONFIG.md).

## Captures

Each request produces three files sharing a name:

| File | Contents |
| --- | --- |
| `.md` | The readable render: request headers with credentials redacted, model, system prompt, tools, messages, response headers (cookies redacted), and the response assembled from its stream. For a backend tried and given up on, such as Claude at a rate limit when the local model stood in, an `<attempt>` section says why and gives its response headers. Start here. |
| `.request.txt` | The request body exactly as the client sent it |
| `.response.txt` | The response body, decompressed |
| `.sent.txt` | Only when Janus changed the request before sending it: the request as it went. `janus audit -body` prints it. |

A request that was shadowed has three more files beside it:
`.shadow.md`, `.shadow.request.txt` (what was sent to the local model), and
`.shadow.response.txt`. They are pruned together with the capture.

A request whose local answer was rejected in favour of Claude's has
`.attempt.response.txt` beside it: the answer that was thrown away.

`janus replay` adds `.replay.md` and `.replay.response.txt` beside a
capture, holding the local model's answer to it. They too are pruned with
the capture.

Captures hold everything Claude Code sent and received: your prompts, the
files it read, command output. Treat the directory accordingly. The
request body also contains Claude Code's account and device identifiers.

Pruning runs at startup and every 50 captures. It removes day directories
older than `logging.retainDays`, then the oldest captures until the total
is under `logging.maxMegabytes`. It only ever touches directories named
like a date and the files inside them; anything else you put under
`captures/` is left alone.

The originals of digested tool results, in `originals/` (see
[CONFIG](CONFIG.md)), are pruned by age only: at startup and then at most
once an hour while Janus runs, each is deleted once it has gone unused
for `logging.retainDays`. They don't count towards `logging.maxMegabytes`,
because a digest promises its original back for that long.

A body larger than 32 MB is cut short in the capture, and the render says
so. The request and response themselves are always relayed in full.
