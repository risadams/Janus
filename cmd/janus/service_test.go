package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHost is a machine with a home in a temporary folder, and launchctl
// and systemctl that only note what they were asked.
func fakeHost(t *testing.T, goos string) (serviceHost, *[]string) {
	t.Helper()
	var ran []string
	return serviceHost{goos: goos, home: t.TempDir(), uid: 501, exe: "/usr/local/bin/janus",
		run: func(name string, args ...string) (string, error) {
			ran = append(ran, name+" "+strings.Join(args, " "))
			switch {
			case name == "launchctl" && args[0] == "print":
				return "state = running", nil
			case name == "systemctl" && args[1] == "is-active":
				return "active", nil
			}
			return "", nil
		}}, &ran
}

// configIn writes a config whose logs go to a folder with a space in its
// name, as macOS's Application Support does.
func configIn(t *testing.T) (path, logs string) {
	t.Helper()
	dir := t.TempDir()
	logs = filepath.Join(dir, "Application Support", "janus")
	path = filepath.Join(dir, "janus.config.json")
	os.WriteFile(path, []byte(`{"logging":{"dir":"`+filepath.ToSlash(logs)+`"}}`), 0o600)
	return path, logs
}

func runSvc(host serviceHost, args ...string) (out string, code int) {
	var stdout, stderr bytes.Buffer
	code = service(args, host, &stdout, &stderr)
	return stdout.String() + stderr.String(), code
}

func TestServiceInstallOnMacOS(t *testing.T) {
	host, ran := fakeHost(t, "darwin")
	cfg, logs := configIn(t)
	out, code := runSvc(host, "install", "-config", cfg)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	plist, err := os.ReadFile(filepath.Join(host.home, "Library", "LaunchAgents", serviceLabel+".plist"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<string>/usr/local/bin/janus</string>", "<string>-config</string>", "<string>" + cfg + "</string>",
		"<key>StandardErrorPath</key><string>" + filepath.Join(logs, "janus.log") + "</string>",
	} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("plist is missing %q:\n%s", want, plist)
		}
	}
	if got := strings.Join(*ran, "\n"); got != "launchctl bootout gui/501/"+serviceLabel+"\nlaunchctl bootstrap gui/501 "+
		filepath.Join(host.home, "Library", "LaunchAgents", serviceLabel+".plist") {
		t.Errorf("commands run:\n%s", got)
	}
	if !strings.Contains(out, "tail -f '"+filepath.Join(logs, "janus.log")+"'") {
		t.Errorf("output does not say how to follow the console, quoted:\n%s", out)
	}

	if out, code := runSvc(host, "status"); code != 0 || !strings.Contains(out, "is running") {
		t.Errorf("status: exit %d: %s", code, out)
	}
	if out, code := runSvc(host, "uninstall"); code != 0 || !strings.Contains(out, "removed") {
		t.Errorf("uninstall: exit %d: %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(host.home, "Library", "LaunchAgents", serviceLabel+".plist")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the plist is still there")
	}
	if out, code := runSvc(host, "status"); code != 1 || !strings.Contains(out, "No janus service") {
		t.Errorf("status after uninstall: exit %d: %s", code, out)
	}
}

func TestServiceInstallOnLinux(t *testing.T) {
	host, ran := fakeHost(t, "linux")
	host.exe = "/home/ris/go/bin/janus"
	cfg, _ := configIn(t)
	if out, code := runSvc(host, "install", "-config", cfg); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	unit, _ := os.ReadFile(filepath.Join(host.home, ".config", "systemd", "user", serviceUnit))
	if want := `ExecStart="/home/ris/go/bin/janus" -config ` + systemdQuote(cfg); !strings.Contains(string(unit), want) {
		t.Errorf("unit is missing %q:\n%s", want, unit)
	}
	if got := strings.Join(*ran, "\n"); got != "systemctl --user daemon-reload\nsystemctl --user enable --now janus.service" {
		t.Errorf("commands run:\n%s", got)
	}
	*ran = nil
	runSvc(host, "uninstall")
	if got := strings.Join(*ran, "\n"); got != "systemctl --user disable --now janus.service\nsystemctl --user daemon-reload" {
		t.Errorf("uninstall ran:\n%s", got)
	}
}

