# Evaluation: digesting large tool results

Issue: #101 · Date: 2026-10-13 · Status: complete

## Question

Does replacing large tool results with a local-model digest (#100) save
enough, without hurting the work, to ship it on?

## Decision

- **Ships off, as an opt-in.** `features.compress` stays false by default.
  On the last full day of ordinary use it would have saved about 1.5% of
  the day's cost and added about two and a half minutes of waiting.
- **Never for files Claude printed to read** (#104). They were most of
  the large Bash results, and Claude needs their exact text.
- **The machinery stays.** The pipeline (#42), the net-savings guard
  (#43), and retrieval through `tool_output` work, and are what any later
  transform builds on. The roadmap's M5 exit allows exactly this: a
  transform "ships disabled with the numbers that explain why".

## Method

1. **Every distinct tool result of 4 KB or more** in the captures of
   2026-10-12 and 2026-10-13 (45: 41 Bash, 4 Read) was digested by
   `qwen/qwen3.8-27b` (MLX, 4-bit) on this Mac, with the production
   prompt, through the opt-in `TestEvaluateOnCaptures`. Wired memory
   peaked at 24 GB; the watchdog never had to unload the model.
2. **The commands behind the 41 Bash results** were sorted by what they
   did.
3. **The saving on 2026-10-12**, the last full day of ordinary use, was
   worked out per result: the tokens cut, written to cache once at
   `claude-opus-5-5`'s 1-hour rate and read on every later turn that
   carried the result, as counted in the captures.
4. **Four short tasks** ran in fresh clones of this repo, each once with
   compression off and once on, through a scratch Janus to the real API,
   with headless Claude Code 2.1.296 (`claude -p`, model
   `claude-opus-5-5`) on the machine's Pro login, and `janus mcp` given
   with `--mcp-config`.

## The digests

| | |
| --- | --- |
| Results digested | 45 of 45, none failed |
| Size sent, digest and note together | median 35% of the original, 62% at worst; 310 kB became 100 kB |
| Time per digest | median 27 s, 90th percentile 47 s, longest 56 s |

Read side by side with their originals, digests of command output were
faithful: one of a 6.7 kB ledger query kept every request id, status, and
timing, and said what it dropped. A rough automatic count of error lines
and paths kept came to 53%, but it scored that digest 3 of 21, so it
measures the counter as much as the digests.

The failure was of a different kind: a digest of Go source, which Claude
had printed with `sed -n` before editing it, described the code instead
of giving it.

## What the large Bash results were

| Kind | Results | Example |
| --- | --- | --- |
| A file printed to read: `cat`, `sed -n`, `head`, `tail` with a path | 28 | `sed -n 150,300p internal/guard/redact.go` |
| Command output: tests, logs, ledger queries, `gh` | 13 | `gh issue view 42 --json body` |

The first kind is what Claude reads in order to edit, and a digest of it
costs a turn to undo. #104 leaves it alone.

## The saving

On 2026-10-12, 14 distinct tool results were 4 kB or more. Of those, 4
were Bash command output, the only kind left to digest. Their median was
70 later turns. Digesting all four would have saved about **$0.24** at
list price, **1.5%** of the day's $15.52, and added about **2.5 minutes**
of waiting, 38 seconds a digest on average.

Large results are fewer than the earlier estimate in #100 suggested ($4.15
for results over 2 kB, all kinds, read and file prints included), for two
reasons:

- **Claude Code already caps what Claude sees.** A 48 kB `git log` came
  back as a 2 kB preview, with the full output saved to a file; a 22.5 kB
  test run came back whole. The cap lies somewhere between.
- **Claude filters its own output.** Asked a precise question, it piped
  through `grep`, redirected to a file and counted, or dropped `-v`, even
  when the prompt named a verbose command.

## Tasks

Costs are as Claude Code reported them, at list price. They vary with
what the cache held from the run before, so read them as rough.

| Task | Compression | Digested | Turns | Time | Cost | Right |
| --- | --- | --- | --- | --- | --- | --- |
| Count passing tests; name the slowest | off | – | 3 | 12 s | $0.27 | yes |
| | on | nothing large enough | 3 | 12 s | $0.19 | yes |
| Fix a planted bug that fails route tests | off | – | 7 | 33 s | $0.22 | yes |
| | on | nothing large enough | 8 | 37 s | $0.24 | yes |
| Most-changed file in 40 commits (a tie) | off | – | 3 | 18 s | $0.40 | yes |
| | on | nothing large enough | 4 | 13 s | $0.21 | yes |
| List tests over 0.05 s from unfiltered verbose output | off | – | 2 | 23 s | $0.27 | yes |
| | on | 22.5 kB, about 7,250 tokens cut | 4 | 47 s | $0.26 | yes |

In the last task, the only one where a digest happened, it took 23
seconds and dropped the per-test times the question needed. Claude said
so, fetched the lines with `tool_output`, and answered correctly. The
retrieval path works; the saving went on the extra turns, and the task
took twice as long.

## What this does not show

- **One day of ordinary use**, from one person, mostly one long session.
  The 2026-10-13 captures are mostly this evaluation's own sessions and
  were left out of the saving.
- **Four tasks**, run once each. They show that a digest does not break
  a task and how retrieval behaves; they do not measure task success.
- **One local model.** A faster model would cut the waiting, not the
  saving.

## Still open

- **Reads of Claude Code's saved outputs.** When Claude Code saves a large
  output to a file, Claude may read the whole file back (53 kB in one run
  here). That is command output arriving through Read, and might be worth
  a digest; it is not distinguished from reading source yet.
- **Where the cost really is.** On 2026-10-12, 40.9 million tokens were
  read from cache, about $8.18 of the day's $15.52: the conversation's
  prefix, read again on every turn. Large tool results are a small part of
  it. Collapsing superseded content in that prefix is stale-context
  pruning (#45), the remaining M5 transform, and the guard (#43) is what
  decides when breaking the cache to do it pays.
