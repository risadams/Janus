# Measurement: does delegating a read to the local model pay?

Date: 2026-10-11 · Status: two tasks measured, one run each; enough to decide what to build next

## Question

`ask_files` lets Claude hand a file to the local model and get back a short
answer, so that the file never enters Claude's context. How much does that
save, and what does it cost in time?

## Method

A 250 kB server log (3,200 lines) with one failed request in it, on line
2,417. The same question was put to Claude Code twice, through Janus so
that cost could be read from the ledger:

> In server.log, one request failed. Tell me its request id, status, error
> message, and line number.

Once with nothing special, and once with the `janus` MCP server registered
and an instruction to use `ask_files` and not read the log itself. Local
model: `qwen/qwen3.8-27b`, loaded with a 107,008-token context, on an
Apple M5 Max.

## Result

| | Wall time | Upstream cost at list price | Upstream requests | Answer |
| --- | --- | --- | --- | --- |
| Claude on its own | 13 s | $0.150 | 4 | Correct |
| Claude delegating to `ask_files` | 351 s | $0.116 | 3 | Correct |

Delegating saved $0.034, or 23%, and took 27 times as long.

## Why

**Claude did not read the file.** Left to itself it searched the log for
the failure, read the lines around the hit, and answered in four turns.
The 250 kB never entered its context. The saving that delegation was
supposed to deliver was mostly not there to be had.

**The local model did read the file, all of it.** It took three passes
(two halves and a pass to combine the notes) and 5 minutes 37 seconds,
which is about 0.76 kB per second, or a minute for every 45 kB.

The "tokens avoided" figure Janus computed for this call was about 63,000:
the size of the file in tokens, less the answer. The real saving, from the
ledger, was nearer 4,000 input tokens. The figure assumes Claude would
have read every byte, and it would not have.

## What changes

1. **The tool's description now tells Claude what it is not for.** Finding
   a string, an error, or a name is a search, and Claude should do that
   itself. The tool is for questions that need everything read: summarise,
   explain, compare.
2. **The savings figure is labelled a ceiling**, in the ledger, in `janus
   stats`, and in the docs. The only honest measure of saving is cost per
   session with and without the tool.
3. **One call reads at most 1 MB**, down from 6 MB. At the measured rate
   6 MB would have been over two hours.
4. **The other planned worker tools need rethinking before they are
   built.** "Search a tree and summarise" (#38) competes directly with
   what Claude already did here in 13 seconds. It should not be built on
   the assumption that it helps.

## Second measurement: a question that needs the whole text

The first task was a needle in a haystack, the worst case for the tool.
This one is the case it is for.

**Task.** The Janus source itself: 29 non-test Go files under `internal/`,
164 kB. Claude Code was asked for an architecture summary of at most 450
words: what each package is responsible for, and the path of one
`POST /v1/messages` request from arrival to its ledger record, naming the
functions in order. Three conditions, one run each, all through Janus.

| | Wall time | Upstream cost at list price | Cache written | Used `ask_files` |
| --- | --- | --- | --- | --- |
| A. Claude on its own | 39 s | $0.426 | 40,515 tokens | n/a |
| B. Tool available, Claude free to choose | 37 s | $0.431 | 39,132 tokens | Not once |
| C. Claude told to delegate all reading | 485 s | $0.251 | 19,853 tokens | 4 calls |

**Delegating saved 41% of the upstream cost and took 12 times as long.**
Of the 485 seconds, 455 were the local model reading.

**Quality held.** All three answers were checked against the code. A and C
both described every package correctly and traced the request path
accurately, with real function names. C was a little less detailed (it
left out two functions A named) and contained no errors that were found.
Claude in condition C also verified the function locations itself with a
search before answering.

**Given the choice, Claude did not use the tool.** In condition B it read
the code its own way and the cost matched A to within 1%. The tool's
description says it is slow, and Claude acted on that.

**Claude reads selectively even here.** Condition A wrote about 40,000
tokens to cache for the whole session. The source alone is larger than
that. Claude chose what to read; the local model, asked to read "the
files", read 402 kB across four calls, tests included.

**The ceiling figure overstated the saving again**, by less this time:
"avoided at most" was 99,280 tokens, against about 20,700 fewer tokens
written to cache and 53,500 fewer read from it.

## Reading the two together

| Task | Saving | Slowdown |
| --- | --- | --- |
| Find one failing line in a 250 kB log | 23% ($0.034) | 27x |
| Summarise 164 kB of source | 41% ($0.175) | 12x |

- Delegation does reduce upstream cost when the task needs broad reading.
  The charter's mechanism is real.
- The price is time, and a lot of it: about seven and a half minutes of
  waiting to save seventeen cents at list price in the better case.
- Claude will not pay that price unprompted. As described now, the tool is
  only used when the user or a project instruction asks for it.
- For comparison, routing whole subagents to the local model cut upstream
  cost by far more on its test task with no change to how Claude behaves,
  because the reading and the reasoning both moved.

## Decisions

1. **Keep `ask_files`, as an opt-in.** It is honest about what it is. It is
   worth having when usage headroom matters more than time: a long
   unattended job, or a subscription close to its limit.
2. **Do not build "search a tree and summarise" (#38).** Claude's own
   searching was the reason the first task showed so little saving.
3. **Do not build "draft or transform" (#39) on the current evidence.**
   Nothing here speaks to it either way, and it should not go ahead on the
   assumption that it helps.
4. **Guidance (#40) becomes a choice, not a tuning exercise.** Claude can
   be told to prefer the tool, in a project's instructions. Whether to do
   that is a decision about trading time for usage, and belongs to whoever
   is waiting.

## What this does not show

- Two tasks, one model, one run of each condition. Conditions A and B of
  the second task were the same work and came within 1% of each other,
  which suggests the cost figures are steady, but one run cannot show
  variation in the delegated case.
- Quality was judged by reading three answers against code the judge
  knows well. That is a check, not a benchmark.
- A faster local model, or a smaller one, would change the time side, and
  possibly the quality side with it.
- Costs are list prices. On a subscription the saving is usage headroom.
