# The ledger and captures

Janus writes two kinds of record under `logging.dir` (default `logs/`).

| | Ledger | Captures |
| --- | --- | --- |
| Where | `logs/ledger/<day>.jsonl` | `logs/captures/<day>/<id>_<path>.*` |
| Holds | One line per request: what it was, what Janus did, what it cost | The full request and response |
| Contains prompts or code | No | Yes |
| Contains credentials | No | No (headers are redacted in the render and not stored elsewhere) |
| Can be switched off | No | Yes, `logging.capture` |
| Pruned | Never | By age and total size |

Days are UTC. Every file is created readable by its owner only (0600, in
0700 directories).

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
| `class` | string | `main`, `subagent`, `connectivity`, or `unknown` |
| `route` | string | Where the request was sent: `upstream`, `local`, or `shadow` for a shadow run |
| `reason` | string | What decided it: `passthrough` (nothing matched), `model alias <name>`, or `rule <name>`. When a rule's local route could not be used, the reason says why and ends `; sent upstream`. For a refused request, why. |
| `stages` | string[] | Changes made to the request on the way: `set_model`, `lift_tool_additions`, `drop_deferred_tools` |
| `backend` | string | Which backend answered: `upstream`, `local`, or `none` if Janus refused the request itself |
| `status` | int | HTTP status, or `0` if no response was started |
| `outcome` | string | See below |
| `error` | string | The API's error type, when `outcome` is `api_error` |
| `ttfb_ms` | int | Time until the first byte of the response body |
| `duration_ms` | int | Time until the response ended |
| `request` | object | Messages requests only: `model`, `stream`, `bytes`, `messages`, `tools`, `cache_breakpoints` |
| `response` | object | Messages responses only: `model`, `stop_reason`, `blocks` (content block types in order), `tools` (names of the tools called, in order) |
| `usage` | object | Messages responses only; see below |
| `unread` | string | Why a Messages response has no `usage`, e.g. `content-encoding br` |
| `fault` | string | What went wrong reading the response from the backend when it broke off part-way, e.g. a dropped connection. Absent for a response that arrived whole, and for one the client abandoned. |
| `capture` | string | The capture's path under `captures/`, without extension |
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
janus stats -json                # the same report, for scripts
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

It reads the ledger directory from the config file, without creating one;
`-dir` points it elsewhere.

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
| `.md` | The readable render: headers with credentials redacted, model, system prompt, tools, messages, and the response assembled from its stream. Start here. |
| `.request.txt` | The request body exactly as the client sent it |
| `.response.txt` | The response body, decompressed |

A request that was shadowed has three more files beside it:
`.shadow.md`, `.shadow.request.txt` (what was sent to the local model), and
`.shadow.response.txt`. They are pruned together with the capture.

Captures hold everything Claude Code sent and received: your prompts, the
files it read, command output. Treat the directory accordingly. The
request body also contains Claude Code's account and device identifiers.

Pruning runs at startup and every 50 captures. It removes day directories
older than `logging.retainDays`, then the oldest captures until the total
is under `logging.maxMegabytes`. It only ever touches directories named
like a date and the files inside them; anything else you put under
`captures/` is left alone.

A body larger than 32 MB is cut short in the capture, and the render says
so. The request and response themselves are always relayed in full.
