# Troubleshooting

Start with `janus doctor`. It checks the config, the port, Anthropic, LM
Studio, and the log directory, and most of what is below shows up there
with what to do about it.

Errors that Janus itself returns start with `janus:` in Claude Code.
Anything else came from Anthropic or LM Studio and was passed through.

The problems here are ones met while building and testing Janus.

## Getting started

**`janus doctor` says the port is in use.** That is expected if Janus is
already running. Otherwise another program has 8787: change `listen` in
`janus.config.json`, and the address in `ANTHROPIC_BASE_URL` to match.

**`another janus ... is already using the log folder`.** Only one Janus
runs per log folder, so two never write the same ledger. The message
gives the running one's pid and address: stop it, or, for a second Janus
on purpose (a scratch one for an experiment), give it its own config with
its own `logging.dir`. The lock is `janus.lock` in the log folder; the
system lets go of it when Janus exits, even after a crash, so there is
nothing to delete by hand.

**Claude Code behaves as if Janus were not there.** `ANTHROPIC_BASE_URL`
has to be set in the environment Claude Code starts from. Janus prints a
line for every request; if it prints nothing, Claude Code is not pointed
at it.

**Claude Code's requests are much larger through Janus.** Set
`ENABLE_TOOL_SEARCH=true` alongside `ANTHROPIC_BASE_URL`. Without it
Claude Code sends every tool definition with every request when its base
URL is not Anthropic's.

**macOS refuses to run the binary.** A copy downloaded with a browser is
quarantined, and the binaries are not signed. Clear it with
`xattr -d com.apple.quarantine janus`, or download with `curl`.

**The config file has a problem.** Janus names the field and what is
wrong with it, and reports every problem at once. An unknown field is an
error, so a misspelt setting is caught and not ignored.

## Reaching LM Studio

**`could not reach LM Studio at ...`.** Its server is not running, or not
where `local.baseUrl` says. The default is `http://studio.local:1234`,
which is for LM Studio on another machine called `studio`; for LM Studio
on the same machine set `http://127.0.0.1:1234`.

**It works on the LM Studio machine and not from another.** LM Studio
serves on loopback only until told otherwise. Turn on serving on the local
network in its server settings. In the Bionic app that setting is also in
`~/.lmstudio/apps/bionic/.internal/http-server-config.json`
(`networkInterface` as `0.0.0.0`, and `port`), edited while the app is
quit.

**`lms server start` hangs with the Bionic app.** The command-line tool
and the app each try to own the server. Start the server from the app.

**`studio.local` stops resolving.** `.local` names depend on mDNS on your
network, which routers and VPNs sometimes break. Put the machine's address
in `local.baseUrl` instead.

**`the server answered but not like LM Studio`.** Something else is at
that address. Janus checks before trusting it, because LM Studio and other
servers answer unknown paths differently.

**`LM Studio does not have local model "..."`.** The `model` in
`local.models` has to be the id LM Studio shows, exactly. Janus checks
because LM Studio does not: asked for a model it lacks, it answers with
whichever one is loaded. `janus doctor` lists the ids it has.

**The first local request takes a very long time.** The model was not
loaded, and LM Studio loaded it on demand. Load it beforehand; `doctor`
warns when none is.

## Local answers

**`this request is about N tokens, more than the N-token context of local
model ...`.** The request does not fit. With a rule it would have gone to
Claude instead; this error means it was sent to an alias or forced local
with `X-Janus-Route`, where there is nowhere else for it to go. Load the
model with a larger context in LM Studio, or route that work with a rule.

**Local answers are empty, or wander.** That is the model. In testing,
`qwen/qwen3.8-27b` drove Claude Code's tools reliably and a 4-billion
parameter model did not: its answers came back empty or as rambling text.
Use `local-then-escalate`, which catches broken answers and asks Claude
instead, and watch the escalation rate in `janus stats`. If most requests
escalate, the model is not up to that work and you are paying for both.

**Subagents are very slow.** A local turn took 7 to 25 seconds in testing
against 1 to 3 for Claude, and a subagent takes many turns. `maxSeconds`
on a `local-then-escalate` rule (default 120) bounds how long one attempt
may run before Claude is asked instead.

