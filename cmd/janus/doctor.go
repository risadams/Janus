// doctor.go is `janus doctor`: it checks the setup and says exactly what
// to fix.

package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/instance"
	"github.com/risadams/janus/internal/ledger"
	"github.com/risadams/janus/internal/openai"
	"github.com/risadams/janus/internal/route"
	"github.com/risadams/janus/internal/update"
)

// finding is the result of one check. A failure means janus will not work
// as configured; a warning means it will, with something worth knowing.
type finding struct {
	level  string // "ok", "warn", or "fail"
	name   string
	detail string
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("janus doctor", stderr)
	configPath := fs.String("config", config.DefaultPath, "path to the config file")
	var flags config.Overrides
	fs.StringVar(&flags.Listen, "listen", "", "override the listen address (host:port)")
	fs.StringVar(&flags.UpstreamURL, "upstream", "", "override the upstream API base URL")
	fs.StringVar(&flags.LocalURL, "local", "", "override the LM Studio base URL")
	asJSON := fs.Bool("json", false, "print the results as JSON")
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
		listenCheck, elsewhere := checkListen(cfg.Listen, cfg.Logging.Dir)
		findings = append(findings,
			listenCheck,
			checkUpstreamOrOff(client, cfg),
			checkLogs(cfg.Logging.Dir),
		)
		findings = append(findings, checkLocal(client, cfg)...)
		if cfg.OpenAI.On() {
			findings = append(findings, checkOpenAI(client, cfg.OpenAI, os.LookupEnv, namesOpenAI(cfg)))
		}
		if f, ok := checkProviders(cfg); ok {
			findings = append(findings, f)
		}
		ledgerCheck, seen := checkLedger(filepath.Join(cfg.Logging.Dir, "ledger"), time.Now(), elsewhere)
		findings = append(findings, ledgerCheck, checkClaude(cfg.Listen, os.Getenv, claudeSettingsPath(os.Getenv), seen))
		if f, seen := checkClaudeVersion(filepath.Join(cfg.Logging.Dir, "ledger"), time.Now()); seen {
			findings = append(findings, f)
		}
		findings = append(findings, checkUpdate(client, cfg.Update.Prerelease))
	}

	failed := false
	for _, f := range findings {
		failed = failed || f.level == "fail"
	}
	if *asJSON {
		if err := printDoctorJSON(stdout, findings, failed); err != nil {
			fmt.Fprintf(stderr, "janus doctor: %v\n", err)
			return 1
		}
		if failed {
			return 1
		}
		return 0
	}

	p := newPainter(stdout)
	fmt.Fprintf(stdout, "%s\n\n", p.paint(bold, versionLine()+" doctor"))
	for _, f := range findings {
		// Continuation lines are indented to sit under the detail.
		detail := strings.ReplaceAll(f.detail, "\n", "\n"+strings.Repeat(" ", 18))
		fmt.Fprintf(stdout, "  %s  %-8s  %s\n", p.level(f.level, pad(f.level, 4)), f.name, detail)
	}
	fmt.Fprintln(stdout)

	if failed {
		fmt.Fprintln(stdout, p.paint(red, "Something above needs fixing before janus will work."))
		return 1
	}
	fmt.Fprintf(stdout, "Start janus, then point Claude Code at it:\n\n")
	printClaudeCommand(stdout, "http://"+cfg.Listen, *configPath, cfg.Upstream.Off)
	return 0
}

// doctorReport is what janus doctor -json prints. Its fields are kept
// within a major version (docs/RELEASES.md); docs/TROUBLESHOOTING.md
// describes them.
type doctorReport struct {
	Version string        `json:"version"`
	OK      bool          `json:"ok"`
	Checks  []doctorCheck `json:"checks"`
}

type doctorCheck struct {
	Name   string `json:"name"`
	Level  string `json:"level"`
	Detail string `json:"detail"`
}

