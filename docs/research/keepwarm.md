# Spike: keeping the prompt cache warm through idle gaps

Issue: #334 · Date: 2026-10-19 · Status: complete

## Question

The baseline ([baseline.md](baseline.md)) found five idle gaps of 1 to 3
hours that cost $20.34 in cache writes, 7% of the week. The cache expired,
and the next turn wrote the conversation again. If Janus sent one cached
read an hour while you were away, would that keep the cache alive? And
should Janus do it?

## Decision

1. **The mechanism works.** A cached read refreshes the 1-hour cache.
   Tokens written at 09:37 and read at 10:32 were still cached at 11:28,
   110 minutes after they were written. The same tokens in a control
   conversation, written at 09:37 and never read, had expired by 11:28.
2. **Janus will not do it. No build issue is filed.** A keep-warm read is
   a request nobody made, sent with the client's credential after the
   client's request has ended. Today the threat model says the credential
   goes to the upstream as part of the client's own request and nowhere
   else, and Janus never holds it beyond that request. Keeping the
   credential for up to three hours, so that Janus can spend the owner's
   usage on its own, changes what Janus is. The owner has to make that
   call in the charter. A spike cannot make it.
3. **If the charter is changed, this is the design.**
   - The feature is off by default.
   - It applies only to a main-conversation turn that ended normally.
   - It replays that turn's exact prefix, at most once an hour, for at
     most three hours (the stopping rule, below).
   - It stops at the first answer that is not a success, when the client
     sends a new turn, and when Janus stops.
   - The credential stays in memory only, beside the request it came
     with, and is dropped when the replays stop.
   - Each replay is a ledger record of its own, class `keepwarm`, with what
     it read. `janus stats` adds the cost of these replays up against the
     writes that were avoided.

   Two technical questions stay open; see what this does not show.
4. **No console note in its place either.** A line saying "this
   conversation's cache expires at 11:28" would save money only if you
   saw it before you left. Someone who has stepped away does not see it.

## Method

There were two headless Claude Code conversations, `claude -p` with
Claude Code 2.1.303 on Opus 5.5, through a scratch Janus with every
feature off. Each conversation ran in its own empty folder, so they shared
only the start of the system prompt. An earlier run, where both used the
same folder, shared their whole prefix, so one conversation's reads kept
the other's cache warm. That run was discarded.

| Time | Request | Cache read | Cache write (1-hour) |
| --- | --- | --- | --- |
| 09:37 | A1, first turn | 8,239 | 18,312 |
| 09:37 | B1, first turn | 8,239 | 14,398 |
| 10:32 | A2, A resumed: the keep-warm read | 10,837 | 16,160 |
| 11:28 | A3, A resumed again | 26,572 | 446 |
| 11:28 | B2, B resumed: the control | 8,239 | 18,419 |

How to read the table:

- **The first 8,239 tokens are common to both conversations.** A2 and A3
  kept them alive for B too, so they say nothing about B.
- **The next 2,598 tokens are A's own.** A2's read of 10,837 is the
  common 8,239 plus these. They hold the conversation's folder, so B has a
  different 2,598 of its own at the same place.
- **The keep-warm read kept A's tokens.** At 11:28, A3 read them in full,
  110 minutes after A1 wrote them, because A2 had read them at 10:32.
- **The control lost B's tokens.** At 11:28, B2 read only the common
  8,239. B's own tokens, written at 09:37 and not read since, had expired.
- **A3 read the rest too.** The other 16,160 tokens, which A2 wrote 55
  minutes earlier, were cached at 11:28 as well.

All the writes were at the 1-hour lifetime. The five requests together
read and wrote about 130k tokens: under a dollar at list price, and a
small share of the five-hour window.

## The stopping rule

A 1-hour cache write costs twice the input price. A cached read costs a
tenth of it. Each hourly read therefore costs 5% of the write it might
save, but it saves something only if you come back while the cache is
still warm.

- **The five gaps of 1.2 to 2.6 hours:** two or three reads each would
  have covered them.
- **The 38.8-hour gap:** reading every hour would have cost about twice
  the write. The break-even is 20 reads, and that is only if you come
  back at all.

Stopping after three hours keeps every gap the week had under three
hours. In the worst case it spends 15% of a write.

## What this does not show

- **Whether Janus can make the read on its own.** The experiment's reads
  were real turns, made by Claude Code. Janus would have to send the last
  request again without causing a full answer. The cache key leaves out
  `max_tokens`, so cutting it to 1 should be enough, but a conversation
  with thinking on may not accept a limit that small. Only a build would
  find out.
- **What the reads cost in the usage limits.** On a subscription, the
  saving is usage headroom, not dollars. How a cached read and a 1-hour
  write each count against the five-hour window is not published, and
  five requests are too few to measure it.
- **Whether the credential is still good after an hour.** Claude Code's
  OAuth access token may expire within three hours. A replay with an
  expired token would fail harmlessly. It would also save nothing.
- **A resumed `claude -p` conversation does not repeat its first turn's
  prefix exactly.** That is why A2 wrote 16,160 tokens again. A turn in a
  live session keeps its prefix; the baseline's 98.8% cache hit ratio
  shows that. So this affects only how the experiment had to be read, not
  what it found.
- **One pair of conversations, 27k tokens each.** The baseline's gaps were
  at contexts up to 946k. The rule that a read refreshes the cache does
  not depend on size, but this experiment did not test it at that size.
