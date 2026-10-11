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
| `route` | string | Where the request was sent. Always `upstream` for now. |
| `reason` | string | Why. Always `passthrough` for now. |
| `stages` | string[] | Transforms applied. Always empty for now. |
| `backend` | string | Which backend answered. Always `upstream` for now. |
| `status` | int | HTTP status, or `0` if no response was started |
| `outcome` | string | See below |
| `error` | string | The API's error type, when `outcome` is `api_error` |
| `ttfb_ms` | int | Time until the first byte of the response body |
| `duration_ms` | int | Time until the response ended |
| `request` | object | Messages requests only: `model`, `stream`, `bytes`, `messages`, `tools`, `cache_breakpoints` |
| `response` | object | Messages responses only: `model`, `stop_reason`, `blocks` (content block types in order) |
| `usage` | object | Messages responses only; see below |
| `unread` | string | Why a Messages response has no `usage`, e.g. `content-encoding br` |
| `capture` | string | The capture's path under `captures/`, without extension |

### `outcome`

| Value | Meaning |
| --- | --- |
| `ok` | The response completed |
| `api_error` | The API answered with an error object; see `error` |
| `http_error` | A 4xx or 5xx whose body Janus could not read as an API error |
| `incomplete` | The response ended before the protocol says it should |
| `cancelled` | The client disconnected first |
| `no_response` | Nothing came back from the backend |

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

Only `input` and `output` are always present. A field the server did not
report is left out, and that is not the same as zero: LM Studio, for
example, reports no cache writes at all.

Total input for a request is `input + cache_read + cache_write`.

### Example

```json
{"v":1,"id":"20261011T150013.033-02e426","time":"2026-10-11T15:00:13.033Z","session":"…","agent":"…","method":"POST","path":"/v1/messages","class":"subagent","route":"upstream","reason":"passthrough","stages":[],"backend":"upstream","status":200,"outcome":"ok","ttfb_ms":654,"duration_ms":2101,"request":{"model":"claude-opus-5-5","stream":true,"bytes":47340,"messages":2,"tools":10,"cache_breakpoints":3},"response":{"model":"claude-opus-5-5","stop_reason":"tool_use","blocks":["tool_use"]},"usage":{"input":2,"output":203,"cache_read":0,"cache_write":17189,"cache_write_5m":17189,"cache_write_1h":0,"thinking":0},"capture":"2026-10-11/20261011T150013.033-02e426_v1-messages"}
```

### Reading it

It is plain JSON Lines, so `jq` works until `janus stats` exists:

```bash
# tokens by class
jq -s 'group_by(.class)[] | {class: .[0].class, requests: length,
  output: (map(.usage.output // 0) | add),
  cache_read: (map(.usage.cache_read // 0) | add),
  cache_write: (map(.usage.cache_write // 0) | add)}' logs/ledger/*.jsonl
```

## Captures

Each request produces three files sharing a name:

| File | Contents |
| --- | --- |
| `.md` | The readable render: headers with credentials redacted, model, system prompt, tools, messages, and the response assembled from its stream. Start here. |
| `.request.txt` | The request body exactly as the client sent it |
| `.response.txt` | The response body, decompressed |

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