func TestServiceInstallRefusesWhatWouldNotLast(t *testing.T) {
	host, ran := fakeHost(t, "darwin")
	cfg, _ := configIn(t)
	cases := map[string]struct {
		host serviceHost
		args []string
		want string
	}{
		"no config file":      {host, []string{"install", "-config", filepath.Join(t.TempDir(), "none.json")}, "no config file at"},
		"a go run build":      {func() serviceHost { h := host; h.exe = "/tmp/go-build123/b001/exe/janus"; return h }(), []string{"install", "-config", cfg}, "temporary build from go run"},
		"windows":             {func() serviceHost { h := host; h.goos = "windows"; return h }(), []string{"install"}, "not supported on windows"},
		"an unknown command":  {host, []string{"restart"}, `unknown command "restart"`},
		"no command at all":   {host, nil, "Usage: janus service"},
		"a stray argument":    {host, []string{"status", "now"}, `unexpected argument "now"`},
		"a config with flaws": {host, []string{"install", "-config", writeConfig(t, `{"listen":"nonsense"}`)}, "has problems"},
	}
	for name, c := range cases {
		out, code := runSvc(c.host, c.args...)
		if code == 0 || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, want a refusal saying %q:\n%s", name, code, c.want, out)
		}
	}
	if len(*ran) != 0 {
		t.Errorf("a refused install ran %q", *ran)
	}
}

func writeConfig(t *testing.T, body string) string {
	path := filepath.Join(t.TempDir(), "janus.config.json")
	os.WriteFile(path, []byte(body), 0o600)
	return path
}

func TestUnitFilesQuoteAwkwardPaths(t *testing.T) {
	if got := systemdQuote(`/home/a "b"/50%`); got != `"/home/a \"b\"/50%%"` {
		t.Errorf("systemdQuote = %s", got)
	}
	if got := launchdPlist("/a&b/janus", "/c<d>.json", "/l"); !strings.Contains(got, "/a&amp;b/janus") || !strings.Contains(got, "/c&lt;d&gt;.json") {
		t.Errorf("plist does not escape:\n%s", got)
	}
}

// stopFails makes the host's launchctl bootout and systemctl disable fail,
// with launchd and systemd reporting janus as running or not.
func stopFails(host serviceHost, running bool) serviceHost {
	run := host.run
	host.run = func(name string, args ...string) (string, error) {
		switch {
		case name == "launchctl" && args[0] == "bootout", name == "systemctl" && args[1] == "disable":
			out, _ := run(name, args...)
			return out, errors.New("exit status 5")
		case name == "launchctl" && args[0] == "print" && !running:
			return "", errors.New("exit status 113")
		case name == "systemctl" && args[1] == "is-active" && !running:
			return "inactive", errors.New("exit status 3")
		}
		return run(name, args...)
	}
	return host
}

func TestServiceUninstallKeepsTheUnitWhenJanusCannotBeStopped(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		host, _ := fakeHost(t, goos)
		cfg, _ := configIn(t)
		if out, code := runSvc(host, "install", "-config", cfg); code != 0 {
			t.Fatalf("%s: install: exit %d:\n%s", goos, code, out)
		}
		out, code := runSvc(stopFails(host, true), "uninstall")
		if code != 1 || !strings.Contains(out, "still running") || strings.Contains(out, "Stopped janus") {
			t.Errorf("%s: uninstall with janus unstoppable: exit %d: %s", goos, code, out)
		}
		if _, err := os.Stat(unitPath(host)); err != nil {
			t.Errorf("%s: the unit was removed with janus still running: %v", goos, err)
		}
	}
}

func TestServiceUninstallRemovesAUnitThatWasNotLoaded(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		host, _ := fakeHost(t, goos)
		cfg, _ := configIn(t)
		if out, code := runSvc(host, "install", "-config", cfg); code != 0 {
			t.Fatalf("%s: install: exit %d:\n%s", goos, code, out)
		}
		if out, code := runSvc(stopFails(host, false), "uninstall"); code != 0 {
			t.Errorf("%s: uninstall of a service that was not loaded: exit %d: %s", goos, code, out)
		}
		if _, err := os.Stat(unitPath(host)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the unit is still there: %v", goos, err)
		}
	}
}
