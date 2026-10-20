# Results: how the charter's goals came out

Issue: #56 · Date: 2026-10-20 · Status: complete

## Question

The charter ([CHARTER.md](../CHARTER.md)) sets five goals, G1 to G5, and
the roadmap's 1.0 exit asks for a report that says, with data, how each
came out. For each goal: did real use meet the target? For each
mechanism: what did it save, what did it cost, and should it be on by
default? And what did real use teach that the charter did not expect?

## Decision

| Goal | Target | Result |
| --- | --- | --- |
| G1 Cut upstream spend | About 9% per working session, no mechanism costing more than it saves | **Not met.** Janus saved about 0.3% |
| G2 Keep private data local | Zero protected items upstream, failing closed | **Met** |
| G3 Keep working when upstream doesn't | Degraded but usable, clearly labelled | **Met**, for a usage-limit outage. Shown on Janus's console, not in Claude Code |
| G4 Learn | 100% of requests explained | **Met** |
| G5 Never worse than no Janus | Byte-equivalent pass-through, p50 overhead under 10 ms | **Met**: about 0.2 ms |

1. **Report G1 as not met. Don't lower the target again.** The charter
   lowered G1 once, from 30% to what the baseline showed was in reach.
   That 9% was mostly the keep-warm saving (7%), which Janus declined for
   a security reason ([keepwarm.md](keepwarm.md)). The rest was digests
   (1.5%), which ship off because they cost more time than they save
   ([digest.md](digest.md)). Lowering the target again to match what
   happened would make it a description, not a goal. Whether to restate
   it is the owner's call in the charter; this note recommends not.
2. **Janus is worth running for G2 to G5, not for savings.** Real use
   tested each of them, and the most valuable hour Janus had was a
   stand-in, not a saving (G3 below).
3. **Defaults stay as they are:** a fresh install passes requests
   through, with logging (NFR-7). The table under "By mechanism" says
   which features to turn on.
4. **One follow-up, #353:** the local-only policy matches far too much
   in a repository that talks about secret files (below). Claude Code
   showing nothing during a stand-in (G3) is noted, not filed: Janus
   could label a turn only by changing the answer.

## Method

The data is the owner's own ledger:
- **Range:** 2026-10-12 05:39 to 2026-10-20 04:20 (local time).
- **Size:** 3,556 requests, 7 working sessions, an estimated $302.52
  at list price.
- **Versions:** Janus v0.9.0-rc.4 to v0.9.0, Claude Code 2.1.29x to
  2.1.303.

Costs and the local saving are from `janus stats`. G2's counts are from
`janus audit -verify`. The rest came from a script over the ledger's
fields; it read no captured content.

The config in daily use:
- **On:** routing, from 10-13, and the guard, with redaction and a
  local-only policy for `.env*`, `secrets/**` and `*.pem`.
- **Routing rules:** subagents go `local-then-escalate` to
  `qwen/qwen3.8-27b` with a 150-second limit. The main loop goes
  upstream, with the local model as its `ifDown`.
- **Off:** `escalate` and `compress`.
- **Not used:** the worker tools.

No new working session started after the baseline. The session running
since 10-17 is the one that wrote this note. So G1 is measured as what
the mechanisms saved over the whole range. Comparing the cost per session
before and after would say nothing: the config stayed the same throughout.

## By goal

### G1: cut upstream spend

- **Routing saved about $0.91 over the whole range, 0.3% of $302.52.**
  That was 32 subagent requests answered locally. `janus stats` prices
  them at the list price of the model each one asked for.
- **Nothing else in use saved money.** The 61 stand-in turns (G3) did
  not: the upstream was refusing requests, so nothing was avoided.
- **The saving cost time, not money.** 15 subagent attempts failed and
  went to Claude: 8 too slow, 7 returned a server error. 4 more went to
  Claude without being tried, because the local model was busy. The
  failed attempts kept the client waiting 1,016 s in all, about 17
  minutes, for a saving of 91 cents. No attempt raised the bill: a
  failed one costs what Claude would have cost anyway.
