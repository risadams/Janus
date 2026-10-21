# Janus user guide

Janus sits between Claude Code and the Anthropic API on your machine. It
keeps a record of every request and what it cost, and can send some of
the work to a local model, keep secrets and private files from leaving,
and keep a session going when Claude is down.

![Where Janus sits: Claude Code sends every request to Janus, which forwards most to Anthropic, sends what its rules choose to LM Studio, and writes a ledger](../diagrams/architecture.svg#gh-light-mode-only)
![Where Janus sits: Claude Code sends every request to Janus, which forwards most to Anthropic, sends what its rules choose to LM Studio, and writes a ledger](../diagrams/architecture-dark.svg#gh-dark-mode-only)

Read these in order the first time:

1. **[Before you start](before-you-start.md).** What to have in place
   first, by feature: Claude Code, LM Studio and a model, and the settings
   in each that differ from their defaults; one machine or two.
2. **[Installing](install.md).** Getting the binary on macOS, Linux, or
   Windows; pointing Claude Code at Janus for good; setting up LM Studio;
   running Janus in the background; updating and removing it.
3. **[Configuring](configure.md).** Where the config file is and how it is
   laid out, how to change it safely, and complete configs for the common
   setups: just the books, subagents on a local model, keeping working
   through an outage, keeping secrets in.
4. **[Using](use.md).** Reading the console, and what `janus stats`,
   `audit`, `route`, `replay`, and `mcp` tell you. What an outage looks
   like.
5. **[Troubleshooting](../TROUBLESHOOTING.md).** First steps when
   something is wrong, then problems by symptom.

In a hurry? The [quick start](../QUICKSTART.md) gets you running in ten
minutes.

## Going further

| To | Read |
| --- | --- |
| Look up any setting | [Configuration reference](../CONFIG.md) |
| Run subagents on a local model, in detail | [Recipe: subagents](../recipes/claude-code.md) |
| Try the local model first, with Claude as the net | [Recipe: local first](../recipes/local-first.md) |
| Keep working when Claude is down | [Recipe: outages](../recipes/outage.md) |
| Choose between Claude, OpenAI, and a local model, and their order | [Recipe: providers](../recipes/providers.md) |
| Move work local before a usage limit runs out | [Recipe: pacing](../recipes/pace.md) |
| Keep secrets and files from leaving | [Recipe: privacy](../recipes/privacy.md), [the guard](../GUARD.md) |
| Hand file reading to the local model | [Recipe: workers](../recipes/workers.md) |
| Understand every figure Janus records | [The ledger](../LEDGER.md) |
| Know exactly what Janus changes in a request | [What Janus changes](../WHAT-JANUS-CHANGES.md) |
| Know what Janus protects against, and what it doesn't | [Threat model](../THREAT-MODEL.md) |
