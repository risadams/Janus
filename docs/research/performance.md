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
- **A synthetic soak is clean.** A soak on hours of real sessions has not
  been done.

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

## What this does not show

- **Real latency.** Against the real API the network and the model
  dominate everything here.
- **A real soak.** The synthetic run shows no leak under load. Hours of
  real Claude Code sessions, with long streams, cancelled requests, and a
  real LM Studio, have not been run as a soak. The sessions run during
  development were minutes long.
- **Other machines.** One fast machine. Pattern matching and JSON scale
  with a single core's speed.
- **The local model.** How long LM Studio takes is in the
  [LM Studio notes](lmstudio.md) and the
  [delegation measurements](delegation.md), and it is seconds to minutes.
