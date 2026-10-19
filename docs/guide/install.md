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

### With Homebrew, on macOS and Linux

From v0.9.0 on, stable releases are in the project's tap:

```bash
brew install risadams/tap/janus
janus version
```

The formula installs the release's own archive, checked against its
`checksums.txt`, so it is the same signed and attested binary as a
download. It installs the tab-completion scripts for bash, zsh, and fish
too (skip the "Tab completion" section below). `brew upgrade janus` brings a new
release. `janus update` leaves a Homebrew copy alone and says to run
that instead, since Homebrew owns the files it installed. Release
candidates are not in the tap.

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

The macOS binaries are signed with the project's Developer ID and
notarized by Apple, so macOS runs them however they were downloaded. The
first run of a copy downloaded in a browser checks with Apple, and needs
the network. To see the signature:

```bash
codesign --verify --strict --verbose=2 ~/.local/bin/janus
codesign --display --verbose=2 ~/.local/bin/janus 2>&1 | grep TeamIdentifier
```

The team is `T5AXV72VU5`.

### Windows, with winget

Stable releases are submitted to winget, from v0.9.0 on; each appears
once Microsoft has reviewed it, which for the first can take some days:

```powershell
winget install RisAdams.Janus
janus version
```

The package installs the release's own archive, checked against its
`checksums.txt`, and puts `janus` on your `PATH` (open a new terminal for
it to take effect). `winget upgrade RisAdams.Janus` brings a new release;
`janus update` leaves a winget copy alone and says to run that instead.

### Windows, with Scoop

