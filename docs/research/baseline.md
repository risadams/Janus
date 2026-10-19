# Baseline: where the cost goes

Issue: #19 · Date: 2026-10-19 · Status: complete

## Question

After a week of ordinary use through Janus, where does the upstream cost
actually go: by request class, by kind of token, by session? And what does
that mean for the charter's G1 target, a cost per working session at
least 30% below the baseline?

## Decision

1. **The baseline for G1 is $23.20 per working session (median), $39.81
   (mean), at list price.** A working session is one Claude Code session id
   with at least 20 main-loop turns; seven of the eight sessions were, and
   they hold all of the cost.
2. **G1's 30% cannot come from the mechanisms Janus has.** The cost is the
   main conversation reading its own cache. Everything Janus can route,
   escalate, digest, or prune together is under 3% of it here (below), and
   the charter's cache rule (R1) is why: the context already sent cannot
   be made smaller without paying to write it again. G1 is restated in the
   charter: its target is now the saving the data shows is in reach,
   reported per session, not 30%.
3. **The one large saving in Janus's reach is the cache expiring while you
   are away.** Five idle gaps of 1 to 3 hours cost $20.34 in cache
   writes, 7% of the week; a cached read every hour of the gap, to keep
   the cache alive, would have cost about $0.85. The spike, #334, found
   that this works but that Janus will not do it: it would mean Janus
   holding your credential and sending requests of its own
   ([keepwarm.md](keepwarm.md)).
4. **The other large lever is how big a session's context grows, and it
   belongs to the user.** Turns above 600k tokens are 13% of turns and 33%
   of the main conversation's cost. Janus can show it as it happens, and
   say when a `/compact` or a fresh session would pay: #335.
5. **Routing subagents to the local model stays an availability feature,
   not a saving.** Subagents were 0.4% of the week's cost. #28, which was
   to prove a saving from them, cannot: its outcome is in its own comments.

## Method

The ledger from 2026-10-12 05:39 to 2026-10-19 07:25 (local time): 3,222
requests, of which 3,060 were upstream Messages requests with usage, from
Janus v0.9.0-rc.4 to rc.17 and Claude Code 2.1.29x to 2.1.303. Costs are
recomputed from each record's usage at the list prices in
`internal/ledger/pricing.go` (as of 2026-10-06), splitting cache writes by
their 5-minute and 1-hour parts; the total, $279.10, is within 0.3% of
`janus stats` ($278.23). Sessions are grouped by the ledger's `session`,
Claude Code's session id. The figures were computed by a script over the
ledger's counts; no captured content was read for them.

Five of the seven days had work on them; there was none on 10-15 or
10-18.

## Findings

### By request class

| Class | Est. cost | Share |
| --- | --- | --- |
| main (the conversation) | $249.33 | 89.0% |
| safety (auto mode's checks) | $28.07 | 10.0% |
| subagent | $1.02 | 0.4% |
| status | $0.86 | 0.3% |
| compaction | $0.76 | 0.3% |

299 of the safety checks were recorded as `main`, on 2026-10-13, before
commit 59cb0a0 gave them a class of their own; they are counted as safety here.
They are Sonnet requests of two messages, no tools, about 170k tokens of
context, and a few tokens of answer.

### By kind of token

| | Share of cost |
| --- | --- |
| Cache reads | 63% |
| Cache writes, 1-hour | 23% |
| Output, thinking included | 14% |
| Uncached input | under 1% |
| Cache writes, 5-minute | under 1% |

The cache hit ratio is 98.8%. Claude Code writes its cache with the
1-hour lifetime (8.3M tokens against 428 at 5 minutes), at twice the
input price.

### By context size

A main-loop turn reads its whole conversation from the cache, so its cost
follows its context:

| Context | Turns | Share of main cost | Median cost per turn |
| --- | --- | --- | --- |
| under 200k | 44% | 22% | $0.041 |
| 200k to 400k | 34% | 32% | $0.076 |
| 400k to 600k | 9% | 12% | $0.124 |
| over 600k | 13% | 33% | $0.181 |

The median main-loop context was 230k tokens, the 90th percentile 723k,
the largest 966k. There were three compactions in the week, at 968k,
420k, and 322k tokens. With a 1M-token window, sessions grow large before
anything shrinks them.

### By session

| Session | Main turns | Est. cost |
| --- | --- | --- |
| 2026-10-13, 11:00 to 21:47 | 991 | $118.45 |
| 2026-10-14, 10:38 to 16:51 | 473 | $72.07 |
| 2026-10-16, 09:59 to 21:04 | 397 | $27.99 |
| 2026-10-17 to 10-19 | 373 | $23.20 |
| 2026-10-13, 09:35 to 10:41 | 215 | $17.75 |
| 2026-10-12, 09:53 to 15:20 | 256 | $15.08 |

Median $23.20, mean $39.81, median 373 main turns. The two most expensive
sessions are also the two with the largest contexts.

### Idle gaps

Six times, more than an hour passed between turns, the cache expired, and
the next turn wrote the conversation again:

| Gap | Context | Cache write | One cached read of it |
| --- | --- | --- | --- |
| 1.2 h | 116k | $0.92 | $0.023 |
| 1.8 h | 694k | $5.55 | $0.139 |
| 2.3 h | 47k | $0.38 | $0.009 |
| 2.5 h | 946k | $7.57 | $0.189 |
| 2.6 h | 740k | $5.92 | $0.148 |
| 38.8 h | 65k | $0.52 | $0.013 |

A read refreshes the cache's lifetime, so one read an hour keeps it. For
the five gaps under three hours, that is about $0.85 instead of $20.34.
Over the 38.8-hour gap, reading every hour would have cost as much as the
write it saved, so keeping a cache warm only pays for a few hours.

### What Janus can reach

| Mechanism | Ceiling here | Source |
| --- | --- | --- |
| Route or escalate subagents, status, compaction | 0.9% | this note |
| Digest large tool results | about 1.5% | [digest.md](digest.md) |
| Prune superseded context | about 0% | [pruning.md](pruning.md) |
| Keep the cache warm through idle gaps | about 7%, not taken | [keepwarm.md](keepwarm.md) |

Auto mode's safety checks (10% where seen) are not on the list: they are
a security decision, and sending them to a local model would weaken it.

## What this does not show

- **One person, one week, one way of working.** Most of the work was
  Janus's own development, in long sessions, with no subagents asked for.
  Someone who uses subagents heavily would see routing matter more.
- **10-13 includes evaluation sessions.** The digest evaluation
  ([digest.md](digest.md)) ran that day, through Janus.
- **Auto mode's safety checks stopped appearing after 2026-10-14 16:50,**
  though auto mode was in use on 10-19. Either they no longer pass
  through `ANTHROPIC_BASE_URL`, or they take a shape Janus does not
  recognise. Which, the ledger cannot say.
- **List prices, not a bill.** The owner's plan is a subscription; the
  dollars measure usage, and the shares are what matters.
- **Task success was not measured.** Nothing here says whether a cheaper
  session would have done the work as well.