func printDoctorJSON(w io.Writer, findings []finding, failed bool) error {
	v, _ := currentVersion()
	r := doctorReport{Version: v, OK: !failed, Checks: make([]doctorCheck, len(findings))}
	for i, f := range findings {
		r.Checks[i] = doctorCheck{Name: f.name, Level: f.level, Detail: f.detail}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// checkConfig loads the config the way janus itself would, without writing
// one. usable is false when the config is too broken to check anything
// else against.
//
// The config is named by its full path. Without -config it is the one in
// the folder doctor is run from, which need not be the one janus runs
// with, and a relative logging.dir is read beside it.
func checkConfig(path string, flags config.Overrides) (findings []finding, cfg config.Config, usable bool) {
	cfg, err := config.Read(path)
	if err != nil {
		return []finding{{"fail", "config", err.Error()}}, cfg, false
	}
	shown := fullPath(path)
	cfg = config.Apply(cfg, flags, os.Getenv)
	if err := cfg.Validate(); err != nil {
		return []finding{{"fail", "config", shown + " has problems:\n" + err.Error()}}, cfg, false
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return []finding{{"ok", "config", shown + " does not exist yet; using the defaults (janus writes it on first run)"}}, cfg, true
	}
	if len(cfg.Upgraded) > 0 {
		return []finding{{"warn", "config", upgradedNote(shown, cfg.Upgraded)}}, cfg, true
	}
	return []finding{{"ok", "config", shown}}, cfg, true
}

// fullPath is path made absolute, or path itself if that cannot be done.
func fullPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// addrInUse reports whether a listen failed because the port is taken.
// Windows reports that with a socket error of its own.
func addrInUse(err error) bool {
	const wsaeaddrinuse = syscall.Errno(10048)
	var errno syscall.Errno
	return errors.Is(err, syscall.EADDRINUSE) || (errors.As(err, &errno) && errno == wsaeaddrinuse)
}

// checkListen says whether janus could listen on addr. When something
// already has the port, the lock on the config's log folder tells whether
// that is a janus using this config. If nothing holds the lock, any janus
// on the port was started with another config and writes another ledger,
// and elsewhere is true: the checks that read the logs are then about a
// folder nobody is writing (#365).
func checkListen(addr, logDir string) (f finding, elsewhere bool) {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		_ = ln.Close() // only opened to see that it could be
		return finding{"ok", "listen", addr + " is free"}, false
	}
	if !addrInUse(err) {
		return finding{"fail", "listen", fmt.Sprintf("cannot listen on %s: %v", addr, err)}, false
	}
	held, holder, err := instance.Held(logDir)
	switch {
	case err != nil:
		return finding{"warn", "listen", addr + " is in use. That is expected if janus is already running;\n" +
			"otherwise another program has the port, and listen needs changing.\n" +
			"Whether it is a janus using this config is unknown: " + err.Error()}, false
	case held && holder != "":
		return finding{"ok", "listen", addr + " is in use by the janus using this config (" + holder + ")"}, false
	case held:
		return finding{"ok", "listen", addr + " is in use by the janus using this config"}, false
	}
	return finding{"warn", "listen", addr + " is in use, but not by a janus using this config: none holds the lock on its\n" +
		"log folder, " + logDir + ". If janus is running, it was started with another config\n" +
		"and writes another ledger; run janus doctor -config with the config it was started with.\n" +
		"Otherwise another program has the port, and listen needs changing."}, true
}

// checkUpstream asks only whether the upstream answers at all. Any HTTP
// response will do: it sends no credentials, so it cannot tell whether a
// real request would be authorised.
// checkUpstreamOrOff checks the upstream, unless there is none.
func checkUpstreamOrOff(client *http.Client, cfg config.Config) finding {
	if cfg.Upstream.Off {
		return finding{"ok", "upstream", "off: nothing is sent to Anthropic, and Claude Code is given a placeholder login"}
	}
	return checkUpstream(client, cfg.Upstream.BaseURL)
}

func checkUpstream(client *http.Client, base string) finding {
	start := time.Now()
	u, _ := url.Parse(base) // the config has been validated
	resp, err := client.Get(base)
	if err != nil {
		return finding{"fail", "upstream", fmt.Sprintf("%s did not answer: %s", base, diagnoseUpstream(u, err))}
	}
	_ = resp.Body.Close() // any answer will do; its body is not read
	return finding{"ok", "upstream", fmt.Sprintf("%s answered in %s", base, time.Since(start).Round(time.Millisecond))}
}

// diagnoseUpstream turns the raw error from an upstream GET into the part
// of the URL that failed and what to do about it, rather than a stack of
// socket errors.
func diagnoseUpstream(u *url.URL, err error) string {
	if u == nil {
		return err.Error() // an unparseable URL reports itself well enough
	}
	host := u.Host
	// Unwrap down to the cause the network layer reports.
	var dns *net.DNSError
	var op *net.OpError
	var unknown x509.UnknownAuthorityError
	var cert *x509.CertificateInvalidError
	switch {
	case errors.As(err, &dns) && dns.IsNotFound:
		return fmt.Sprintf("no such host %q. Check the hostname in upstream.baseUrl", host)
	case errors.As(err, &dns) && dns.IsTemporary:
		return fmt.Sprintf("a temporary failure resolving %q. Check the network, or the DNS the machine uses", host)
	case errors.As(err, &unknown):
		return fmt.Sprintf("TLS to %q was not trusted. Check that upstream.baseUrl uses https, or the certificate if you run it behind a proxy", host)
	case errors.As(err, &cert):
		return fmt.Sprintf("the TLS certificate for %q is invalid: %s", host, cert.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return "it did not answer in time. A firewall may be dropping the connection, or the upstream is overloaded"
	case errors.As(err, &op) && isRefused(op.Err):
		return fmt.Sprintf("nothing is listening on %q. Check the port in upstream.baseUrl", host)
	default:
		return err.Error()
	}
}

// isRefused reports whether a network error is a connection refused.
// Windows and the other platforms word it differently.
func isRefused(err error) bool {
	var errno syscall.Errno
	return errors.Is(err, syscall.ECONNREFUSED) || (errors.As(err, &errno) && errno == syscall.ECONNREFUSED)
}

// checkOpenAI says where the OpenAI key comes from and whether it was
// found, never what it is (ADR-005). It sends nothing to OpenAI: there is
// no account to test against yet (charter D7), and this version sends it
// no requests. A missing key is a warning for the same reason.
//
// It asks whether the API answers at all, sending no key, and reports
// whether the key is where openai.key says, without its value. A missing
// key fails when a rule sends requests to OpenAI, since janus then will
// not start.
func checkOpenAI(client *http.Client, o config.OpenAI, lookupEnv func(string) (string, bool), used bool) finding {
	reach := ""
	if b, err := openai.NewBackend(o, openai.Key{}, client); err != nil {
		reach = err.Error()
	} else if err := b.Health(context.Background()); err != nil {
		reach = fmt.Sprintf("%s did not answer: %v", o.URL(), err)
	}
	_, keyErr := openai.LoadKey(o, lookupEnv)
	switch {
	case keyErr != nil && used:
		return finding{"fail", "openai", keyErr.Error() + ". A routing rule names openai, so janus will not start without it."}
	case keyErr != nil:
		return finding{"warn", "openai", keyErr.Error()}
	case reach != "":
		return finding{"warn", "openai", reach}
	}
	kind, name := o.KeySource()
	where := name
	if kind == "keychain" {
		where = "the credential store entry " + name
	}
	return finding{"ok", "openai", fmt.Sprintf("%s answers, key found in %s", o.URL(), where)}
}

// checkProviders says, with routing on, which providers the rules send
// requests to, in each rule's order, so that a list that names no one
// expected is seen before a session runs on it.
func checkProviders(cfg config.Config) (finding, bool) {
	if !cfg.Features.Route || len(cfg.Routing) == 0 {
		return finding{}, false
	}
	var used []string
	add := func(name string) {
		if !slices.Contains(used, name) {
			used = append(used, name)
		}
	}
	for _, r := range cfg.Routing {
		for _, name := range r.Fallback {
			add(describeProvider(cfg, name))
		}
		if r.Route != "" {
			add("upstream")
		}
		if r.Local != "" {
			add(describeProvider(cfg, r.Local))
		}
	}
	if !cfg.Upstream.Off {
		add("upstream") // what no rule matches goes there
	}
	detail := "rules send requests to " + strings.Join(used, ", ")
	if cfg.Upstream.Off {
		detail += "; upstream is off, so nothing goes to Anthropic"
	}
	return finding{"ok", "providers", detail}, true
}

// checkLocal looks at LM Studio, at each model a request can be routed
// to, and, with compression on, at the model that writes digests. An
// unreachable LM Studio is a warning while routing is off, since janus then
// runs without it: with compression alone, results are just sent
// undigested.
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
	if cfg.Features.Compress {
		name, want, _ := cfg.DigestModel() // Validate has established that it exists
		m, found := byID[want.Model]
		what := fmt.Sprintf("digests are written by %q, %s", name, want.Model)
		switch {
		case !found:
			findings = append(findings, finding{"fail", "compress", fmt.Sprintf("%s, which LM Studio does not have.\nIt has: %s", what, strings.Join(all, ", "))})
		case !m.Loaded:
			findings = append(findings, finding{"warn", "compress", what + ", which is not loaded; the first digest will wait while it loads"})
		default:
			findings = append(findings, finding{"ok", "compress", what + ", loaded"})
		}
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
	// Only made to see that it could be; an empty probe file left behind
	// is harmless.
	_ = f.Close()
	_ = os.Remove(f.Name())
	if probe != dir {
		return finding{"ok", "logs", dir + " will be created when janus runs"}
	}
	return finding{"ok", "logs", dir + " is writable"}
}

// quietAfter is how long the ledger can go without a request before doctor
// asks whether Claude Code is still using janus.
const quietAfter = 24 * time.Hour

// traffic is what the ledger says about whether Claude Code goes through
// janus.
type traffic int

const (
	// trafficQuiet is a ledger with nothing recorded within quietAfter.
	trafficQuiet traffic = iota
	// trafficRecent is a ledger with a request recorded within quietAfter.
	trafficRecent
	// trafficUnknown is a ledger the running janus does not write, which
	// says nothing either way.
	trafficUnknown
)

// checkLedger says when janus last recorded a request. A setup that works
// but that Claude Code no longer goes through looks healthy in every other
// check, so a quiet ledger is the sign of it (#231). elsewhere says that
// the janus running now writes another ledger, so that a quiet one here
// is no sign of anything (#365).
func checkLedger(dir string, now time.Time, elsewhere bool) (f finding, seen traffic) {
	newest, err := ledger.Newest(dir)
	switch {
	case err != nil:
		return finding{"warn", "ledger", fmt.Sprintf("cannot read %s: %v", dir, err)}, trafficQuiet
	case elsewhere && newest.IsZero():
		return finding{"ok", "ledger", "nothing recorded in " + dir + ",\nwhich the janus running now does not write; see listen above"}, trafficUnknown
	case newest.IsZero():
		return finding{"ok", "ledger", "nothing recorded yet in " + dir}, trafficQuiet
	}
	when := fmt.Sprintf("%s (%s)", newest.Local().Format("2006-01-02 15:04"), ago(now.Sub(newest)))
	switch {
	case elsewhere:
		return finding{"ok", "ledger", "the last request in " + dir + " was recorded " + when + ",\n" +
			"but the janus running now does not write there; see listen above"}, trafficUnknown
	case now.Sub(newest) > quietAfter:
		return finding{"warn", "ledger", "the last request was recorded " + when + ".\n" +
			"If Claude Code has been used since, it is not going through janus; see claude below."}, trafficQuiet
	}
	return finding{"ok", "ledger", "the last request was recorded " + when}, trafficRecent
}

// versionLookback is how far back doctor looks in the ledger for the
// Claude Code release in use.
const versionLookback = 7 * 24 * time.Hour

// checkClaudeVersion says whether the Claude Code release in the newest
// request janus recorded is one janus was tested with. A newer one may
// send requests janus classifies wrongly, without anything failing (#267),
// so it is a warning, never a failure. seen is false when no request in
// the last week named its release, and there is nothing to say.
func checkClaudeVersion(dir string, now time.Time) (f finding, seen bool) {
	records, _, err := ledger.Read(dir, now.Add(-versionLookback), time.Time{})
	if err != nil {
		return finding{}, false
	}
	var used string
	for i := len(records) - 1; i >= 0 && used == ""; i-- {
		used = records[i].ClaudeCode
	}
	v, ok := update.Parse(used)
	if !ok {
		return finding{}, false
	}
	tested := route.TestedClaudeCode
	oldest, _ := update.Parse(tested.Oldest)
	newest, _ := update.Parse(tested.Newest)
	switch {
	case v.Compare(newest) > 0:
		return finding{"warn", "client", fmt.Sprintf("Claude Code %s is newer than any this janus was tested with (up to %s).\n"+
			"It will most likely work. If janus stats counts requests as unknown, or routing or the guard\n"+
			"misbehave, run janus update; if the newest janus is no newer, report it at\n"+
			"https://github.com/risadams/Janus/issues.", used, tested.Newest)}, true
	case v.Compare(oldest) < 0:
		return finding{"warn", "client", fmt.Sprintf("Claude Code %s is older than any this janus was tested with (from %s).\n"+
			"Update Claude Code with claude update.", used, tested.Oldest)}, true
	}
	return finding{"ok", "client", fmt.Sprintf("Claude Code %s, which janus was tested with (%s to %s)", used, tested.Oldest, tested.Newest)}, true
}

// ago writes a duration the way a person would say it.
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

// claudeSettingsPath is Claude Code's user settings file, under
// CLAUDE_CONFIG_DIR when that is set and ~/.claude otherwise.
func claudeSettingsPath(getenv func(string) string) string {
	dir := getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".claude")
	}
	return filepath.Join(dir, "settings.json")
}

