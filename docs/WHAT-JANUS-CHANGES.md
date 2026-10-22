# What Janus changes, and how to check

Janus sits between Claude Code and Anthropic. This page lists every way
it alters what passes through, and how to see for yourself. If something
is not listed here, Janus does not do it.

## Why the list is short

A proxy that reads your traffic is only worth running if you can trust
what it does to it. So Janus starts from doing nothing: everything it
might change is behind a switch, and every change it makes is named in the
ledger and can be checked against a copy of what was sent.

There is a second reason, and it is about money. Each turn of a Claude Code
conversation sends the whole conversation again, and Anthropic reads the
part it has seen before from its prompt cache at a tenth of the input
price. Change one block that was already sent, even by a byte, and that
block and everything after it is written to the cache again, at twice the
input price. On a long conversation that is a dollar or more for a single
turn, more than any rewrite could save.

![Why a block once sent never changes: turns that resend the same bytes are read from the cache, but a change to an earlier block makes it and everything after it a cache write](diagrams/prompt-cache.svg#gh-light-mode-only)
![Why a block once sent never changes: turns that resend the same bytes are read from the cache, but a change to an earlier block makes it and everything after it a cache write](diagrams/prompt-cache-dark.svg#gh-dark-mode-only)

So when Janus does change a request, it decides once, when a block is
first sent, and sends that block in exactly the same bytes on every later
turn. Thinking blocks are never changed at all: Anthropic signs them and
would reject an altered one. The [baseline](research/baseline.md) has
the measurements behind these prices.

## With nothing switched on

A fresh install forwards each request to Anthropic and each response back,
byte for byte, with two exceptions:

- **Hop-by-hop headers** (`Connection`, `Keep-Alive`, and the like) are
  per-connection by definition and are not forwarded. Every proxy does
  this.
- **`Accept-Encoding` on Messages requests is narrowed to gzip**, or
  removed if the client did not offer gzip. Janus reads responses for the
  ledger and can read gzip; it cannot read brotli or zstd with the
  standard library. The client still only ever receives an encoding it
  asked for.

Your credential goes to the upstream untouched and is not stored or
logged. Janus adds no header of its own to what it sends upstream.

A test suite holds Janus to this: `internal/proxy/fidelity_test.go` sends
seventeen kinds of request and response through it and compares bytes.

## When a request goes to the local model

Routing is off until you set `features.route`. When a rule, the alias, or
the override header sends a request to LM Studio, the request is adapted,
and each change is named in the ledger's `stages`:

| Stage | What changes | Why |
| --- | --- | --- |
| `set_model` | `model` becomes the local model's id | LM Studio would otherwise answer with whatever model is loaded |
| `lift_tool_additions` | Tool definitions Claude Code sends inside messages are moved to the `tools` list | LM Studio rejects them where they are |
| `drop_deferred_tools` | Placeholder entries for those tools are removed | They stand for the definitions just moved |
| `fold_system_messages` | System messages in the middle of the conversation become reminders in the user message before them | Many models' chat templates refuse a system message anywhere but first |
| `tool_references_as_text` | The tool names a tool search returns become text in its result, saying whether the tool can be called | LM Studio rejects the `tool_reference` blocks the Anthropic API would expand |
| `leave_out_messages` | For a stand-in on a conversation too long for the local model: messages between the first and the latest turns that fit are left out, and a note in the first message says how many | Otherwise there is no stand-in at all; Claude's copy is untouched |
| `load_model` | Nothing in the request: Janus had LM Studio load the model first, with `contextTokens` or 65,536 tokens | Left to load on demand, LM Studio picks a context Janus can't see |

Also:

- **Your Anthropic credential is not sent to LM Studio.**
- **The answer's `model` is set back** to the name Claude Code asked for,
  so that Claude Code accepts it. This means Claude Code cannot tell a
  local answer from Claude's. Janus's console and ledger can.

## When a request goes to OpenAI

Only a routing rule's `fallback` list, `janus claude -provider openai`,
`janus use codex`, or `X-Janus-Route: openai` sends a request to OpenAI.
There it is not adapted but **translated**: OpenAI is sent a Responses
API request built from Claude Code's Messages request, and Claude Code is
sent a Messages answer built from OpenAI's. That is the largest change
Janus makes. The whole of it is in [the OpenAI research
note](research/openai.md); in short:

| What | Becomes | Why |
| --- | --- | --- |
| Claude Code's headers, its Anthropic credential among them | None of them. The request carries your OpenAI key and a content type, nothing else | OpenAI has no use for them, and your Anthropic credential must not reach it |
| `model` | The OpenAI model chosen for it, by `openai.models` | OpenAI has no Claude models |
| `thinking`, an effort setting, or a thinking budget | `reasoning.effort`, fitted to what the model accepts | The nearest thing OpenAI has |
| The system prompt | A developer message. The billing line Claude Code puts first is left out | The Responses API's form for it |
| Messages, tool calls, and tool results | Input items in OpenAI's form, in the same order | |
| Tool names OpenAI would refuse (dots, slashes, more than 64 characters) | A name of letters, digits, and `_`, made the same way every time, and named back to Claude Code as it was | OpenAI's name pattern |
| Claude's thinking blocks | Left out | Claude's signed thinking means nothing to OpenAI |
| Tools Claude Code marks to load later | Left out until loaded; a tool search's result becomes text saying which tools it found | OpenAI has no deferred tools |
| `cache_control` marks | OpenAI's prompt-cache breakpoints on the same blocks, with a cache key made from a hash of the session id | So OpenAI's cache holds as Claude's does |
| `stop_sequences`, `temperature`, `top_p`, `top_k` | Dropped, and named in the ledger's `request.dropped` | OpenAI's reasoning models take none of them |
| A conversation larger than the model's window | Older turns left out, with a note, at a place that holds from turn to turn; stage `leave_out_messages` | So it fits, without breaking the cache every turn |

Every request is sent with `store: false`, so OpenAI keeps nothing of it
between requests, and with truncation off, so OpenAI never drops the
start of a conversation by itself. The same history always becomes the
same bytes.

On the way back:

- OpenAI's answer, streamed or whole, becomes the Messages answer it
  stands for. Claude Code is told the model it asked for answered; the
  ledger's `response.model` is OpenAI's own.
- OpenAI's reasoning becomes a thinking block with no signature, which
  Janus leaves out when the conversation next goes to Claude (below).
- A tool-call id Claude would refuse is replaced, the same way every
  time.
- OpenAI's errors become Anthropic's, with messages that start `janus:`.
  A refused key is explained without OpenAI's own message, which can
  quote part of the key, and your key is removed from anything OpenAI
  sends back before Janus or Claude Code reads it.

A request that cannot be translated is not sent to OpenAI: a rule's list
passes over OpenAI, saying why, and otherwise the client gets an error.
Anthropic's server tools (web search, web fetch, code execution) and
Files API ids have no OpenAI form, so a request using them never goes.

The guard checks the request before it is translated, so OpenAI is sent
what the guard checked.

## When a conversation has had local turns and goes upstream

| Stage | What changes | Why |
| --- | --- | --- |
| `drop_unsigned_thinking` | Thinking blocks written by the local model, or translated from OpenAI's reasoning, are removed from the history | Anthropic signs its own thinking and rejects a request carrying any without a valid signature |

Thinking that Anthropic signed is never touched.

## With compression on

`features.compress` changes requests going upstream. See
[Compression](CONFIG.md#compression).

| Stage | What changes | Why |
| --- | --- | --- |
| `digest` | A tool result over the size threshold is replaced by a digest the local model wrote, headed `[janus: a local model digested this ... output ...]` with the id that fetches the original | The result is sent again on every later turn of the conversation; the digest is far smaller |

The same result is sent in the same form on every turn, so prompt caching
is unaffected. Errors, images, files Claude printed to read, and content
the local-only policy covers are never digested. The ledger's `transforms` field records each digest
and what it saved.

## With the guard on

`features.guard` alone changes nothing: it looks, and records what it
found. With more:

| Setting | What changes in requests to Anthropic | In the answer |
| --- | --- | --- |
| `guard.redact` | Each secret found is replaced by `JANUS_REDACTED_` and twelve hex digits | Placeholders are turned back into the secrets before Claude Code sees them |
| `guard.localOnly` | Protected content is replaced by a notice saying it was withheld and why | Nothing |

Neither changes what goes to the local model. See [the guard](GUARD.md).

## When Janus answers instead of a backend

Janus writes the response itself, as an error in Anthropic's format with a
message starting `janus:`, when:

- a request was forced local, or sent to an alias, and the local model
  cannot take it;
- the guard could not make a request safe to send, or could not restore an
  answer;
- a request came from a web page rather than a program on this machine.

With `upstream.off`, Janus also answers Claude Code's connectivity checks,
its start-up quota check, and its token counts itself, with nothing sent
anywhere ([no Anthropic account](CONFIG.md#no-anthropic-account)).

## The override header

`X-Janus-Route`, if you send it, is read by Janus and removed. It reaches
no backend. The launchers' `-provider` and `janus local` set it.

## Checking

**One request.** With captures on (the default), each request is saved
under `logs/captures/<day>/`:

| File | Holds |
| --- | --- |
| `<id>_v1-messages.request.txt` | The body exactly as Claude Code sent it |
| `<id>_v1-messages.sent.txt` | The body as Janus sent it on, **only if it differed** |
| `<id>_v1-messages.response.txt` | The response as the client received it |

So `diff` of the first two is everything Janus changed, and no
`.sent.txt` means nothing was.

```bash
janus audit <id>          # where it went, why, and what was changed
janus audit -body <id>    # the exact body that was sent
```

**Everything.** `janus audit` counts requests sent as received, sent
changed (and how), kept local, and stopped. `janus audit -verify` does not
take the ledger's word for it: it re-reads every body that was sent and
looks for secrets and local-only content.

**Before it happens.** `janus route <request file>` reports what the rules
would do with a saved request, and sends nothing.

**The ledger.** Each line of `logs/ledger/<day>.jsonl` has `backend`,
`route`, `reason`, and `stages` for one request. See
[the ledger](LEDGER.md).

**The code.** The changes above are in `internal/backend/local.go`
(`Normalize`, `RepairForUpstream`), `internal/backend/backend.go`
(encoding), and `internal/guard` (redaction and withholding). Janus has no
dependencies outside Go's standard library, so that is all there is to
read.
