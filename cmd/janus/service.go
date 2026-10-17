// service.go is `janus service`: it writes and loads the unit that runs
// Janus in the background when you log in, which the install guide had
// people write by hand (#243). launchd on macOS, a systemd user unit on
// Linux.

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/risadams/janus/internal/config"
)

// serviceLabel names the launchd job and the systemd unit.
const (
	serviceLabel = "com.github.risadams.janus"
	serviceUnit  = "janus.service"
)

// serviceHost is what janus service needs from the machine, so the tests
// can stand in for launchctl and systemctl.
type serviceHost struct {
	goos string
	home string
	uid  int
	exe  string
	// run runs a command and returns what it printed.
	run func(name string, args ...string) (string, error)
}

func realServiceHost() (serviceHost, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return serviceHost{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return serviceHost{}, err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return serviceHost{goos: runtime.GOOS, home: home, uid: os.Getuid(), exe: exe,
		run: func(name string, args ...string) (string, error) {
			out, err := exec.Command(name, args...).CombinedOutput() // #nosec G204 -- launchctl or systemctl, with arguments janus builds
			return strings.TrimSpace(string(out)), err
		}}, nil
}

// serviceVerbs are what janus service does.
var serviceVerbs = []verb{
	{"install", "write and start the unit that runs janus at log in"},
	{"uninstall", "stop and remove that unit"},
	{"status", "say whether the unit is installed and running"},
}

func runService(args []string, stdout, stderr io.Writer) int {
	host, err := realServiceHost()
	if err != nil {
		fmt.Fprintf(stderr, "janus service: %v\n", err)
		return 1
	}
	return service(args, host, stdout, stderr)
}

func service(args []string, host serviceHost, stdout, stderr io.Writer) int {
	usage := func() {
		fmt.Fprintf(stderr, "Usage: janus service install [-config file] | uninstall | status\n\n"+
			"install writes and starts the unit that runs janus when you log in: a launchd\n"+
			"agent on macOS, a systemd user unit on Linux. uninstall stops and removes it.\n")
	}
	if len(args) == 0 {
		usage()
		return 2
	}
	// The flag set is made before the platform is checked, so that janus
	// completion finds it on every platform.
	verb := args[0]
	fs := newFlagSet("janus service "+verb, stderr)
	configPath := fs.String("config", config.DefaultPath, "path to the config file janus runs with (install only)")
	if host.goos != "darwin" && host.goos != "linux" {
		fmt.Fprintf(stderr, "janus service: not supported on %s yet; docs/guide/install.md says how to run janus at log on\n", host.goos)
		return 1
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "janus service: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	switch verb {
	case "install":
		return serviceInstall(host, *configPath, stdout, stderr)
	case "uninstall":
		return serviceUninstall(host, stdout, stderr)
	case "status":
		return serviceStatus(host, stdout)
	case "-h", "-help", "--help", "help":
		usage()
		return 0
	}
	fmt.Fprintf(stderr, "janus service: unknown command %q\n", verb)
	usage()
	return 2
}

// unitPath is where the unit file goes.
func unitPath(host serviceHost) string {
	if host.goos == "darwin" {
		return filepath.Join(host.home, "Library", "LaunchAgents", serviceLabel+".plist")
	}
	return filepath.Join(host.home, ".config", "systemd", "user", serviceUnit)
}

func serviceInstall(host serviceHost, configPath string, stdout, stderr io.Writer) int {
	// A service starts in no particular folder, so it is given the config
	// file in full, and that file must be the one meant: a config janus
	// would otherwise write afresh in whatever folder the service starts in.
	abs, err := filepath.Abs(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "janus service: %v\n", err)
		return 1
	}
	if _, err := os.Stat(abs); err != nil { // #nosec G703 -- the config file the user named, only looked at
		fmt.Fprintf(stderr, "janus service: no config file at %s. Run janus once there to write one, or pass -config with the file to use.\n", abs)
		return 1
	}
	cfg, err := config.Read(abs)
	if err == nil {
		err = config.Apply(cfg, config.Overrides{}, func(string) string { return "" }).Validate()
	}
	if err != nil {
		fmt.Fprintf(stderr, "janus service: %s has problems:\n%v\n", abs, err)
		return 1
	}
	// A binary built by go run is deleted when it exits.
	if strings.Contains(host.exe, "go-build") {
		fmt.Fprintf(stderr, "janus service: %s is a temporary build from go run. Install janus first (go install ./cmd/janus, or a release binary), then run that one.\n", host.exe)
		return 1
	}

	path := unitPath(host)
	logFile := filepath.Join(cfg.Logging.Dir, "janus.log")
	var unit string
	if host.goos == "darwin" {
		unit = launchdPlist(host.exe, abs, logFile)
	} else {
		unit = systemdUnit(host.exe, abs)
	}
	// ~/Library/LaunchAgents and ~/.config/systemd/user are made as the
	// system makes them, if they are not there already.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { // #nosec G301 -- the system's own folder for user services
		fmt.Fprintf(stderr, "janus service: %v\n", err)
		return 1
	}
	if host.goos == "darwin" {
		// The log folder must exist before launchd opens the log in it.
		if err := os.MkdirAll(cfg.Logging.Dir, 0o700); err != nil {
			fmt.Fprintf(stderr, "janus service: %v\n", err)
			return 1
		}
		// Loading over a loaded job fails, so take any old one down first.
		// This fails when there is none, which is the usual case; if it
		// fails with one loaded, the bootstrap below fails and says so.
		_, _ = host.run("launchctl", "bootout", launchdTarget(host))
	}
	// The unit names the binary and the config file and holds no secret;
	// it is written as launchd and systemd units usually are.
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil { // #nosec G306 G703 -- an ordinary unit file, in the system's folder for them
		fmt.Fprintf(stderr, "janus service: %v\n", err)
		return 1
	}

	var steps [][]string
	follow := ""
	if host.goos == "darwin" {
		steps = [][]string{{"launchctl", "bootstrap", "gui/" + strconv.Itoa(host.uid), path}}
		follow = "tail -f " + shellQuote(logFile)
	} else {
		steps = [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", "--now", serviceUnit}}
		follow = "journalctl --user -u janus -f"
	}
	for _, step := range steps {
		if out, err := host.run(step[0], step[1:]...); err != nil {
			fmt.Fprintf(stderr, "janus service: %s failed: %v\n%s\n", strings.Join(step, " "), err, out)
			return 1
		}
	}
	fmt.Fprintf(stdout, "Wrote %s and started it.\n\n"+
		"  Runs     %s -config %s\n"+
		"  Console  %s\n\n"+
		"It starts again when you log in. janus service status says whether it is\n"+
		"running; janus service uninstall stops and removes it.\n", path, host.exe, abs, follow)
	return 0
}

