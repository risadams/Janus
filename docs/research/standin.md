# Evaluation: the local stand-in at a session limit

Issue: #168 · Date: 2026-10-14 · Status: complete

## Question

On 2026-10-13 the owner's 5-hour limit ran out at 21:49, with the main
rule's `ifDown` pointing at `qwen/qwen3.8-27b`. The session did not keep
working on the local model, as `ifDown` promises. Why not, and what does
it take?

## Decision

- **Fix what stopped the session:**
  - a tool search's `tool_reference` blocks reach the local model as
    text (#169)
  - Janus loads an unloaded model itself, at `contextTokens` or 65,536
    tokens (#170)
  - a stand-in on a conversation too long for the local model leaves out
    older turns (#171)
  - a stand-in waits for a busy model instead of failing (#172)
- **Auto mode's safety checks stay with Claude by default**, and the
  console says to leave auto mode when one cannot reach Claude (#173). A
  `safety` rule with `ifDown` is documented as an opt-in. The local model
  can answer the checks in the expected format, but they carry the
  conversation's transcript: a check late in a long session is too large
  for any local context, and a short one took 46 s.
- **The local model's answers were not the problem.** In the four turns
  it carried, it did what Claude would have done: it ran the right
  commands, and when they were refused it said why and worked around it.

## What happened

From the ledger and captures of 2026-10-13, 21:49 to 21:59:

| Time | Request | What happened |
| --- | --- | --- |
| 21:49:59 | Main session, 604 messages, ~417k tokens | 429; no stand-in, because the request fits no local context |
| 21:50:53 | Fresh session after `/clear`, ~33.7k tokens | 429; no stand-in, because the model was not loaded and a cold start was capped at 32,768 tokens (#149) |
| 21:52–21:57 | Same session, after the model was loaded by hand at 107,008 tokens | Four local answers, 21–84 s each. Every Bash call was refused: the `safety` checks went to Claude and got 429s, which Claude Code reports as "temporarily unavailable" |
| 21:59:00 | Same session, after a ToolSearch | LM Studio 400: `request.messages.8.content.0.content.0.type: Invalid literal value, expected "text"`, the `tool_reference` block. Every later turn would fail the same way |

A fresh Claude Code main turn is 25–35k tokens before any work: the
system prompt, `CLAUDE.md`, and the tool definitions.

## Checks after the fixes

- **The 21:59 request**, replayed on qwen3.8-27b with the
  `tool_reference` fix: a tool call in 21.5 s.
- **The 21:49 request** (1.26 MB, ~417k tokens), sent through a scratch
  Janus whose upstream always answers 429, with older turns left out: a
  sensible tool call (`git branch`, `git status`) in 87 s. LM Studio
  counted 52,910 input tokens. Janus estimates bytes / 3, which came to
  about 80k, so the estimate errs on the safe side.
- **Safety checks.** The 21:47:59 check was ~157k tokens, the whole
  transcript, and could not be replayed. The 21:53:26 check (~32k
  tokens), replayed: `<severity>5` in 7 output tokens, stopped at
  `</severity>`, in 46 s. The scale is 0–100 with 50 the allow/block line,
  and the action was a read-only `git log`/`tag`/`status`, so 5 is sound.
  Thinking is disabled on these requests, so the 64-token budget is
  enough.

## What this does not show

- The quality of a long stand-in session. Four turns were seen on the
  day, and one turn each in the checks above.
- Whether leaving out the middle of a conversation leads the model to
  repeat work or contradict decisions made there. The note it is given
  says the middle is missing; how well it copes was not measured.
- The local model's verdicts on harmful actions. One safety check, of a
  harmless command, was replayed.
- Memory. The model ran on LM Studio at `studio.local` at a 107k context;
  what 65,536 tokens costs on the owner's 36 GB Mac was not measured.

## When to stand in (#150)

Added 2026-10-14. Until then, any 429 or 5xx made the local model stand
in. Claude Code retries those itself, so a two-second overload could put a
local model's turn into a conversation for good.

**What the headers say.** The ledger keeps `retry-after`,
`x-should-retry` and the `anthropic-ratelimit-*` headers on every failed
upstream response. Over the 74 failures recorded to 2026-10-14:

- Every session-limit 429 had `anthropic-ratelimit-unified-status:
  rejected` and a `retry-after` equal to the seconds left until the
  5-hour limit reset: 9,720 (2.7 hours) on 2026-10-13, 5,745 on
  2026-10-14. `anthropic-ratelimit-unified-reset` gave the same time.
- `x-should-retry` was `true` on all of them, hours from the reset, so it
  does not tell a long limit from a short one.
- Claude Code's start-up quota check got a 429 with no rate-limit headers.
  It is its own class, and no rule stands in for it.
- No 529 ("overloaded") or 5xx was seen.

**Decision.** Stand in at once when a 429's `retry-after`, or its
unified reset when `rejected`, is a minute or more away. Otherwise relay
the failure to Claude Code to retry, and stand in only when Claude has
failed three times within two minutes with no answer between: an outage,
not a blip. An answer from Claude clears the count.

**What this does not show.** How a 529 or an unreachable Anthropic looks
in practice, since none happened; the three-in-two-minutes threshold rests
on Claude Code's backoff, not a measured outage. Whether a short 429
(seconds from its reset) ever happens on a subscription.
