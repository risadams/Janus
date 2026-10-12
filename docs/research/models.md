# Spike: which local models can do which jobs here

Issue: #8 · Date: 2026-10-12 · Status: complete for the models available; one candidate measured on this machine

## Question

Which local models fit this machine, how fast are they, and which of
Claude Code's jobs can they do: utility calls, summarising, and driving a
tool loop with Claude Code's real schemas?

## Decision

- **Hardware:** an Apple M5 Max with 36 GB, which is also the machine the
  user works on. LM Studio (Bionic) answers at `studio.local:1234`.
- **One candidate: `qwen/qwen3.8-27b`** (MLX, 4-bit), for subagent
  conversations only, behind `local-then-escalate` with a time limit. #28
  decides whether to turn it on, from real use.
- **Not local:** main-loop turns and compaction. Utility calls do not exist
  in Claude Code's traffic, so there is nothing to route.
- **Charter D2** is resolved on that basis.

## Where each model ran

LM Studio on this Mac reports every model below as loaded locally, but it
served two of them from another machine. The signal is wired memory. It
rose by about 15 to 22 GB when a model loaded here, and stayed flat at about
3.7 GB while the other two were loaded. Checking for open model files
does not work: none showed as open even for a model that was running here.

| Model | Format | Ran on | Wired memory when loaded |
| --- | --- | --- | --- |
| `qwen/qwen3.8-27b` | MLX 4-bit, 16 GB | **this Mac** | 18 GB, growing to 32 GB during a long answer |
| `gemma-3-27b-it-abliterated` | GGUF Q4_K_M, 17 GB | **this Mac** | 25 GB, then 29 to 31 GB as soon as work began |
| `openyourmind-qwen3.6-35b-a3b-…` (MoE, 3B active) | GGUF Q4_K_M, 22 GB | another machine | flat, about 3.7 GB |
| `gemma-4-e4b-it-obliterated` | GGUF Q4_K_M, 6 GB | another machine | flat, about 3.7 GB |

**Only `qwen/qwen3.8-27b` was measured on this hardware.** Figures for the
two remote models describe that machine and its LM Studio setup. Their
failures below may come from how they were served, not from the models.

## Method

Each model was loaded through LM Studio's REST API with a 64k context
(`POST /api/v1/models/load`). LM Studio loads with `parallel: 4`. Then
three jobs:

1. **Speed:** unique filler prompts of about 0.8k, 6k and 18k tokens,
   asking for 256 tokens with reasoning off.
2. **Real tool-loop turns:** `janus replay` on six main-loop requests
   captured from interactive Claude Code 2.1.296 sessions that morning.
   These are the smallest captured that day: 27.7k to 46k tokens of
   context as Claude counts them, 18.6k to 31.3k as the local models do.
   Each has 14 tools.
3. **Summarising:** one of those conversations (20 messages, about 32.7k
   local tokens) with Claude Code's real compaction instruction appended,
   `max_tokens` 8,000.

Each local request was cut off after 7 minutes. Timings on this Mac move
with whatever else it is doing: the same cold 18.6k-token prompt on
`qwen/qwen3.8-27b` took 26 seconds to its first token in one run and 74
in another. Read the times as ranges.

## Speed

| Model | Ran on | Prompt tokens | Time to first token | Reading, tokens/s | Writing, tokens/s |
| --- | --- | --- | --- | --- | --- |
| `qwen/qwen3.8-27b` | this Mac | 813 / 6,063 / 18,063 | 1.5 / 7.8 / 24.3 s | 533 / 780 / 744 | 25.0 / 24.2 / 22.9 |
| 35B-A3B MoE | remote | 771 / 6,021 / 18,021 | 1.2 / 6.3 / 19.1 s | 642 / 963 / 946 | 38.1 / 31.3 / 14.7 |
| `gemma-4-e4b` | remote | 776 / 6,026 / 18,026 | 0.3 / 1.1 / 3.7 s | 2,854 / 5,249 / 4,880 | 122 / 115 / 109 |
| `gemma-3-27b` | this Mac | no result: the request was refused (400), probably over the `reasoning` field, and the model was unloaded before a retry | | | |

## Real tool-loop turns

"Same shape" is `janus replay`'s check: the same stop reason and the same
tools as Claude. It is not a check that the answer is right, so the
answers were also read.

