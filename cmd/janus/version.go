// version.go is `janus version`, and the check for a newer release that
// it and `janus doctor` share.

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/update"
)

// releasesAPI is where the check looks for releases. Tests point it at a
// fake.
var releasesAPI = update.DefaultAPI

// install is how this copy of janus was installed, which decides how it
// is upgraded.
type install int

const (
	devBuild  install = iota // a plain go build: nothing to compare
	archive                  // a release archive, unpacked by hand
	goInstall                // go install ...@version
	homebrew                 // brew install
)

// currentVersion is this binary's version and how it was installed. A
// release build has its version set by the linker; one made with `go
// install ...@v1.2.3` takes it from the module; anything else is "dev".
func currentVersion() (string, install) {
	if version != "dev" {
		if exe, err := executable(); err == nil && isHomebrew(exe) {
			return version, homebrew
		}
		return version, archive
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version, goInstall
	}
	return "dev", devBuild
}

// isHomebrew reports whether exe is inside a Homebrew cellar, where brew,
// not janus, decides what is installed.
func isHomebrew(exe string) bool {
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return strings.Contains(filepath.ToSlash(exe), "/Cellar/")
}

// versionLine is what `janus version` prints.
func versionLine() string {
	v, _ := currentVersion()
	return "janus " + v
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("janus version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", false, "ask GitHub whether a newer release is out")
	pre := fs.Bool("pre", false, "with -check, count pre-releases too (as update.prerelease does)")
	configPath := fs.String("config", config.DefaultPath, "path to the config file, for update.prerelease")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "janus version: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	fmt.Fprintln(stdout, versionLine())
	if !*check {
		return 0
	}

	cfg, err := config.Read(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "janus: %v\n", err)
		return 1
	}
	client := &http.Client{Timeout: 10 * time.Second}
	_, msg, err := checkForUpdate(context.Background(), client, *pre || cfg.Update.Prerelease)
	if err != nil {
		fmt.Fprintf(stderr, "janus: could not check for a newer release: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, msg)
	return 0
}

// checkForUpdate asks GitHub for the newest release and says, in a line or
// two, how this binary compares and how to upgrade it. newer is true when
// there is something to upgrade to.
func checkForUpdate(ctx context.Context, client *http.Client, pre bool) (newer bool, msg string, err error) {
	current, how := currentVersion()
	latest, found, err := update.Latest(ctx, client, releasesAPI, "janus/"+current, pre)
	if err != nil {
		return false, "", err
	}
	kind := "release"
	if pre {
		kind = "release or pre-release"
	}
	if !found {
		if pre {
			return false, "No release has been published yet.", nil
		}
		return false, "No stable release has been published yet. janus version -check -pre counts pre-releases too.", nil
	}

	have, ok := update.Parse(current)
	if how == devBuild || !ok {
		return false, fmt.Sprintf("This is a development build, so there is nothing to compare. The newest %s is %s:\n%s", kind, latest.Tag, latest.URL), nil
	}
	if have.Compare(latest.Version) >= 0 {
		return false, fmt.Sprintf("This is the newest %s.", kind), nil
	}
	return true, fmt.Sprintf("%s is out. %s", latest.Tag, upgradeHow(how, latest)), nil
}

// upgradeHow says how to move to r, for the way this copy was installed.
func upgradeHow(how install, r update.Release) string {
	switch how {
	case goInstall:
		return "To upgrade: go install github.com/risadams/janus/cmd/janus@" + r.Tag
	case homebrew:
		return "To upgrade: brew upgrade janus"
	}
	return "To upgrade: janus update, which checks the download first.\nOr download it from " + r.URL + "\nand replace this binary, then restart janus."
}

// noticeUpdate is the notice at startup that a newer release is out. It
// asks only when update.check is on, at most once a day (the answer is
// kept in the log folder), and says nothing unless there is something to
// upgrade to: a check that fails, offline or rate-limited, is not worth a
// line on the console. A development build has nothing to compare, so it
// asks nothing. It is meant to run in the background, so that startup and
// the first request never wait for GitHub.
func noticeUpdate(ctx context.Context, cfg config.Config, client *http.Client, say func(string)) {
	if !cfg.Update.Check {
		return
	}
	current, how := currentVersion()
	have, ok := update.Parse(current)
	if how == devBuild || !ok {
		return
	}
	cache := filepath.Join(cfg.Logging.Dir, update.CacheFile)
	latest, found, err := update.Daily(ctx, client, releasesAPI, "janus/"+current, cfg.Update.Prerelease, cache, time.Now())
	if err != nil || !found || have.Compare(latest.Version) >= 0 {
		return
	}
	say(latest.Tag + " is out; janus version -check says how to upgrade.")
}
