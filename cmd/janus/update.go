// update.go is `janus update`, which replaces this binary with the newest
// release once it has checked the download. It runs only when asked:
// Janus never updates itself.

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/update"
)

// These stand in for the machine, so the tests can update a file of their
// own, check against a key of their own, and restart a pretend service.
var (
	releaseKeys    = update.ReleaseKeys
	executable     = os.Executable
	updateService  = realServiceHost
	updatePlatform = func() (string, string) { return runtime.GOOS, runtime.GOARCH }
)

func runUpdate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("janus update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pre := fs.Bool("pre", false, "count pre-releases too (as update.prerelease does)")
	configPath := fs.String("config", config.DefaultPath, "path to the config file, for update.prerelease")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "janus update: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	fail := func(format string, args ...any) int {
		fmt.Fprintf(stderr, "janus update: "+format+"\n", args...)
		return 1
	}

	current, how := currentVersion()
	have, ok := update.Parse(current)
	if !ok {
		how = devBuild
	}
	if why := updateRefusal(how); why != "" {
		return fail("%s", why)
	}

	cfg, err := config.Read(*configPath)
	if err != nil {
		return fail("%v", err)
	}
	keys, err := releaseKeys()
	if err != nil {
		return fail("the release keys built into janus are unreadable: %v", err)
	}
	exe, err := executable()
	if err != nil {
		return fail("cannot find this binary: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	ctx := context.Background()
	latest, found, err := update.Latest(ctx, client, releasesAPI, "janus/"+current, *pre || cfg.Update.Prerelease)
	switch {
	case err != nil:
		return fail("could not check for a newer release: %v", err)
	case !found:
		fmt.Fprintln(stdout, "No stable release has been published yet. janus update -pre counts pre-releases too.")
		return 0
	case have.Compare(latest.Version) >= 0:
		fmt.Fprintf(stdout, "janus %s is the newest release.\n", current)
		return 0
	}

	goos, goarch := updatePlatform()
	fmt.Fprintf(stdout, "Updating janus %s to %s...\n", current, latest.Tag)
	if err := update.Install(ctx, client, latest, goos, goarch, exe, "janus/"+current, keys); err != nil {
		return fail("%v.\nNothing was changed: %s is still janus %s.", err, exe, current)
	}
	fmt.Fprintf(stdout, "Checked its signature and checksum, and replaced %s: janus %s is now %s.\n", exe, current, latest.Tag)
	restartAfterUpdate(exe, stdout)
	return 0
}

// updateRefusal says why janus update will not replace a copy installed
// this way, or is empty if it will.
func updateRefusal(how install) string {
	switch how {
	case devBuild:
		return "this is a development build, which janus update does not replace. Build it again from source, or install a release."
	case goInstall:
		// Replacing it would leave the module cache and go's idea of what
		// is installed behind; go upgrades it itself.
		return "this janus was installed with go install, which upgrades it too.\n" + upgradeHow(how, update.Release{Tag: "latest"})
	case homebrew:
		// Brew owns the cellar, and would put the old version back.
		return "this janus was installed with Homebrew, which upgrades it too.\n" + upgradeHow(how, update.Release{})
	}
	return ""
}

// restartAfterUpdate restarts the service that janus service installed, if
// it runs exe, so that the new version takes over; otherwise it says to
// restart janus by hand. Either way the old process stops as on Ctrl-C,
// finishing the requests it has in flight.
func restartAfterUpdate(exe string, stdout io.Writer) {
	host, err := updateService()
	if err != nil || !serviceRuns(host, exe) {
		fmt.Fprintln(stdout, "Restart janus to run the new version.")
		return
	}
	var cmd []string
	if host.goos == "darwin" {
		// A stop, not kickstart -k, so launchd sends only SIGTERM and
		// waits; KeepAlive starts it again.
		cmd = []string{"launchctl", "kill", "SIGTERM", launchdTarget(host)}
	} else {
		cmd = []string{"systemctl", "--user", "restart", serviceUnit}
	}
	if out, err := host.run(cmd[0], cmd[1:]...); err != nil {
		fmt.Fprintf(stdout, "Could not restart the janus service (%v %s). Run %s to run the new version.\n", err, out, strings.Join(cmd, " "))
		return
	}
	fmt.Fprintln(stdout, "Restarting the janus service: it finishes the requests in flight, then starts the new version.")
}

// serviceRuns reports whether janus service installed a unit that runs exe.
func serviceRuns(host serviceHost, exe string) bool {
	if host.goos != "darwin" && host.goos != "linux" {
		return false
	}
	unit, err := os.ReadFile(unitPath(host))
	if err != nil {
		return false
	}
	if host.goos == "darwin" {
		return strings.Contains(string(unit), "<string>"+xmlEscape(exe)+"</string>")
	}
	return strings.Contains(string(unit), "ExecStart="+systemdQuote(exe)+" ")
}
