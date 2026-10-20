# Before you start

What to have in place before installing Janus, and the settings in Claude
Code and LM Studio that differ from their defaults. Each tool's own docs
cover installing it; this page says what Janus needs from it.

## What you need, by feature

| You want | You need |
| --- | --- |
| The books (`janus stats`), the guard, captures | Claude Code, signed in |
| Subagents or other requests on a local model, a stand-in when Claude is down, worker tools, compression | Also LM Studio, a model that can drive Claude Code's tools, and the memory to run it |
| To build Janus yourself | Also Go 1.27 or later. Not needed for a release build |

Start with the first row. Janus is useful on its own, and a week of
`janus stats` tells you what there is to save before you set up a local
model. Setting up a local model is the expensive part, in time and
memory, so it is worth knowing first whether it would pay.

## Claude Code

Install it and sign in as usual, with a subscription or an API key:
[Claude Code setup](https://code.claude.com/docs/en/setup).
Check that `claude` works on its own before putting Janus in front of it.
Janus passes your login through as it is, so there is nothing to sign in
to again, and nothing to change in your account.

Three settings point it at Janus, none of them a default:

| Setting | Why |
| --- | --- |
| `ANTHROPIC_BASE_URL=http://127.0.0.1:8787` | Sends Claude Code's requests to Janus instead of to Anthropic. |
| `ENABLE_TOOL_SEARCH=true` | Claude Code turns tool search off when its base URL isn't Anthropic's, and then sends every tool's definition with every request. This turns it back on. |
| `claude mcp add janus -- ...` | Only for the [worker tools](../recipes/workers.md), which Claude calls to hand reading to the local model. |

`janus claude` sets the first two for one session.
[Installing](install.md#4-point-claude-code-at-janus) shows the other
ways to set them, for one session or for good.

## LM Studio

Only for the local-model features. Janus talks to the LM Studio app
(Bionic) over its HTTP server, using LM Studio's Anthropic-compatible
`/v1/messages` and its model API. Install it from
[lmstudio.ai](https://lmstudio.ai) and see
[its docs](https://lmstudio.ai/docs/app) for anything beyond what is here.

### A model

Download one in the app. Janus is tested with `qwen/qwen3.8-27b` at 4-bit
on a Mac with 36 GB of memory, where one request at a time is comfortable
and two at once took it past 31 GB. Small models, around 4 billion
parameters, could not drive Claude Code's tools in testing: their answers
came back empty or rambling.

You don't have to load the model yourself. Janus asks LM Studio to load it
when a request needs it, with a 65,536-token context unless you set
`contextTokens`.

### Settings that differ from the defaults

| Setting | Where | Why |
| --- | --- | --- |
| The server running | Start it from the app, and have it start when the app opens (`autoStartOnLaunch` in the server config file below), so it is there after a restart. | Janus reaches LM Studio only through its server, on port 1234 unless you change it. |
| Serving on the network | Only when LM Studio is on another machine. Turn on serving on the local network in the server settings, or, with the app quit, set `networkInterface` to `0.0.0.0` in `~/.lmstudio/apps/bionic/.internal/http-server-config.json`. | Until then it listens on loopback only, so other machines can't reach it. |
| No API token required | The server settings, if your version can require one. | Janus sends no credentials to LM Studio. |

Two things to avoid:

- **Don't start the server with `lms server start`** while using the
  Bionic app. The command-line tool tries to wake a separate headless
  service, which takes local inference away from the app and stops its
  server. Start the server from the app.
- **Don't leave `local.baseUrl` at its default.** `http://studio.local:1234`
  is the Janus developer's own LM Studio machine, a placeholder. Set it to
  yours.

## Where things run

Janus always runs beside Claude Code. LM Studio can be on the same
machine or another one, usually because a local model needs more memory
than a laptop has.

![One machine or two: Janus always beside Claude Code; LM Studio either on the same machine on loopback, or on another machine with more memory, reached over the network](../diagrams/machines.svg#gh-light-mode-only)
![One machine or two: Janus always beside Claude Code; LM Studio either on the same machine on loopback, or on another machine with more memory, reached over the network](../diagrams/machines-dark.svg#gh-dark-mode-only)

**One machine.** Claude Code, Janus, and LM Studio all on your computer.
`local.baseUrl` is `http://127.0.0.1:1234`, and nothing is exposed to the
network.

**LM Studio on another machine,** such as a desktop with more memory.
Claude Code and Janus stay on your computer; only LM Studio is on the
other. On that machine:

1. Turn on serving on the network, as above.
2. If it has a firewall, allow incoming connections to LM Studio. With
   the macOS firewall on, macOS asks the first time the server listens on
   the network.
3. Find its name or address. On a Mac, the local hostname is in System
   Settings, General, Sharing, and ends in `.local`. A `.local` name
   depends on mDNS, which some routers and VPNs break; an IP address
   doesn't.

Then, from your computer, check that you can reach it before involving
Janus:

```bash
curl http://mymac.local:1234/v1/models
```

It lists the models LM Studio has. Put the same address in `local.baseUrl`.

Janus itself always listens on loopback (`127.0.0.1:8787`), on the same
machine as Claude Code. It forwards whatever credentials a client sends,
so it refuses to listen on the network unless `allowRemote` says so.

## Then

[Install Janus](install.md), and run `janus doctor`. It checks the config,
the port, that Anthropic is reachable, that LM Studio answers and has your
model, and the log folder, and says what to fix.