From v0.9.0 on, stable releases are in the project's
[Scoop](https://scoop.sh) bucket:

```powershell
scoop bucket add risadams https://github.com/risadams/scoop-bucket
scoop install risadams/janus
janus version
```

The manifest installs the release's own archive, checked against its
`checksums.txt`. `scoop update janus` brings a new release; `janus
update` leaves a Scoop copy alone and says to run that instead. Release
candidates are not in the bucket.

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

By default Janus reads `janus.config.json` from the folder you start it
in, and writes its ledger and captures to a folder per user (macOS `~/Library/Application Support/janus`, Linux `$XDG_STATE_HOME/janus` (or `~/.local/state/janus`), Windows `%LocalAppData%\janus`), so they are in one place whichever folder that is. Give the
config a folder of its own:

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
and the log folder, and says what to fix. It changes nothing. Its last
two lines say when the ledger last recorded a request and whether Claude
Code is pointed at Janus; until you have used Janus, `claude` warns that
it isn't. Before you
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

Typing both variables every time gets old. Three ways to make it
permanent:

**`janus claude`** starts Claude Code through Janus, setting both
variables for that session only. Run it in your Janus folder, or pass
`-config` with the file Janus runs with:

```bash
janus claude                                # instead of claude
janus claude --resume                       # Claude Code's own arguments follow
janus claude -config ~/janus/janus.config.json -- -p "hello"
```

It first checks that Janus is answering on its `listen` address. If it
isn't:

- With the guard on (`features.guard`), it refuses and says to start
  Janus. Going straight to Anthropic would send what the guard keeps back.
- With `-require`, it refuses too.
- Otherwise it says so and starts Claude Code going straight to
  Anthropic, so the session works but is not in the ledger.

Janus's own flags (`-config`, `-require`) come first; the first argument
that isn't one of them, or everything after `--`, goes to Claude Code
unchanged. `janus claude -h` is Janus's help; `janus claude -- -h` is
Claude Code's. Don't combine it with the settings file below: `janus
claude` cannot change what that file sets, so a stopped Janus would still
cut Claude Code off.

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
you log in, and again if it stops, run it as a service. From your Janus
folder:

```bash
janus service install            # or: janus service install -config ~/janus/janus.config.json
janus service status
janus service uninstall
```

`install` writes the unit with the full paths of the janus binary and
the config file, since a service starts in no particular folder, and
starts it. It refuses a config file that does not exist, or has
problems, and a binary from `go run`, which is deleted when it exits:
install janus first. Run `install` again after moving either.
`uninstall` stops the service and removes the unit; if launchd or
systemd cannot stop it, it says so and leaves the unit in place, so the
service can still be stopped by name.

On macOS it writes a launchd agent,
`~/Library/LaunchAgents/com.github.risadams.janus.plist`, and the
console lines Janus would print go to `janus.log` in its log folder; the
command prints how to follow them. On Linux it writes a systemd user
unit, `~/.config/systemd/user/janus.service`, and the console goes to
the journal: `journalctl --user -u janus -f`.

To write one yourself instead, the plist runs the binary with `-config`
and the config's full path, with `RunAtLoad` and `KeepAlive` set, and is
loaded with `launchctl bootstrap gui/$(id -u) <plist>`; the systemd unit
has `ExecStart=<binary> -config <config>` and `Restart=on-failure`, and
is started with `systemctl --user enable --now janus`.

### Windows

Run it from a terminal you keep open, or create a Task Scheduler task
that runs at log on, with `janus.exe` as the program and
`-config C:\Users\you\janus\janus.config.json` as its arguments.

Only one Janus runs per log folder. If a second one starts, it stops and
names the first, so a service and a copy in a terminal cannot both write
the same ledger.

## Tab completion (optional)

`janus completion <shell>` prints a script that completes janus's
commands and flags, for `bash`, `zsh`, `fish`, or `powershell`. It is
made from the janus you run, so a newer janus completes its new flags
once you write the script again.

| Shell | Add this |
| --- | --- |
| bash | `eval "$(janus completion bash)"` in `~/.bashrc` |
| zsh | `janus completion zsh > "${fpath[1]}/_janus"` once, or `source <(janus completion zsh)` in `~/.zshrc` after `compinit` |
| fish | `janus completion fish > ~/.config/fish/completions/janus.fish` once |
| PowerShell | `janus completion powershell \| Out-String \| Invoke-Expression` in your `$PROFILE` |

After a flag that takes a value, such as `-config`, the shell completes
file names.

## Updating

`janus version -check` says whether a newer release is out, and how to
upgrade the copy you have; `janus doctor` says the same in one line. Only
stable releases are offered unless you add `-pre`, or set
`update.prerelease` in the config, to count pre-releases too. The check
asks GitHub for the list of releases and sends nothing else, and only
when you run one of these commands.

To hear about a new release without asking, set `"update": {"check":
true}` in the config. Janus then looks when it starts, at most once a
day, and prints a line on the console if a newer release is out. It
never updates itself.

`janus update` upgrades a copy installed from a release archive. It
downloads the newest release for your machine, checks the signature on
its `checksums.txt` against the key built into the Janus you have and
the archive against those sums, and only then replaces the binary. If
any check fails, it says why and leaves the binary as it was. If `janus
service install` set Janus up to run in the background, `janus update`
restarts it; the old Janus finishes the requests it has in flight, for
up to a minute, before the new one starts. Otherwise, restart Janus
yourself. `-pre` and `update.prerelease` count pre-releases, as they do
for `janus version -check`.

A copy from `go install` or Homebrew is refused, since those upgrade it
themselves: run the `go install` command again, or `brew upgrade janus`.
You can also download the new release as in step 1 and replace the
binary by hand. Your config, ledger, and captures are left as they are. `janus version` says which version you
have, and the console prints it at the top when Janus starts.

## Removing Janus

1. Stop Janus, and remove its service if you made one (`janus service
   uninstall`).
2. Remove the setting that points Claude Code at Janus: the alias, or the
   `env` entries in `~/.claude/settings.json`.
3. If you added the worker tools, `claude mcp remove janus`.
4. Delete the binary, the Janus folder, and the log folder
   (`logging.dir`; `janus doctor` names it). The log folder holds the
   captures, which contain your prompts and code in full, and
   `redact.key`, the key for the guard's placeholders.

Next: [configuring Janus](configure.md).
