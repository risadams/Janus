// doctor.go is `janus doctor`: it checks the setup and says exactly what
// to fix.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/config"
)

// finding is the result of one check. A failure means janus will not work
// as configured; a warning means it will, with something worth knowing.
type finding struct {
	level  string // "ok", "warn", or "fail"
	name   string
	detail string
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("janus doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", config.DefaultPath, "path to the config file")
	var flags config.Overrides
	fs.StringVar(&flags.Listen, "listen", "", "override the listen address (host:port)")
	fs.StringVar(&flags.UpstreamURL, "upstream", "", "override the upstream API base URL")
	fs.StringVar(&flags.LocalURL, "local", "", "override the LM Studio base URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "janus doctor: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	findings, cfg, usable := checkConfig(*configPath, flags)
	if usable {
		client := &http.Client{Timeout: 5 * time.Second}
		findings = append(findings,
			checkListen(cfg.Listen),
			checkUpstream(client, cfg.Upstream.BaseURL),
			checkLogs(cfg.Logging.Dir),
		)
		findings = append(findings, checkLocal(client, cfg)...)
	}

	fmt.Fprintf(stdout, "%s doctor\n\n", versionLine())
	failed := false
	for _, f := range findings {
		failed = failed || f.level == "fail"
		// Continuation lines are indented to sit under the detail.
		detail := strings.ReplaceAll(f.detail, "\n", "\n"+strings.Repeat(" ", 18))
		fmt.Fprintf(stdout, "  %-4s  %-8s  %s\n", f.level, f.name, detail)
	}
	fmt.Fprintln(stdout)

	if failed {
		fmt.Fprintln(stdout, "Something above needs fixing before janus will work.")
		return 1
	}
	fmt.Fprintf(stdout, "Start janus, then point Claude Code at it:\n\n")
	printClaudeCommand(stdout, "http://"+cfg.Listen)
	return 0
}

// checkConfig loads the config the way janus itself would, without writing
// one. usable is false when the config is too broken to check anything
// else against.
func checkConfig(path string, flags config.Overrides) (findings []finding, cfg config.Config, usable bool) {
	cfg, err := config.Read(path)
	if err != nil {
		return []finding{{"fail", "config", err.Error()}}, cfg, false
	}
	cfg = config.Apply(cfg, flags, os.Getenv)
	if err := cfg.Validate(); err != nil {
		return []finding{{"fail", "config", path + " has problems:\n" + err.Error()}}, cfg, false
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return []finding{{"ok", "config", path + " does not exist yet; using the defaults (janus writes it on first run)"}}, cfg, true
	}
	return []finding{{"ok", "config", path}}, cfg, true
}

// addrInUse reports whether a listen failed because the port is taken.
// Windows reports that with a socket error of its own.
func addrInUse(err error) bool {
	const wsaeaddrinuse = syscall.Errno(10048)
	var errno syscall.Errno
	return errors.Is(err, syscall.EADDRINUSE) || (errors.As(err, &errno) && errno == wsaeaddrinuse)
}

func checkListen(addr string) finding {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if addrInUse(err) {
			return finding{"warn", "listen", addr + " is in use. That is expected if janus is already running;\notherwise another program has the port, and listen needs changing."}
		}
		return finding{"fail", "listen", fmt.Sprintf("cannot listen on %s: %v", addr, err)}
	}
	ln.Close()
	return finding{"ok", "listen", addr + " is free"}
}

// checkUpstream asks only whether the upstream answers at all. Any HTTP
// response will do: it sends no credentials, so it cannot tell whether a
// real request would be authorised.
func checkUpstream(client *http.Client, base string) finding {
	start := time.Now()
	resp, err := client.Get(base)
	if err != nil {
		return finding{"fail", "upstream", fmt.Sprintf("%s did not answer: %v", base, err)}
	}
	resp.Body.Close()
	return finding{"ok", "upstream", fmt.Sprintf("%s answered in %s", base, time.Since(start).Round(time.Millisecond))}
}

// checkLocal looks at LM Studio, and at each model a request can be
// routed to. An unreachable LM Studio is a warning while routing is off,
// since janus then runs without it.
func checkLocal(client *http.Client, cfg config.Config) []finding {
	base := cfg.Local.BaseURL
	u, _ := url.Parse(base) // Validate has established that this parses
	models, err := backend.NewLocal(u, client).Models(context.Background())

	level := "warn"
	if cfg.Features.Route {
		level = "fail" // requests for a routed model have nowhere else to go
	}
	switch {
	case errors.Is(err, backend.ErrNotLMStudio):
		return []finding{{level, "local", fmt.Sprintf("%s: %v; is local.baseUrl right?", base, err)}}
	case err != nil:
		return []finding{{level, "local", fmt.Sprintf("%s did not answer: %v\n"+
			"Check that LM Studio is open with its server started. To reach it by a name\n"+
			"other than localhost, its server must be set to serve on the local network.", base, err)}}
	}

	var loaded, all []string
	byID := map[string]backend.Model{}
	for _, m := range models {
		byID[m.ID] = m
		all = append(all, m.ID)
		if m.Loaded {
			loaded = append(loaded, fmt.Sprintf("%s (context %s)", m.ID, commas(m.LoadedContext)))
		}
	}
	var findings []finding
	if len(loaded) == 0 {
		findings = append(findings, finding{"warn", "local", fmt.Sprintf("%s has %d models and none loaded; the first request to one will wait while it loads", base, len(models))})
	} else {
		findings = append(findings, finding{"ok", "local", fmt.Sprintf("%s, loaded: %s", base, strings.Join(loaded, ", "))})
	}
	if !cfg.Features.Route {
		return findings
	}

	names := make([]string, 0, len(cfg.Local.Models))
	for name := range cfg.Local.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		want := cfg.Local.Models[name]
		m, found := byID[want.Model]
		what := fmt.Sprintf("%q goes to %s", name, want.Model)
		switch {
		case !found:
			findings = append(findings, finding{"fail", "route", fmt.Sprintf("%s, which LM Studio does not have.\nIt has: %s", what, strings.Join(all, ", "))})
		case !m.Loaded:
			findings = append(findings, finding{"warn", "route", what + ", which is not loaded; the first request will wait while it loads"})
		case want.ContextTokens > m.LoadedContext:
			findings = append(findings, finding{"warn", "route", fmt.Sprintf("%s: contextTokens is %s but the model is loaded with %s,\nso larger requests will be sent and then fail in LM Studio",
				what, commas(want.ContextTokens), commas(m.LoadedContext))})
		default:
			findings = append(findings, finding{"ok", "route", fmt.Sprintf("%s, loaded with a context of %s tokens", what, commas(m.LoadedContext))})
		}
	}
	return findings
}

// checkLogs confirms the log directory can be written, without creating
// it: it tries a scratch file in the directory, or in the nearest parent
// that exists.
func checkLogs(dir string) finding {
	probe := dir
	for {
		if info, err := os.Stat(probe); err == nil {
			if !info.IsDir() {
				return finding{"fail", "logs", probe + " is a file, so " + dir + " cannot be a directory under it"}
			}
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return finding{"fail", "logs", "no existing directory above " + dir}
		}
		probe = parent
	}

	f, err := os.CreateTemp(probe, ".janus-doctor-*")
	if err != nil {
		return finding{"fail", "logs", fmt.Sprintf("cannot write under %s: %v", probe, err)}
	}
	f.Close()
	os.Remove(f.Name())
	if probe != dir {
		return finding{"ok", "logs", dir + " will be created when janus runs"}
	}
	return finding{"ok", "logs", dir + " is writable"}
}
