// doctor.go — the `janus doctor` command: check the setup and say exactly
// what to fix.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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
			checkLocal(client, cfg.Local.BaseURL),
			checkLogs(cfg.Logging.Dir),
		)
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

func checkListen(addr string) finding {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
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

// checkLocal looks at LM Studio. Problems here are warnings: janus runs
// without the local model, it just cannot use it.
func checkLocal(client *http.Client, base string) finding {
	resp, err := client.Get(strings.TrimRight(base, "/") + "/api/v0/models")
	if err != nil {
		return finding{"warn", "local", fmt.Sprintf("%s did not answer: %v\n"+
			"Check that LM Studio is open with its server started. To reach it by a name\n"+
			"other than localhost, its server must be set to serve on the local network.", base, err)}
	}
	defer resp.Body.Close()

	// LM Studio answers 200 even for paths it does not know, with an error
	// in the body, so the body is what has to be checked.
	var list struct {
		Data []struct {
			ID            string `json:"id"`
			State         string `json:"state"`
			LoadedContext int    `json:"loaded_context_length"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if json.Unmarshal(body, &list) != nil || list.Data == nil {
		return finding{"warn", "local", fmt.Sprintf("%s answered (HTTP %d) but not like LM Studio; is local.baseUrl right?", base, resp.StatusCode)}
	}

	var loaded []string
	for _, m := range list.Data {
		if m.State == "loaded" {
			loaded = append(loaded, fmt.Sprintf("%s (context %s)", m.ID, commas(m.LoadedContext)))
		}
	}
	if len(loaded) == 0 {
		return finding{"warn", "local", fmt.Sprintf("%s has %d models and none loaded; the first request to one will wait while it loads", base, len(list.Data))}
	}
	return finding{"ok", "local", fmt.Sprintf("%s, loaded: %s", base, strings.Join(loaded, ", "))}
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
