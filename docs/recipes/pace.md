# Recipe: send work local before a usage limit runs out

On a subscription, what runs out is not money but the usage limits: one
over five hours, and one over a week. Each answer from Claude says how
much of each is used. A rule with `ifUsageAbove` waits until a limit is
nearly used up, then sends its requests to the local model, so the work
you care least about moves first and the main conversation keeps Claude
for longer.

Without it, the [outage stand-in](outage.md) takes over only once Claude
has refused, and then it takes over everything, the main conversation
included. In the first days of real use the five-hour limit was reached
twice, and the local model answered main turns for an hour and a half.

That is the wrong way round. The main conversation is where the hard
thinking happens, so it is the last thing you want a weaker model doing.
Subagents, which mostly read and search, are the first thing you can
spare. Pacing gives them up early so the main conversation keeps Claude
for longer:

![One five-hour window: Claude answers everything until 80% of the limit is used, then subagents go to the local model while the main conversation stays on Claude; when the limit is reached the local model stands in for the main conversation too, and when the limit resets Claude answers again and Janus notes the hand-back](../diagrams/usage-window.svg#gh-light-mode-only)
![One five-hour window: Claude answers everything until 80% of the limit is used, then subagents go to the local model while the main conversation stays on Claude; when the limit is reached the local model stands in for the main conversation too, and when the limit resets Claude answers again and Janus notes the hand-back](../diagrams/usage-window-dark.svg#gh-dark-mode-only)

The setup below gives you exactly this picture: a pacing rule for
subagents, and an outage stand-in behind it for everything else.

## Set it up

You need a local model configured, as in
[the subagents recipe](claude-code.md#1-tell-janus-about-the-local-model).
Then add a rule before the others:

```json
"routing": [
  {
    "name": "pace-subagents",
    "class": "subagent",
    "route": "local-then-escalate",
    "local": "janus-local",
    "ifUsageAbove": { "5h": 0.8 }
  },
  { "name": "everything", "route": "upstream", "ifDown": "janus-local" }
],
"features": { "route": true }
```

`ifUsageAbove` names a limit by the API's name for its window, `5h` or
`7d`, and the share of it, from 0 to 1, past which the rule matches. With
more than one, any one past its share is enough. Until then the rule does
not match, and the request goes on to the next rule.

`local-then-escalate` is the safe choice: the local model tries, and if
its answer fails the checks, Claude answers after all, which spends some
of the limit but keeps the work right. `local` never asks Claude, which
saves more of the limit and trusts the local model more.

## What it knows, and when

Janus learns where the limits stand from each answer Claude gives, so:

- Until Claude has answered once since Janus started, nothing is known,
  and the rule does not match.
- Once a limit's reset time has passed, its use is unknown again until
  Claude next answers, and the rule does not match: the limit has started
  again from nothing.
- Answers from the local model say nothing about Claude's limits, so
  while the rule is sending everything it matches local, the figure stays
  where Claude left it. With `local-then-escalate`, each answer Claude
  gives on a failed check brings it up to date.

The console says where the limits stand as they fill, and the decision
for each matched request says which limit made the rule match:

```
subagent   qwen3.8-27b (local)     ✓ 14.2s   tool call   18k in · 40% cached     210 out    free
```

with `reason` in the ledger `rule pace-subagents (5h limit 83% used, over
80%): local answer accepted`. `janus stats` shows which kinds of request
use the five-hour limit; start with those that use a large share and that
the local model handles well.

## The trade-off

- **Time.** A local answer is slower than Claude's: in the first tests a
  local subagent turn took 7 to 24 seconds against 1 to 3 for Claude, and
  local-first attempts on 12 to 14 October took 34 seconds at the median.
- **Quality.** It is the local model's work. A threshold of 0.8 keeps
  Claude for most of each window and moves the last fifth; a lower one
  saves more and gives more to the local model.
- **The main conversation.** A rule for `main` with a threshold works,
  but that is the work most worth keeping on Claude. Moving `subagent`
  first, and `status` if you route it, is usually enough to stay under.
