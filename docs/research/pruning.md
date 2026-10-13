# Evaluation: pruning superseded context

Issue: #45 · Date: 2026-10-13 · Status: complete

## Question

Content that a later turn supersedes, such as a file read again or edited
since, still rides along on every turn. Does collapsing it save money,
once the cache it breaks is paid for?

## Decision

- **No pruning transform.** Over two days of captures, the net-savings
  guard (#43) would have approved two prunes, worth less than a cent. With
  perfect hindsight the best possible saving was $0.03 on a day of
  ordinary use; pruning everything superseded would have lost $2.55.
- **Charter R1 holds where it was expected to.** Changing anything already
  sent breaks the cache from there on, and Claude Code's turns read almost
  everything from cache. The pipeline and the guard already ensure Janus
  never does this unless it pays; the numbers show it almost never would.
- **M5's exit is met** for both transforms: digestion ships off with its
  numbers (docs/research/digest.md), and pruning is not built, with these.

## Method

From the captures of 2026-10-12 (ordinary use) and 2026-10-13 (mostly
this work's own sessions), each conversation's longest request was taken
as its history. A tool result counted as superseded at the first later
message that touched the same file: a Read, Edit, or Write of it, or a
Bash command printing it (`cat`, `sed -n`, `head`, `tail`, `nl`). That
is generous: a later `sed -n 1,40p` does not really supersede an earlier
`sed -n 200,300p`. Repeated identical Bash commands were looked for too;
there were none.

For each, at the turn it was superseded:

- **cut**: its size less a 120-byte note saying what was dropped;
- **tail**: the cached history from it up to that turn, which pruning
  writes to cache again instead of reading it;
- **later turns**: the requests that followed, from the captures.

Each prune was priced as the guard prices it, at `claude-opus-5-5`'s rates
(read $0.20, 1-hour write $8 per million tokens), at 3 bytes a token:
worth `cut × read × (turns + 1) − tail × (write − read)`, with the guard's
estimate of later turns, half the turns so far, for its decision, and the
real count for the outcome.

## Results

| Day | Superseded | Tokens held | Guard approves | Their outcome | Best possible, in hindsight | Pruning everything |
| --- | --- | --- | --- | --- | --- | --- |
| 2026-10-12 | 21 | 16,251 | 0 | – | $0.03 | −$2.55 |
| 2026-10-13 | 36 | 64,844 | 2 | +$0.008 | $0.24 | −$15.71 |

Counting only exact repeats (a file read again with Read, or read then
edited), 2026-10-12 had 4, holding 6,241 tokens; the guard approved none,
and pruning all four would have lost $1.46.

## Why

- **The tail is large and the cut is small.** For the exact repeats, the
  history between a superseded result and the turn that superseded it
  was typically 9 times the result's size. Breaking even needs about 39
  later turns for every token of that tail, per token cut: a few hundred
  turns per prune.
- **The worst cases are the long conversations** where pruning seems most
  attractive: one 530-token read pruned under 189,000 tokens of history
  would have cost $1.46 by itself.
- **The guard's caution is right here.** Its estimate of later turns is
  deliberately low, and it still approved only two prunes in two days,
  both profitable.

## What this does not show

- Two days, one person's work, and a generous definition of superseded.
  More traffic would change the totals, not the ratio of write to read
  prices that decides them.
- Pruning where the cache is broken anyway, for example in the first
  request after Claude Code compacts a conversation, was not measured.
  Compaction already replaces the history with a summary, so there is
  little left to prune.
