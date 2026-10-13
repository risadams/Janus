# What Janus changes, and how to check

Janus sits between Claude Code and Anthropic. This page lists every way
it alters what passes through, and how to see for yourself. If something
is not listed here, Janus does not do it.

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

Also:

- **Your Anthropic credential is not sent to LM Studio.**
- **The answer's `model` is set back** to the name Claude Code asked for,
  so that Claude Code accepts it. This means Claude Code cannot tell a
  local answer from Claude's. Janus's console and ledger can.

## When a conversation has had local turns and goes upstream

| Stage | What changes | Why |
| --- | --- | --- |
| `drop_unsigned_thinking` | Thinking blocks written by the local model are removed from the history | Anthropic signs its own thinking and rejects a request carrying any without a valid signature |

Thinking that Anthropic signed is never touched.

## With compression on

`features.compress` changes requests going upstream. See
[Compression](CONFIG.md#compression).

| Stage | What changes | Why |
| --- | --- | --- |
| `digest` | A tool result over the size threshold is replaced by a digest the local model wrote, headed `[janus: a local model digested this ... output ...]` with the id that fetches the original | The result is sent again on every later turn of the conversation; the digest is far smaller |

The same result is sent in the same form on every turn, so prompt caching
is unaffected. Errors, images, and content the local-only policy covers
are never digested. The ledger's `transforms` field records each digest
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

## The override header

`X-Janus-Route`, if you send it, is read by Janus and removed. It reaches
neither backend.

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
