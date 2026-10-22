# Performance

Measured 2026-10-11 on an 18-core Apple silicon Mac, Go 1.27.2. Question:
does Janus meet NFR-1, "no worse than direct", and what does each feature
cost?

## Decision

- **Pass-through meets the budget with room to spare**: about 0.2 ms added
  at the median on a 300 KB request, against a budget of 10 ms.
- **The guard is where the time goes**, and it is small beside a model's
  answer: with redaction and a local-only policy both on, about 24 ms on a
  300 KB request, once Janus has seen the conversation. Claude takes
  seconds.
- **Redaction was too slow as first built** (54 ms on every 300 KB
  request) and was fixed here: the detector now remembers what it has
  already scanned. It costs that once per conversation and about 9 ms
  after.
- **Configuring OpenAI changes nothing that goes to Claude** (#409): the
  fidelity test runs again with OpenAI configured and listed after
  Claude, and every case matches a direct connection byte for byte.
- **Translating to OpenAI costs about 3 ms on a 300 KB request**, and
  about 29 ms at 3 MB, beside OpenAI's seconds; it is spent only on
  requests that go to OpenAI. A long streamed answer costs about 1.3 µs
  an event to translate back (2026-10-22, below).
- **Both soaks are clean.** The synthetic one, and a working day of real
  Claude Code sessions through v0.9.0: memory, threads, and open files
  stayed flat, every request ended, and none failed.

## Method

`internal/route/perf_test.go`, run with:

```bash
JANUS_PERF=1 go test ./internal/route -run 'TestMeasureOverhead|TestSoak' -v -timeout 30m
```

A request shaped like one from Claude Code's main loop (a long system
prompt, forty tools, a history of tool calls and results, a few secrets,
one read of a protected file) is sent to a stand-in for Anthropic that
answers at once with a short stream, directly and then through Janus with
each feature on. Each request adds one new 4 KB turn to the conversation,
as a session does. 300 runs each (60 at 3 MB), after a warm-up.

The stand-in answers in a fraction of a millisecond, so what is measured
is Janus's own work and nothing else. Both ends are on loopback.

## What each feature adds

The sizes are small, large, and very large. In the short sessions in the
[traffic notes](traffic.md), Claude Code's requests ran from about 45 KB
to 130 KB, which falls between the first two tables; a request grows with
the conversation, so 293 KB stands for a long one.

**First request** is the whole time for the first request of a
conversation, when Janus has seen none of it. **Added** is the time over
a direct connection for the requests after.

### 32 KB

| Through Janus with | First request | Added at p50 | Added at p95 |
| --- | --- | --- | --- |
| Pass-through, nothing on | 0.8 ms | 0.0 ms | 0.1 ms |
| Routing rules | 0.7 ms | 0.2 ms | 0.3 ms |
| Captures kept | 0.3 ms | 0.0 ms | 0.1 ms |
| Redaction | 3.2 ms | 0.6 ms | 0.5 ms |
| Redaction with entropy search | 3.2 ms | 0.6 ms | 0.6 ms |
| Local-only policy | 0.9 ms | 0.6 ms | 0.5 ms |
| Everything | 4.0 ms | 1.3 ms | 1.4 ms |

### 293 KB

| Through Janus with | First request | Added at p50 | Added at p95 |
| --- | --- | --- | --- |
| Pass-through, nothing on | 0.7 ms | 0.2 ms | 0.2 ms |
| Routing rules | 3.4 ms | 2.9 ms | 3.0 ms |
| Captures kept | 0.6 ms | 0.2 ms | 0.2 ms |
| Redaction | 53.7 ms | 9.4 ms | 9.6 ms |
| Redaction with entropy search | 58.4 ms | 9.5 ms | 9.7 ms |
| Local-only policy | 12.5 ms | 12.0 ms | 12.3 ms |
| Everything | 73.4 ms | 23.9 ms | 24.1 ms |

### 3 MB

| Through Janus with | First request | Added at p50 | Added at p95 |
| --- | --- | --- | --- |
| Pass-through, nothing on | 2.3 ms | 0.2 ms | 0.9 ms |
| Routing rules | 28.3 ms | 26.7 ms | 27.3 ms |
| Captures kept | 2.3 ms | 0.9 ms | 1.0 ms |
| Redaction | 577 ms | 91 ms | 92 ms |
| Redaction with entropy search | 632 ms | 93 ms | 93 ms |
| Local-only policy | 121 ms | 119 ms | 120 ms |
| Everything | 774 ms | 234 ms | 236 ms |

## Reading the numbers

- **Pass-through copies bytes and reads them as they go by.** Its cost
  does not grow with the request in any way that matters.
- **Anything that must understand the request parses it**, and parsing is
  about 2.5 ms per 300 KB with the standard library's JSON. Routing parses
  once. That is nearly all of its cost.
- **Redaction** is dominated by running the detectors' patterns over the
  text, at about 6 MB a second. A conversation is sent again in full with
  every turn, so the detector keeps a hash of each piece of text it has
  scanned and what it found there, and only new text is scanned. That took
  a 293 KB request from 54 ms to 9 ms. The memory holds hashes and
  positions, never text or secrets, and is bounded at 50,000 entries.
  What is left is parsing, hashing, and writing the request out again.
