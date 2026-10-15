# Troubleshooting

## First steps

When something is wrong, these five things find most problems:

1. **`janus doctor`.** It checks the config, the port, Anthropic, LM
   Studio, and the log folder, when the ledger last recorded a request,
   and whether Claude Code is pointed at Janus, and says what to fix.
2. **The console.** Janus prints a line for every request. No lines at
   all means Claude Code is not using Janus. A `↳` note under a line says
   what happened to that request and why.
3. **Who wrote the error.** Errors from Janus itself start with `janus:`
   in Claude Code, and say what to do. Anything else came from Anthropic
   or LM Studio and was passed through as it was.
4. **`janus audit <id>`** for one request: where it went, why, and what
   was changed. The id is in the ledger and the capture's file name; see
   [using Janus](guide/use.md#what-left-your-machine-janus-audit).
5. **`janus route <capture>`** to see which rule a request matched, and
   why.

The problems below are ones met while building and testing Janus.

## By symptom

| Symptom | Where to look |
| --- | --- |
| `janus: command not found`, or the binary won't run | [Installing](#installing) |
| Janus won't start | [Getting started](#getting-started) |
| Janus prints nothing when Claude Code runs | [Getting started](#getting-started) |
| Claude Code can't connect at all | [Installing](#installing) |
| `could not reach LM Studio`, or a model is missing | [Reaching LM Studio](#reaching-lm-studio) |
| Hit the usage limit and the local model didn't take over | [When Claude is down](#when-claude-is-down) |
| Every tool call refused as "temporarily unavailable" | [When Claude is down](#when-claude-is-down) |
| Local answers are empty, wrong, or slow | [Local answers](#local-answers) |
| A `403` or `500` from Janus, or something withheld | [The guard](#the-guard) |
| `janus stats` is empty or costs look wrong | [The books](#the-books) |
| `janus doctor` warns about `ledger` or `claude` | [The books](#the-books) |

## Installing

**`janus: command not found`.** The binary is not in a folder on your
`PATH`. With `go install` it is in `$(go env GOPATH)/bin`, usually
`~/go/bin`; from a release, wherever you moved it. Run it by its full
path, or add the folder to `PATH` in your shell's profile.

**macOS refuses to run the binary.** A copy downloaded with a browser is
quarantined, and the binaries are not signed. Clear it with
`xattr -d com.apple.quarantine janus`, or download with `curl`.

**Windows says it protected your PC.** SmartScreen warns about any
program without a known publisher, and the binaries are not signed.
Choose "More info", then "Run anyway".

**Claude Code can't connect at all.** `ANTHROPIC_BASE_URL` points at
Janus, and Janus isn't running. If you set it in
`~/.claude/settings.json`, every session needs Janus: start it, run it as
a service ([installing](guide/install.md#6-run-janus-in-the-background-optional)),
or remove the setting to go straight to Anthropic.

**The service doesn't start.** `janus service status` says whether it
is running. Run the command it runs, which `janus service install`
printed (`Runs ...`), in a terminal; Janus prints why it stopped. On
macOS its console is in `janus.log` in the log folder. Moving the binary
or the config breaks a service: run `janus service install` again. In a
unit you wrote yourself, a relative `-config` path is the usual cause,
since a service doesn't start in your Janus folder.

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

**The config file has a problem.** Janus names the file and what is
wrong. A JSON mistake or an unknown field is reported on its own, since
the rest can't be read until it is fixed; after that, Janus reports every
remaining problem at once. An unknown field is an error, so a misspelt
setting is caught and not ignored. `janus doctor` reports the same
problems without starting anything.

**`... is janus's own listen address`.** `upstream.baseUrl` or
`local.baseUrl` points at Janus itself: for example, `ANTHROPIC_BASE_URL`
was copied into `JANUS_UPSTREAM_URL` or `-upstream`. Janus would send each
request back to itself without end, so it refuses to start. Set the
upstream to the Anthropic API (`https://api.anthropic.com`) or your
gateway, and `local.baseUrl` to LM Studio.

## Reaching LM Studio

**`could not reach LM Studio at ...`.** Its server is not running, or not
where `local.baseUrl` says. If you never set it, it is still the default,
`http://studio.local:1234`: a placeholder for the Janus developer's own
LM Studio machine, which yours can't reach. Set it to where your LM Studio
runs: `http://127.0.0.1:1234` on the same machine.

**It works on the LM Studio machine and not from another.** LM Studio
serves on loopback only until told otherwise. Turn on serving on the local
network in its server settings. In the Bionic app that setting is also in
`~/.lmstudio/apps/bionic/.internal/http-server-config.json`
(`networkInterface` as `0.0.0.0`, and `port`), edited while the app is
quit.

**`lms server start` hangs with the Bionic app.** The command-line tool
and the app each try to own the server. Start the server from the app.

**A `.local` name stops resolving.** Names like `mymac.local` depend on
mDNS on your network, which routers and VPNs sometimes break. Put the
machine's IP address in `local.baseUrl` instead.

**`the server answered but not like LM Studio`.** Something else is at
that address. Janus checks before trusting it, because LM Studio and other
servers answer unknown paths differently.

**`LM Studio does not have local model "..."`.** The `model` in
`local.models` has to be the id LM Studio shows, exactly. Janus checks
because LM Studio does not: asked for a model it lacks, it answers with
whichever one is loaded. `janus doctor` lists the ids it has.

**The first local request takes a very long time.** The model was not
loaded, so Janus had LM Studio load it first (the ledger's `stages` has
`load_model`). A 27B model takes ten seconds or more. Load it beforehand
to avoid the wait; `doctor` warns when none is.

**`... more than the N-token context Janus loads local model "..."
with`.** The model isn't loaded, and the request is bigger than the
context Janus would load it with: `contextTokens` on its entry, or 65,536
if that is unset. Janus doesn't load it, since the request would fail
anyway; it goes to Claude, or keeps Claude's error when the local model
was standing in. Raise `contextTokens` if the machine has the memory.

**`LM Studio could not load local model ...`.** LM Studio refused the
load, often for want of memory; its reason follows. Unload another model,
or lower `contextTokens`.

## When Claude is down

With an `ifDown` rule, the local model answers when Claude can't. When it
didn't, the console line has a note saying why, starting `no local
stand-in:`. The usual reasons are below.

**`no local stand-in: ... more than the N-token context of local model
...`.** The model is loaded with a context too small for the request, and
Janus doesn't reload a loaded model. Reload it in LM Studio with a larger
context, or unload it and let Janus load it at `contextTokens`.

**`... even its first and latest turns are more than the N tokens local
model ... has room for`.** A stand-in leaves out the middle of a long
conversation to fit, but the first message and the latest turn together
are still too big, usually because of one very large tool result. Claude's
error stands. Load the model with a larger `contextTokens`.

**`no local stand-in: could not reach LM Studio`, or `the local backend
has failed N times in a row`.** LM Studio is down or unreachable; see
[Reaching LM Studio](#reaching-lm-studio). After three failures in a row
Janus skips it for 30 seconds.

**`no local stand-in: ... had 1 requests in flight, its limit, for ...`.**
Another request held the model for the whole of `timeoutSeconds`. Stand-ins
wait their turn, but not forever.

**Claude Code shows an overloaded or rate-limit error and retries, though
the rule has `ifDown`.** That is on purpose when the failure looks brief:
Janus hands it to Claude Code to retry, and stands in only for a usage
limit, or after three failures within two minutes. If it keeps happening
and the local model never answers, the request's class may not be
covered: `ifDown` on a `main` rule
doesn't cover subagents, and Claude Code's background requests (`quota`,
`status`, `safety`) have classes of their own. The ledger's `class`
says which it was.

**At a session limit, every tool call is refused as "temporarily
unavailable", though the local model is answering.** That is auto mode:
Claude Code checks each tool call with Claude, and those checks stay with
Claude. Press Shift+Tab to leave auto mode and approve tool calls
yourself; see CONFIG, "Auto mode while Claude is out".


## Local answers

**The local model often doesn't answer, and you want to know why.** Run
`janus stats`. Its "Why the local model did not answer" table counts each
cause (unreachable, busy, slow, too large, LM Studio errors, rejected
answers) with the next step for each. The console note for a request says
the same, and `janus audit <id>` gives it for one request.
[The ledger](LEDGER.md#why-the-local-model-did-not-answer) lists every
cause.


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

**`janus doctor` says the last request was recorded days ago.** Janus is
set up, but Claude Code has not been sending requests through it: a
session started without `ANTHROPIC_BASE_URL` goes straight to Anthropic
and leaves nothing in the ledger. The `claude` line says what doctor can
see: `ANTHROPIC_BASE_URL` in the shell doctor ran in, and under `env` in
Claude Code's settings file (`~/.claude/settings.json`, or under
`CLAUDE_CONFIG_DIR`). It warns when either points somewhere other than
Janus's `listen` address, and when neither is set while the ledger is
quiet. It cannot see a shell alias, so with an alias and recent requests
it says `ok`. Check too that doctor read the config Janus runs with: the
ledger is under that config's `logging.dir`.

**The ledger is split across two folders.** Before the default log
folder became one per user, it was `logs` beside the config, so starting
Janus from two folders made two ledgers. A config that sets `logging.dir`
keeps it. To move to the per-user folder, remove `logging.dir` from the
config (or set it to the folder `janus doctor` names) and restart Janus;
to read old and new together, `janus stats -dir <old>/ledger -dir
<new>/ledger`.

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