- **Task success was not measured.** Nothing in the ledger shows whether
  a local subagent answer was as good as Claude's would have been.

Why the target was out of reach is in [baseline.md](baseline.md): 89% of
the cost is the main conversation reading its own cache, and charter R1
puts that out of bounds.

### G2: keep private data local

- **Real use:** 395 requests bound for Anthropic carried something that
  looked like a secret: 465 AWS access keys and 122 assigned secrets.
  All 395 had them replaced with placeholders, and none was sent with
  them.
- **Captures re-read:** `janus audit -verify` reread the 385 requests
  still kept as sent, and found nothing that looked like a secret. The
  other 3,095 had been pruned by the 7-day retention.
- **Local-only content:** 1,582 requests had it withheld, replaced by a
  notice. Not one was sent with it.
- **Failing closed:** the guard cut off 3 turns on 10-12 where the model
  wrote a placeholder in a form Janus could not restore. Corrupting a
  tool call instead would have been worse (charter R7).
- **The corpus:** `TestNothingProtectedReachesTheUpstream` sends a request
  holding every kind of secret, plus local-only content, down every route
  Janus has (64 cases), and finds nothing protected upstream.
  `TestTheLeakCheckCanFail` shows the same check catches all of it with
  the guard off. The detector finds 21 of 21 formatted secrets, with no
  false positives on 44 lookalikes (GUARD.md).

### G3: keep working when upstream doesn't

- **The outage that happened was the owner's own usage limit.** On
  10-14 the five-hour limit refused every request from 09:54 until Claude
  answered again at 11:42. On 10-13 it ran out from 17:49 to 17:59.
- **The local model carried the session.** It answered 66 main-loop
  turns in place of Claude, 60 of them on 10-14. 61 ended normally, each
  with a tool call (Bash, Edit, Read, Write). 3 were cancelled by the
  user. 2, on 10-13, were the requests that stopped that first
  stand-in. The fixes for that stand-in are #169 to #172 (see
  [standin.md](standin.md)), and none failed after them. The median turn
  took 32 s, against Claude's few seconds.
- **The local model saw only part of the conversation.** It left out
  older messages to fit its context, up to 782 of 899, and a note told it
  so (WHAT-JANUS-CHANGES).
- **Back to Claude:** 127 requests to Claude had the local model's
  unsigned thinking removed from their history. Claude refused none. Charter
  R2 feared a mixed conversation would be rejected; it was not.
- **Brief faults:** two connection resets on 10-16 and 10-17 were retried
  by Claude Code, and the next request succeeded. Nine 502s in two
  minutes on 10-12 came before routing was on, so nothing stood in.
- **Labelled:** the console marks each stand-in turn with the model's
  name and "(local)", and the response names the local model. Claude
  Code itself shows nothing different. It shows the turn as it would
  show Claude's. "Clearly labelled" holds for someone watching Janus's
  console, not for someone watching only Claude Code.

### G4: learn

- **Every request is explained.** All 3,556 ledger records have a decision
  reason and a capture reference. 1,364, from before routing was on, say
  pass-through. The other 2,192 name the rule that decided, and say why
  when the request went anywhere other than the rule's first choice: a
  local answer accepted or turned down, a stand-in, or why there was
  none.
- **Local and upstream answers can be compared offline**, for as long as
  captures are kept (7 days here):
  - `janus replay` re-runs a captured request against either backend.
  - A `shadow` rule records both answers as they happen.
  - Neither was used in daily use. The comparisons in the research notes
    were all made with replay.

### G5: never worse than no Janus

- **Byte-equivalent:** `internal/proxy/fidelity_test.go` holds
  pass-through byte for byte on every CI run, on three operating systems.
  The one sanctioned exception is narrowing `Accept-Encoding`.
- **Overhead:** about 0.2 ms at p50 on a 300 KB request, with routing on
  or nothing on, against the 10 ms budget. The guard adds about 24 ms
  with redaction and local-only both on ([performance.md](performance.md)).