**`Invalid signature in thinking block` from Anthropic.** A conversation
answered locally for a turn and then sent upstream carried the local
model's unsigned thinking. Janus removes it on every path upstream
(`drop_unsigned_thinking` in the ledger). If you see this error, please
report it with the output of `janus audit <id>`: it should not happen.

**Local routing stopped and everything is going to Claude.** After three
failures in a row Janus stops sending rule-matched requests to LM Studio
for 30 seconds, so that each request does not wait to find out it is
down. The `reason` in the ledger, and in `janus audit <id>`, says so: "the
local backend has failed 3 times in a row and is being skipped until ...".
It resumes by itself.

**Claude Code's cost figure looks wrong.** It prices local answers as if
Claude gave them. `janus stats` counts them at no upstream cost.

## The guard

**A `500` saying `janus: ... The request was not sent upstream.`** The
guard could not make the request safe to send and stopped it. The message
says why: the request could not be read, was too large to check (over
32 MB), or still held a secret or local-only content after replacement.
`janus audit` lists these under "Stopped by the guard".

**Claude Code says `Server error mid-response` and the turn stops.** If
the ledger record's `guard.blocked` says the model referred to a redacted
secret in a form Janus cannot restore, Claude called a tool with a
placeholder Janus never issued, and Janus stopped the turn before the
tool ran. Ask again. If it keeps happening, `janus audit <id>` shows which
request; the capture beside it holds the answer as far as it got.

**Something that is not a secret keeps being replaced.** Add the exact
value to `guard.allow`. If it is the entropy search finding random-looking
strings, turn `guard.entropy` off; it is off by default for that reason.

**A secret was not caught.** Detection is by known formats and telling
names. Add a pattern for it to `guard.patterns`, then run
`janus audit -verify`, which re-checks what was already sent using the
detectors as they are now.

**Claude says a file was withheld.** The local-only policy matched it.
The console line and `janus audit <id>` name the path and the pattern.

**Claude cannot see part of a search result.** A line that quoted a
local-only file was withheld; the rest of the result was sent.

**A `403` saying `janus: this request was refused because ...`.** The
request did not look like it came from a program on this machine: its
`Host` was not a loopback address, or it carried the `Origin` of a web
page. If you reach Janus through a name of your own, use `127.0.0.1` or
`localhost` in `ANTHROPIC_BASE_URL`.

## Compression

**Nothing is digested.** Only tool results over
`compress.digest.minBytes` (4,000 bytes by default) qualify, from the tools
in `compress.digest.tools` if it is set. Errors, Read results (unless that
list names Read), files printed with Bash to
read, and protected content are left alone, and Claude Code already cuts
very large output down to a preview. When a result that
qualified is not digested, the console says why. The usual reason is
`the request offers no mcp__<server>__tool_output tool`: add `janus mcp` to
Claude Code, as in [the workers recipe](recipes/workers.md), so that the
original can be fetched.

**Turns take longer.** Each digest holds its turn while the local model
writes it, 9 to 56 seconds on the evaluation's Mac. Lower
`compress.digest.maxSeconds`, or switch compression off; on ordinary use
it saved little ([the evaluation](research/digest.md)).

**Claude says a digest left something out.** It can fetch the original
with `tool_output`, by line range or pattern, and usually does. The
original is in `<logging.dir>/originals` for `logging.retainDays`.

## The books

**`janus stats` says there are no requests.** It reads
`<logging.dir>/ledger` from the config it was given, which by default is
`janus.config.json` in the current folder. The first line of the report
is the folder it read. Run it where the config is, or pass the same
`-config` Janus runs with; with no config file it says so and reads the
default folder.

**Costs do not match the bill.** `janus stats` prices tokens at list
price. On a subscription there is no per-token bill; the figure is what
the same usage would cost through the API, which is useful for comparing
before and after and not as an invoice.

**The captures directory is large.** Captures are full requests and
responses. They are pruned to `logging.retainDays` (7) and
`logging.maxMegabytes` (1024); lower either, or set `logging.capture` to
false. The ledger is small and is never pruned.

## Still stuck

`janus audit <id>` for the request that went wrong, and the lines Janus
printed around it, are what to bring to an
[issue](https://github.com/risadams/Janus/issues). Check a capture for
anything private before attaching it: captures hold your prompts and code.