| Model | Ran on | Same shape | Time per turn | What the answers were like |
| --- | --- | --- | --- | --- |
| `qwen/qwen3.8-27b`, run 1 | this Mac | 5 of 6 | 10 to 54 s, median 31 s | Sensible every time. Four commands did what Claude's did in other words. One ran a further reasonable check where Claude summed up. One spent its thinking guessing where the config was before searching for it. |
| `qwen/qwen3.8-27b`, run 2, with `fold_system_messages` | this Mac | 5 of 6 | 29 to 133 s, median 79 s | As sensible. The fold added about 10 tokens per request. The slower times are the machine, not the change (see Method). |
| 35B-A3B MoE, before the fold | remote | 0 of 6 | failed within 0.4 s | `Jinja Exception: System message must be at the beginning.` |
| 35B-A3B MoE, with the fold | remote | 3 of 6 | 13 to 102 s | Usable but looser: two parallel `Bash` calls where Claude made one (twice), a stop where Claude ran a command, and an `AskUserQuestion` where Claude ran a command. |
| `gemma-4-e4b` | remote | 0 of 6 | 0.6 s to the 7-minute limit | Unusable: two empty answers (one token, end of turn), a stop where Claude ran a command, and three runaways that ran to the token limit or the time limit. The one I read was unrelated HTML and CSS. |
| `gemma-3-27b` | this Mac | – | – | Not measured. It took wired memory to 31 GB on the first request, and the memory watchdog unloaded it. |

All of these are far slower than Claude, which answered the same turns in
2 to 8 seconds.

Two things about the local answers matter to Janus beyond quality.
`qwen/qwen3.8-27b` gives tool calls bare numeric ids (`376424142`), and it
emits empty text blocks. The API accepts both in a later upstream turn
(docs/research/integrity.md).

### Chat templates

Claude Code puts `role: "system"` messages in the middle of a
conversation. The MoE's template refused them, and the MLX
`qwen/qwen3.8-27b` accepted them. Janus now folds them into the user
message before them on the way to a local model (`fold_system_messages`,
PR #93). The evidence is one model served remotely, so the fold is a
precaution, not a sign that local models usually fail this way.

## Summarising

| Model | Ran on | Result |
| --- | --- | --- |
| `qwen/qwen3.8-27b`, run 1 | this Mac | 7,596 tokens in 5 min 48 s, 7 s to the first token with the prompt cached. Followed Claude Code's format (`<analysis>`, then the nine-part `<summary>`) and carried the earlier details over correctly. |
| `qwen/qwen3.8-27b`, run 2 | this Mac | Reached the 7-minute limit before finishing. |
| 35B-A3B MoE | remote | Reached the 7-minute limit before finishing. |
| `gemma-4-e4b` | remote | 61 tokens of nonsense in 9 seconds. |

For comparison, Claude compacted a 183-message conversation, five times
the size, into 7,926 tokens in 68 seconds (docs/research/traffic.md,
"Compaction").

## Memory

LM Studio's default `parallel: 4` keeps room for four requests at once,
each up to the loaded context. At a 64k context on this 36 GB Mac:

- `qwen/qwen3.8-27b` took 18 GB when it loaded and 32 GB by the end of a
  long answer.
- `gemma-3-27b` took 25 GB when it loaded and 31 GB on its first request.
- One load of `qwen/qwen3.8-27b` was refused for "insufficient system
  resources" while free memory was down to about 8 GB.

So a 27B model with a large context competes with the user's work for
memory. Janus sends one request at a time per conversation, so loading
with fewer parallel slots or a smaller context should help. That was not
measured here.

## Per job

| Job | Local? | Why |
| --- | --- | --- |
| Utility calls (titles, small classifications) | Nothing to route | Claude Code 2.1.296 sends none, headless or interactive (traffic.md). |
| Subagent conversations | Candidate: `qwen/qwen3.8-27b`, behind `local-then-escalate` | It made sensible tool calls on real Claude Code turns. Earlier, routed through Janus, it finished two small subagent tasks correctly, at 7 to 24 seconds per turn with LM Studio's prompt cache. A subagent never mixes origins in its conversation. #28 is where this gets tried in daily use. |
| Main-loop turns | No | 10 to 130 seconds a turn against Claude's 2 to 8. Each local turn also costs Claude its reasoning on the next turn (integrity.md). |
| Compaction | No | A good summary of a 33k-token conversation took 6 minutes. The one real compaction captured was about 160k tokens, beyond the loaded context and far slower. Upstream, compaction is mostly a cache read. #46 can close on this unless the baseline finds compaction expensive. |

## What local cannot do here

- Keep up with Claude. Median turns were 6 to 15 times slower, and the
  gap grows with prompt size.
- Take Claude Code's long contexts. Interactive turns start at 28k tokens
  and grow past 130k, where the 64k context used here ends.
- Run a 27B model with a large context without taking memory from the
  user's own work.
- Tell, from the replay check alone, whether an answer is right. Matching
  shape is necessary but not enough, so real use behind escalation is the
  test.

## Still open

- `gemma-3-27b` was not measured. It needs a smaller context or fewer
  parallel slots to fit next to the user's work.
- The remote machine's models were measured only as that machine serves
  them.
- Whether lowering `parallel` or the context keeps `qwen/qwen3.8-27b`'s
  memory in check without slowing it down.
