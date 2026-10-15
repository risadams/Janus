# Installing Janus

This page takes you from nothing to Claude Code talking to Anthropic
through Janus. It covers macOS, Linux, and Windows, setting up LM Studio
for the local-model features, making the Claude Code setting stick,
running Janus in the background, and updating or removing it.

You need Claude Code working already. LM Studio is optional: without it Janus still keeps the
books and runs the guard. [Before you start](before-you-start.md) lists
what each feature needs, and the settings that differ from the defaults.

## 1. Get the binary

Janus is one file with no dependencies. Pick one way to get it.

### With Go

If you have Go 1.27 or later:

```bash
go install github.com/risadams/janus/cmd/janus@latest
```

The binary lands in `$(go env GOPATH)/bin`, usually `~/go/bin`. Make sure
that folder is on your `PATH`, then check:

```bash
janus version
```

### macOS and Linux, from a release

Each release on the
[releases page](https://github.com/risadams/Janus/releases) has an archive
per platform:

| Machine | Archive ends in |
| --- | --- |
| Mac with Apple silicon (M1 and later) | `_darwin_arm64.tar.gz` |
| Mac with an Intel processor | `_darwin_amd64.tar.gz` |
| Linux on x86-64 | `_linux_amd64.tar.gz` |
| Linux on ARM64 | `_linux_arm64.tar.gz` |

These commands download the newest release for a Mac with Apple silicon
and check it. For another machine, change `darwin_arm64` in the first and
last lines.

```bash
url=$(curl -s https://api.github.com/repos/risadams/Janus/releases | grep -m1 -o 'https://[^"]*_darwin_arm64\.tar\.gz')
curl -LO "$url"
curl -LO "${url%/*}/checksums.txt"
shasum -a 256 -c checksums.txt --ignore-missing    # on Linux: sha256sum -c checksums.txt --ignore-missing
tar -xzf "${url##*/}"
cd janus_*_darwin_arm64
```

The checksum line should say `OK`. The newest release may be a release
candidate; Janus has no 1.0 yet.

If you have the [GitHub CLI](https://cli.github.com), you can also check
that the archive was built by this repository's release workflow, from the
tagged commit:

```bash
gh attestation verify "../${url##*/}" --repo risadams/Janus
```

Move the binary somewhere on your `PATH`:

```bash
mkdir -p ~/.local/bin && mv janus ~/.local/bin/
janus version
```

On macOS, a copy downloaded in a browser rather than with `curl` is
quarantined, because the binaries are not signed by Apple. If macOS says
it cannot check the file for malicious software, clear the quarantine:

```bash
xattr -d com.apple.quarantine ~/.local/bin/janus
```

### Windows, from a release

Download the `_windows_amd64.zip` (or `_windows_arm64.zip` on an ARM
machine) and `checksums.txt` from the
[releases page](https://github.com/risadams/Janus/releases). In
PowerShell, in the download folder:

```powershell
Get-FileHash .\janus_*_windows_amd64.zip -Algorithm SHA256
Select-String windows_amd64 .\checksums.txt
```

The two hashes should match (`checksums.txt` is lower case; compare the
characters). Then unpack it and check the binary runs:

```powershell
Expand-Archive .\janus_*_windows_amd64.zip -DestinationPath $HOME\janus
cd $HOME\janus\janus_*_windows_amd64
.\janus.exe version
```

Windows SmartScreen may warn that the file is from an unknown publisher,
since the binaries are not signed. Choose "More info", then "Run anyway".

## 2. Give Janus a folder

Janus keeps its config file, ledger, and captures together. By default it
reads `janus.config.json` from the folder you start it in and writes its
logs to a `logs` folder beside the config. A folder of its own keeps that
tidy:

```bash
mkdir -p ~/janus && cd ~/janus
```

Every `janus` command takes `-config` to name the file instead, so you can
keep it anywhere and run commands from any folder.

## 3. Check, then start

```bash
janus doctor
```

`doctor` checks the config, the port, that Anthropic answers, LM Studio,
and the log folder, and says what to fix. It changes nothing. Before you
set up LM Studio it will warn about it; that is fine for now.

```bash
janus
```

The first run writes `janus.config.json` with the defaults, then prints
where it is listening, where it writes, and the command for the next
step. Leave it running.

## 4. Point Claude Code at Janus

In another terminal:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8787 ENABLE_TOOL_SEARCH=true claude
```

In PowerShell:

```powershell
$env:ANTHROPIC_BASE_URL="http://127.0.0.1:8787"; $env:ENABLE_TOOL_SEARCH="true"; claude
```

Your login is passed through untouched, so there is nothing to sign in
to. Ask Claude anything; Janus prints a line for each request. If it
prints nothing, Claude Code is not using it.

`ENABLE_TOOL_SEARCH=true` matters. When its base URL is not Anthropic's,
Claude Code turns tool search off and sends every tool's definition with
every request. Setting it keeps requests the size they would be without
Janus.

### Making it stick

Typing both variables every time gets old. Two ways to make it
permanent:

**A shell alias** keeps plain `claude` going straight to Anthropic, and
gives you a second command that goes through Janus. In `~/.zshrc` or
`~/.bashrc`:

```bash
alias claude-janus='ANTHROPIC_BASE_URL=http://127.0.0.1:8787 ENABLE_TOOL_SEARCH=true claude'
```

**Claude Code's settings file** sends every session through Janus. In
`~/.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8787",
    "ENABLE_TOOL_SEARCH": "true"
  }
}
```

With the settings file, Claude Code cannot reach Anthropic at all while
Janus is stopped, so either keep Janus running in the background (below)
or remember to remove the setting.

## 5. Set up LM Studio (optional)

Routing, the outage stand-in, the worker tools, and compression all need a
local model. Janus uses [LM Studio](https://lmstudio.ai) for that.

1. Install LM Studio and download a model. Janus is tested with
   `qwen/qwen3.8-27b` at 4-bit, on a Mac with 36 GB of memory, where it
   handles one request at a time comfortably; two at once took it past
   31 GB. Small models (around 4 billion parameters) could not drive
   Claude Code's tools in testing; their answers came back empty or
   rambling.
2. Start LM Studio's local server, from the app. It listens on port 1234.
   To use LM Studio on another machine, turn on serving on the local
   network in its server settings; [Before you
   start](before-you-start.md#lm-studio) has the settings, and how to check
   it can be reached.
3. Tell Janus where it is, in `janus.config.json`:

   ```json
   "local": {
     "baseUrl": "http://127.0.0.1:1234",
     "models": { "janus-local": { "model": "qwen/qwen3.8-27b" } }
   }
   ```

   Always set `baseUrl`. The default, `http://studio.local:1234`, is a
   placeholder: it is the Janus developer's own LM Studio machine. On the
   same machine as Janus use `http://127.0.0.1:1234`; on another, its
   address and port. The `model` must be the id LM Studio shows, exactly;
   `janus doctor` lists the ids it found.
4. Run `janus doctor` again. The `local` line should be `ok` and name the
   model.

You don't have to load the model in LM Studio yourself. When a request
needs it and it isn't loaded, Janus has LM Studio load it, with a
65,536-token context unless you set `contextTokens`.

Nothing goes to the local model until you turn on a feature that uses it.
[Configuring Janus](configure.md) has complete configs for each.

## 6. Run Janus in the background (optional)

Running Janus in a terminal is fine for trying it. To have it start when
you log in, run it as a service. Give it the config file in full, since a
service does not start in your Janus folder.

### macOS: launchd

Save as `~/Library/LaunchAgents/com.github.risadams.janus.plist`, with
your own paths:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.github.risadams.janus</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/you/.local/bin/janus</string>
    <string>-config</string>
    <string>/Users/you/janus/janus.config.json</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/Users/you/janus/janus.out.log</string>
  <key>StandardErrorPath</key><string>/Users/you/janus/janus.log</string>
</dict>
</plist>
```

Then load it:

```bash
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.github.risadams.janus.plist
```

The console lines Janus would print go to `janus.log`; follow them with
`tail -f ~/janus/janus.log`. To stop it, `launchctl bootout
gui/$(id -u)/com.github.risadams.janus`.

### Linux: systemd

Save as `~/.config/systemd/user/janus.service`:

```ini
[Unit]
Description=Janus proxy for Claude Code

[Service]
ExecStart=%h/.local/bin/janus -config %h/janus/janus.config.json
Restart=on-failure

[Install]
WantedBy=default.target
```

Then:

```bash
systemctl --user daemon-reload
systemctl --user enable --now janus
journalctl --user -u janus -f    # the console lines
```

### Windows

Run it from a terminal you keep open, or create a Task Scheduler task
that runs at log on, with `janus.exe` as the program and
`-config C:\Users\you\janus\janus.config.json` as its arguments.

Only one Janus runs per log folder. If a second one starts, it stops and
names the first, so a service and a copy in a terminal cannot both write
the same ledger.

## Updating

Download the new release as in step 1 and replace the binary, or run the
`go install` command again. Then restart Janus. Your config, ledger, and
captures are left as they are. `janus version` says which version you
have, and the console prints it at the top when Janus starts.

## Removing Janus

1. Stop Janus, and remove its service if you made one (`launchctl
   bootout ...`, then delete the plist; or `systemctl --user disable --now
   janus`, then delete the unit file).
2. Remove the setting that points Claude Code at Janus: the alias, or the
   `env` entries in `~/.claude/settings.json`.
3. If you added the worker tools, `claude mcp remove janus`.
4. Delete the binary, and the Janus folder. That folder's `logs` holds
   the captures, which contain your prompts and code in full, and
   `redact.key`, the key for the guard's placeholders.

Next: [configuring Janus](configure.md).