- **The local-only policy** parses and checks the request before routing,
  and when something is protected, does so again on what will be sent and
  once more to confirm nothing is left. Three passes; almost all of it is
  JSON. The measured conversation always holds a protected read, which is
  the expensive case. A request with nothing protected is checked once.
- **Detect-only guarding** adds nothing to a request: the scan happens
  after the response has gone.
- **The entropy search** costs little beyond the rest of redaction.
- **Costs add.** "Everything" is close to the sum of its parts, because
  each part parses the request for itself. Sharing one parse between them
  would save perhaps a third. Not done: it is 24 ms against seconds.

## Soak

40,000 requests of 96 KB, eight at a time, with routing, escalation (one
request in five tried on the local model first), captures, redaction with
the entropy search, and the local-only policy all on:

| After | Heap in use | Goroutines |
| --- | --- | --- |
| 5,000 | 14.7 MB | 62 |
| 10,000 | 16.3 MB | 62 |
| 15,000 | 16.8 MB | 62 |
| 20,000 | 14.5 MB | 62 |
| 25,000 | 15.8 MB | 62 |
| 30,000 | 17.8 MB | 62 |
| 35,000 | 16.3 MB | 62 |
| Done, connections closed | 1.9 MB | 24 |

59 seconds, 682 requests a second, none failed, no stuck streams. Memory
and goroutines are flat while it runs and fall away when it stops.

## Translation (#409)

Measured 2026-10-22, same machine, Go 1.27.3, with
`JANUS_PERF=1 go test ./internal/route -run TestMeasureTranslation -v`.
The requests are the overhead measurement's, built the same way; the
stream is the fake OpenAI's long answer, 2,265 events of text and a tool
call.

| Request | Translate to OpenAI, p50 |
| --- | --- |
| 32 KB | 0.11 ms |
| 289 KB | 1.39 ms |
| 3,071 KB | 14.4 ms |

| Stream | Events | Translate back, p50 | Per event |
| --- | --- | --- | --- |
| 537 KB | 2,265 | 2.95 ms | 1.3 µs |

A request is translated twice on its way to OpenAI: once when it is
planned, to know it can be, and once just before sending, on the body as
the guard leaves it. So the cost per request is about twice the first
table: about 3 ms at 300 KB, 29 ms at 3 MB. Stream translation runs as
the events arrive, so it adds about a microsecond and a half to each.

The soak above, rerun with one turn in seven sent to OpenAI and the next
back to Claude (40,000 requests, 8 at a time, everything on), stayed
flat at 16 to 19 MB of heap and 62 to 65 goroutines, fell to 2.8 MB and
25 when it ended, and had no failures: 67 s, 595 requests a second. A
first run grew to 588 MB, which was the fake OpenAI keeping every request
it was sent; it now has a setting to keep none.

## Soak with real sessions

On 2026-10-19 the Janus in everyday use was sampled every five minutes
(`ps` and `lsof`, with the day's ledger) while Claude Code worked through
it. Routing and the guard were on, with captures, and two routing rules
that stand in for Claude only when it is down. The day's first process,
v0.9.0-rc.17, ran for 2 h 23 m and was stopped at 09:17 to upgrade. Its
successor, v0.9.0, started at 09:35 and was sampled for 7 h 47 m, 94
samples:

| Local time | Memory (RSS) | Threads | Open files | TCP | Requests so far |
| --- | --- | --- | --- | --- | --- |
| 09:37 | 69.2 MB | 18 | 18 | 3 | 0 |
| 10:37 | 70.6 MB | 18 | 21 | 5 | 10 |
| 11:37 | 62.9 MB | 18 | 18 | 2 | 27 |
| 12:37 | 63.0 MB | 18 | 19 | 3 | 31 |
| 13:07 | 63.8 MB | 19 | 17 | 1 | 132 |
| 14:07 | 40.5 MB | 19 | 17 | 1 | 132 |
| 17:22 | 39.9 MB | 19 | 17 | 1 | 132 |

- **No leak.** Memory peaked at 70.6 MB and fell to 40 MB once the work
  stopped. Threads stayed between 18 and 19. Open files stayed between 17
  and 21, and went back to 17 every time it was idle. TCP connections
  went back to one, the listener.
- **No stuck streams.** The v0.9.0 process recorded 139 requests: 135 of
  them streamed, 134 main-loop turns, one compaction, and four token
  counts. Every one ended `ok`. The median took 5.7 s and the longest
  81 s. None were cancelled, and none waited more than 30 s for a first
  byte.
- **The rc.17 process before it** went from 50.6 to 68.9 MB over 2 h 23 m,
  while it served its first 150 requests of the day. Its threads stayed
  at 18 to 20 and its open files at 17 to 19.

## What this does not show

- **OpenAI's own latency.** Translation is measured against the fake; no
  request has gone to OpenAI.

- **Real latency.** Against the real API the network and the model
  dominate everything here.
- **A heavy day.** The real soak served 139 requests in its 7 h 47 m,
  most of them in the first three and a half hours; the busiest day of the
  baseline week ([baseline.md](baseline.md)) had about a thousand. Every
  request went to Claude, so the local model, escalation, and cancelled
  requests were not exercised. The synthetic soak covers volume and
  escalation, but not a real LM Studio.
- **Other machines.** One fast machine. Pattern matching and JSON scale
  with a single core's speed.
- **The local model.** How long LM Studio takes is in the
  [LM Studio notes](lmstudio.md) and the
  [delegation measurements](delegation.md), and it is seconds to minutes.