- **Stable:** both soaks were clean. The real one ran 7 h 47 m on
  v0.9.0, with no growth in memory, threads, or open files.

## By mechanism

| Mechanism | Used | Saved | Cost | On by default? |
| --- | --- | --- | --- | --- |
| Route subagents (`local-then-escalate`) | from 10-13 | $0.91 (0.3%) | 17 min of waiting on 15 failed attempts | No. Worth turning on for availability, not for savings |
| Stand-in (`ifDown`) | from 10-13 | nothing in dollars; 66 turns that would have failed | the local model's slower, partial-context turns | No, but it is the first rule to add |
| Escalate (`features.escalate`) | no | not measured in use | | No |
| Guard: redaction | yes | 395 requests' secrets kept | about 9 ms a turn; 3 turns stopped | No, but recommended |
| Guard: local-only policy | yes | 1,582 requests' content kept | over-matching (below) | No; set the paths with care |
| Digests (`features.compress`) | no | about 1.5% if on ([digest.md](digest.md)) | about 2.5 min a day of waiting | No |
| Pruning | not built | about 0% ([pruning.md](pruning.md)) | | |
| Worker tools (`janus mcp`) | no | trades time for usage ([delegation.md](delegation.md)) | slower answers | Opt-in |
| Thinking repair | 127 requests | kept mixed conversations working | none seen | Always on; not a switch |

## What was learned that was not expected

- **The cost is the main conversation, not the side work.** The charter
  expected session titles, summaries, test logs, and subagents to be
  where a local model saves money. Utility calls don't appear in Claude
  Code's traffic at all ([models.md](models.md)), and subagents were 0.3%
  of the cost. Charter R5 ("savings are smaller than hoped") came true,
  and by more than feared.
- **The local model is good enough to work with; the plumbing was the
  hard part.** In the stand-in turns that [standin.md](standin.md)
  reviewed, it did what Claude would have done. What stopped it was the
  protocol around it:
  - tool search's `tool_reference` blocks
  - loading a model that was not loaded
  - conversations too long for its context
  - one model busy with another request

  Charter R3 expected the tool loop to be the hard part.
- **On a subscription, the outage that matters is your own limit.** The
  API never went down for long. The owner's five-hour limit ran out
  twice. The roadmap's M8 (Pace) follows from this.
- **Mixed conversations work.** Removing unsigned thinking was enough for
  Claude to accept a conversation the local model had added turns to,
  all 127 times.
- **The local-only policy matches too much where secrets are the subject.**
  It withheld content from 1,582 of 3,480 upstream requests, almost half,
  in sessions spent building the guard. It matched commands that only
  named a pattern: a command naming `.environments[]` matched `.env*`,
  and one naming `*.pem` matched `*.pem`. Nothing leaked, but Claude was
  given notices in place of output that held nothing protected. This is
  failing closed by design: Janus cannot tell what a command reads, so it
  searches every word of it. #353 is to tell more cases apart safely.
- **The secret counts measure turns, not secrets.** A secret stays in the
  conversation once read, so it is counted again on every later turn.
  Most were likely the guard's own test fixtures, which are made to look
  real; this note did not read the captures to tell them from real
  secrets.

## What this does not show

- **One person, one week, mostly one project.** Most of the work was Janus
  building itself: long sessions, few subagents, many fake secrets.
  Another person's numbers would differ, most of all for G1 and G2.
- **Task success.** Neither the local subagent answers nor the stand-in
  turns were scored against what Claude would have done.
- **Escalation, digests, and the worker tools in daily use.** They were
  measured in their own spikes, and stayed off in daily use.
- **A real API outage.** G3 was tested by a usage limit and a few
  connection resets, and by the outage tests in `internal/route`; never by
  a long outage of the API itself.
- **Overhead in daily use.** The ledger cannot time a direct connection
  next to the proxied one. G5's figures are from the benchmarks.
- **List prices, not a bill.** The plan is a subscription. The dollars
  measure usage, and the shares are what matter.
