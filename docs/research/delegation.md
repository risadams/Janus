# Measurement: does delegating a read to the local model pay?

Date: 2026-10-11 · Status: one task measured; enough to change the plan, not enough to settle it

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

## What this does not show

- One task, one file, one model, one run each way. It is a single case
  that happened to be the worst one for the tool: a needle in a haystack.
- **The case the tool is for was not measured**: a question whose answer
  needs the whole of a large text, where Claude would have to read it all.
  There the upstream saving should be real and the comparison is local
  reading time against Claude's reading cost. That measurement is the next
  thing worth doing.
- Whether Claude, given the new description, chooses well between
  searching and delegating on its own. In this test it was told which to
  do.
- A faster local model, or a smaller one, would change the time side.