func serviceUninstall(host serviceHost, stdout, stderr io.Writer) int {
	path := unitPath(host)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stdout, "No janus service is installed (%s does not exist).\n", path)
		return 0
	}
	// Stopping fails harmlessly when the service is not loaded. If it
	// fails with janus still running, the unit is left in place, so that
	// the service can still be stopped by name.
	stop := []string{"systemctl", "--user", "disable", "--now", serviceUnit}
	if host.goos == "darwin" {
		stop = []string{"launchctl", "bootout", launchdTarget(host)}
	}
	if out, err := host.run(stop[0], stop[1:]...); err != nil && serviceRunning(host) {
		fmt.Fprintf(stderr, "janus service: %s failed, and janus is still running, so %s is left in place: %v\n%s\n", strings.Join(stop, " "), path, err, out)
		return 1
	}
	if err := os.Remove(path); err != nil {
		fmt.Fprintf(stderr, "janus service: %v\n", err)
		return 1
	}
	if host.goos == "linux" {
		// The service is already stopped and its unit gone; systemd forgets
		// it at the next reload either way.
		_, _ = host.run("systemctl", "--user", "daemon-reload")
	}
	fmt.Fprintf(stdout, "Stopped janus and removed %s.\n", path)
	return 0
}

func serviceStatus(host serviceHost, stdout io.Writer) int {
	path := unitPath(host)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stdout, "No janus service is installed.\n")
		return 1
	}
	p := newPainter(stdout)
	if serviceRunning(host) {
		fmt.Fprintf(stdout, "janus is %s as a service (%s).\n", p.paint(green, "running"), path)
		return 0
	}
	fmt.Fprintf(stdout, "The janus service is installed (%s) but %s.\n", path, p.paint(yellow, "not running"))
	return 1
}

// serviceRunning is whether launchd or systemd says the janus service is
// running.
func serviceRunning(host serviceHost) bool {
	if host.goos == "darwin" {
		out, err := host.run("launchctl", "print", launchdTarget(host))
		return err == nil && strings.Contains(out, "state = running")
	}
	out, _ := host.run("systemctl", "--user", "is-active", serviceUnit)
	return out == "active"
}

func launchdTarget(host serviceHost) string {
	return "gui/" + strconv.Itoa(host.uid) + "/" + serviceLabel
}

// launchdPlist is the launchd agent: run janus at log in, and again if it
// stops, with its console going to logFile.
func launchdPlist(exe, configPath, logFile string) string {
	x := xmlEscape
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + serviceLabel + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + x(exe) + `</string>
    <string>-config</string>
    <string>` + x(configPath) + `</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ExitTimeOut</key><integer>70</integer>
  <key>StandardOutPath</key><string>` + x(logFile) + `</string>
  <key>StandardErrorPath</key><string>` + x(logFile) + `</string>
</dict>
</plist>
`
}

// systemdUnit is the systemd user unit. Its console goes to the journal.
func systemdUnit(exe, configPath string) string {
	return `[Unit]
Description=Janus proxy for Claude Code

[Service]
ExecStart=` + systemdQuote(exe) + ` -config ` + systemdQuote(configPath) + `
Restart=on-failure

[Install]
WantedBy=default.target
`
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// systemdQuote quotes one argument of ExecStart: double quotes, with
// backslashes and quotes escaped and % doubled, since systemd reads % as
// the start of a specifier.
func systemdQuote(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(s)
	return `"` + s + `"`
}

// shellQuote quotes a path for a command a person will paste.
func shellQuote(s string) string {
	if !strings.ContainsAny(s, " '\"$`\\") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