// checkClaude says whether Claude Code is pointed at janus, by
// ANTHROPIC_BASE_URL in doctor's own environment or in Claude Code's user
// settings. Neither is the only way: a shell alias, or a variable set only
// where Claude Code is started, cannot be seen from here. So an unset
// variable is a warning only when the ledger is quiet too; one that points
// somewhere else always is.
func checkClaude(listen string, getenv func(string) string, settingsPath string, seen traffic) finding {
	type source struct{ where, value string }
	var set []source
	if v := getenv("ANTHROPIC_BASE_URL"); v != "" {
		set = append(set, source{"ANTHROPIC_BASE_URL in this shell", v})
	}
	if settingsPath != "" {
		var settings struct {
			Env map[string]string `json:"env"`
		}
		data, err := os.ReadFile(settingsPath) // #nosec G304 -- Claude Code's own settings file
		switch {
		case err == nil && json.Unmarshal(data, &settings) != nil:
			return finding{"warn", "claude", settingsPath + " is not valid JSON, so whether it points Claude Code at janus is unknown"}
		case err == nil && settings.Env["ANTHROPIC_BASE_URL"] != "":
			set = append(set, source{"env.ANTHROPIC_BASE_URL in " + settingsPath, settings.Env["ANTHROPIC_BASE_URL"]})
		}
	}

	var at []string
	for _, s := range set {
		if !config.PointsAtListen(listen, s.value) {
			return finding{"warn", "claude", fmt.Sprintf("%s is %s, which is not janus (listening on %s),\n"+
				"so Claude Code sessions that use it do not go through janus", s.where, s.value, listen)}
		}
		at = append(at, s.where)
	}
	if len(at) > 0 {
		return finding{"ok", "claude", strings.Join(at, " and ") + " points at janus"}
	}
	unset := "ANTHROPIC_BASE_URL is not set in this shell or in " + settingsPath
	switch seen {
	case trafficRecent:
		return finding{"ok", "claude", unset + ";\nsessions reach janus some other way, such as an alias"}
	case trafficUnknown:
		return finding{"ok", "claude", unset + ";\nsessions may reach janus some other way, such as an alias, but the ledger\n" +
			"that would show it is the running janus's; see listen above"}
	}
	return finding{"warn", "claude", unset + ",\n" +
		"so Claude Code goes straight to Anthropic unless started with it (as below).\n" +
		"To send every session through janus, set it under \"env\" in that file; see\n" +
		"docs/guide/install.md for the catch"}
}

// checkUpdate says whether a newer release is out. Not knowing is never a
// failure: janus works the same offline. A development build has nothing
// to compare, so it asks nothing.
func checkUpdate(client *http.Client, pre bool) finding {
	if _, how := currentVersion(); how == devBuild {
		return finding{"ok", "update", "a development build; janus version -check names the newest release"}
	}
	newer, msg, err := checkForUpdate(context.Background(), client, pre)
	switch {
	case err != nil:
		return finding{"warn", "update", "could not check for a newer release: " + err.Error()}
	case newer:
		return finding{"warn", "update", msg}
	}
	return finding{"ok", "update", msg}
}
