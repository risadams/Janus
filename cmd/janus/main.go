// main.go — entry point for the janus binary.
//
// For now janus is a transparent proxy that keeps the books: it forwards
// everything to the upstream API untouched, and records what each request
// was and what it cost. Routing to the local model, and everything built on
// it, arrives in later milestones.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/capture"
	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/guard"
	"github.com/risadams/janus/internal/ledger"
	"github.com/risadams/janus/internal/proxy"
	"github.com/risadams/janus/internal/route"
	"github.com/risadams/janus/internal/shadow"
)

// version is overridden at build time with
// -ldflags "-X main.version=v1.2.3"; "dev" marks a plain `go build`.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "version", "-version", "--version":
			fmt.Fprintln(stdout, versionLine())
			return 0
		case "stats":
			return runStats(args[1:], stdout, stderr)
		case "doctor":
			return runDoctor(args[1:], stdout, stderr)
		case "route":
			return runRoute(args[1:], stdout, stderr)
		case "replay":
			return runReplay(args[1:], stdout, stderr)
		case "audit":
			return runAudit(args[1:], stdout, stderr)
		case "mcp":
			return runMCP(args[1:], os.Stdin, stdout, stderr)
		}
	}

	fs := flag.NewFlagSet("janus", flag.ContinueOnError)
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
		fmt.Fprintf(stderr, "janus: unknown command %q\n", fs.Arg(0))
		return 2
	}

	cfg, created, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "janus: %v\n", err)
		return 1
	}
	cfg = config.Apply(cfg, flags, os.Getenv)
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "janus: %s has problems:\n%v\n", *configPath, err)
		return 1
	}
	if created {
		fmt.Fprintf(stdout, "Wrote a starter config to %s. Edit it to change these settings.\n\n", *configPath)
	}

	// Validate has already established that these parse.
	upstreamURL, _ := url.Parse(cfg.Upstream.BaseURL)
	localURL, _ := url.Parse(cfg.Local.BaseURL)
	upstream := backend.NewUpstream(upstreamURL, nil)

	local := backend.NewLocal(localURL, nil)

	var plan proxy.Planner
	var router *route.Router
	if cfg.Features.Route {
		router = newRouter(cfg, upstream, local)
		plan = router.Plan
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "janus: could not listen on %s: %v\n", cfg.Listen, err)
		return 1
	}
	printBanner(stdout, cfg, ln.Addr().String())

	logger := log.New(stderr, "", log.LstdFlags)
	led := ledger.Open(filepath.Join(cfg.Logging.Dir, "ledger"))
	var captures *capture.Writer
	if cfg.Logging.Capture {
		captures = capture.New(filepath.Join(cfg.Logging.Dir, "captures"), cfg.Logging.RetainDays, cfg.Logging.MaxMegabytes)
	}

	// Shadows are only ever asked for by a routing rule, so the runner is
	// only needed when routing is on.
	var shadows *shadow.Runner
	if cfg.Features.Route {
		shadows = shadow.New(local, led, captures, logger.Printf)
		defer shadows.Close()
	}

	var detector *guard.Detector
	var redactor *guard.Redactor
	var policy *guard.Policy
	var keepLocal proxy.Planner
	if cfg.Features.Guard {
		detector = newDetector(cfg)
		if cfg.Guard.Redact {
			redactor = guard.NewRedactor(detector)
		}
		policy = newPolicy(cfg)
		if policy != nil && cfg.Guard.LocalOnly.Action == "local" {
			// The policy needs a way to the local model whether or not
			// routing is on.
			planner := router
			if planner == nil {
				planner = newRouter(cfg, upstream, local)
			}
			keepLocal = planner.LocalOnly(cfg.Guard.LocalOnly.Local)
		}
	}

	handler := proxy.New(proxy.Options{
		Upstream:     upstream,
		Plan:         plan,
		Redactor:     redactor,
		Policy:       policy,
		PolicyAction: cfg.Guard.LocalOnly.Action,
		LocalOnly:    keepLocal,
		KeepBodies:   captures != nil,
		Done: func(ex proxy.Exchange) {
			rec := record(ex, led, captures, detector, logger)
			if router != nil {
				router.Report(ex) // so it notices the local backend failing
			}
			if shadows != nil {
				shadows.Submit(ex, rec)
			}
		},
	})
	if !cfg.AllowRemote {
		// Loopback keeps other machines out; this keeps out web pages in
		// a browser on this one.
		handler = proxy.SameMachine(handler, func(r *http.Request, why string) {
			logger.Printf("%s %s refused: %s", r.Method, r.URL.Path, why)
		})
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// Give streams in flight a moment to finish, then cut them.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			srv.Close()
		}
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "janus: %v\n", err)
		return 1
	}
	return 0
}

// newRouter builds the router from the config's local models and rules.
func newRouter(cfg config.Config, upstream backend.Backend, local *backend.Local) *route.Router {
	models := make(map[string]route.LocalModel, len(cfg.Local.Models))
	for name, m := range cfg.Local.Models {
		seconds := m.TimeoutSeconds
		if seconds == 0 {
			seconds = config.DefaultLocalTimeoutSeconds
		}
		models[name] = route.LocalModel{ID: m.Model, ContextTokens: m.ContextTokens, Timeout: time.Duration(seconds) * time.Second}
	}
	rules := make([]route.Rule, len(cfg.Routing))
	for i, r := range cfg.Routing {
		rules[i] = route.Rule{
			Name: r.Name, Class: route.Class(r.Class), Model: r.Model, MaxTokens: r.MaxTokens, Tools: r.Tools,
			Route: r.Route, Local: models[r.Local], SkipChecks: r.SkipChecks, TrialTimeout: trialTimeout(r),
		}
		if m, ok := models[r.IfDown]; ok {
			rules[i].IfDown = &m
		}
	}
	return &route.Router{Upstream: upstream, Local: local, Aliases: models, Rules: rules}
}

// trialTimeout is how long a local-then-escalate rule lets the local model
// work before asking Claude.
func trialTimeout(r config.Rule) time.Duration {
	if r.Route != route.RouteEscalate {
		return 0
	}
	seconds := r.MaxSeconds
	if seconds == 0 {
		seconds = config.DefaultTrialSeconds
	}
	return time.Duration(seconds) * time.Second
}

// record writes down one finished request: its capture, its ledger line,
// and a line on the console. It runs after the response has gone out, and
// a failure here is logged and nothing more: logging must never be the
// reason a request fails. It returns the ledger record it wrote.
func record(ex proxy.Exchange, led *ledger.Ledger, captures *capture.Writer, detector *guard.Detector, logger *log.Logger) ledger.Record {
	rec := ledger.FromExchange(ex)
	found := ""
	g := ex.Guard
	if g == nil {
		g = &proxy.Guard{}
	}
	if g.Scanned || g.Blocked != "" || len(g.LocalOnly) > 0 {
		rec.Guard = &ledger.Guard{Blocked: g.Blocked, LocalOnly: g.LocalOnly, Withheld: g.Withheld}
	}
	if g.Scanned {
		// The redactor dealt with this request on its way out.
		replaced := g.Replaced
		rec.Guard.Findings, rec.Guard.Kinds, rec.Guard.Where = g.Report.Findings, g.Report.Kinds, g.Report.Where
		rec.Guard.Redacted, rec.Guard.Restored = &replaced, g.Restored
		if g.Report.Findings == 0 && g.Blocked == "" && len(g.LocalOnly) == 0 {
			rec.Guard = nil // nothing to say
		}
	} else if report, scanned := scanForSecrets(detector, ex); scanned && report.Findings > 0 {
		// Detect only: look at what was sent, after the fact.
		if rec.Guard == nil {
			rec.Guard = &ledger.Guard{}
		}
		rec.Guard.Findings, rec.Guard.Kinds, rec.Guard.Where = report.Findings, report.Kinds, report.Where
		found = fmt.Sprintf(" [guard: %d found going upstream: %s]", report.Findings, report.Summary())
	}
	switch {
	case g.Blocked != "":
		found = " [guard: stopped: " + g.Blocked + "]"
	case g.Replaced > 0:
		found = fmt.Sprintf(" [guard: %d kept from going upstream (%s), %d put back in the answer]", g.Replaced, g.Report.Summary(), g.Restored)
	}
	switch {
	case len(g.LocalOnly) == 0 || g.Blocked != "":
	case g.Withheld > 0:
		found += fmt.Sprintf(" [local-only: %d withheld from upstream (%s)]", g.Withheld, strings.Join(g.LocalOnly, "; "))
	case ex.Backend == "local":
		found += fmt.Sprintf(" [local-only: kept on this machine (%s)]", strings.Join(g.LocalOnly, "; "))
	}
	if captures != nil {
		name, err := captures.Write(ex)
		if err != nil {
			logger.Printf("janus: %v", err)
		}
		rec.Capture = name
		if a := ex.Attempt; a != nil && !a.Accepted && len(a.ResponseBody) > 0 && name != "" {
			if err := captures.WriteAttempt(name, a.ResponseBody); err != nil {
				logger.Printf("janus: %v", err)
			}
		}
	}
	if err := led.Append(rec); err != nil {
		logger.Printf("janus: %v", err)
	}
	logger.Print(formatExchange(ex) + found)
	return rec
}

// scanForSecrets looks for secrets in a Messages request that was sent to
// the upstream, whether or not the upstream ended up answering it.
// Requests that only ever went to the local model are not scanned: they
// did not leave the machine's own network.
func scanForSecrets(detector *guard.Detector, ex proxy.Exchange) (guard.Report, bool) {
	wentUpstream := ex.Backend == "upstream" || (ex.Attempt != nil && ex.Attempt.Backend == "upstream")
	if detector == nil || !ex.IsMessages() || !wentUpstream || len(ex.RequestBody) == 0 || ex.RequestTruncated {
		return guard.Report{}, false
	}
	req, err := anthropic.ParseRequest(ex.RequestBody)
	if err != nil {
		return guard.Report{}, false
	}
	return detector.ScanRequest(req), true
}

// newPolicy builds the local-only policy from the config, or nil if it
// protects nothing. Validate has already established that it compiles.
func newPolicy(cfg config.Config) *guard.Policy {
	lo := cfg.Guard.LocalOnly
	if !lo.On() {
		return nil
	}
	policy, _ := guard.NewPolicy(lo.Paths, lo.Markers)
	return policy
}

// newDetector builds the detector from the config. Validate has already
// established that the patterns compile.
func newDetector(cfg config.Config) *guard.Detector {
	d := &guard.Detector{Allow: cfg.Guard.Allow, Entropy: cfg.Guard.Entropy}
	for _, pattern := range cfg.Guard.Patterns {
		d.Patterns = append(d.Patterns, regexp.MustCompile(pattern))
	}
	return d
}

// formatExchange is the one line janus logs per request. For a Messages
// response it adds what the model did and what it cost in tokens.
func formatExchange(ex proxy.Exchange) string {
	line := fmt.Sprintf("%s %s -> %d (%s)", ex.Method, ex.Path, ex.Status, ex.Duration.Round(time.Millisecond))
	switch ex.Backend {
	case "", "upstream":
	case "none":
		line += " refused: " + ex.Reason
	default:
		line += " [" + ex.Backend + "]"
	}
	if a := ex.Attempt; a != nil && !a.Accepted {
		line += fmt.Sprintf(" [after %s tried for %s: %s]", a.Backend, a.Duration.Round(100*time.Millisecond), a.Why)
	}
	if ex.Unread != "" {
		return line + " response not read: " + ex.Unread
	}
	s := ex.Summary
	if s == nil {
		return line
	}
	if s.Error != nil {
		return line + " error=" + s.Error.Type
	}
	if s.Model != "" {
		line += " " + s.Model
	}
	if s.StopReason != "" {
		line += " " + s.StopReason
	}
	line += fmt.Sprintf(" in=%d", s.Usage.Input)
	if s.Usage.CacheWrite != nil {
		line += fmt.Sprintf(" cache_write=%d", *s.Usage.CacheWrite)
	}
	if s.Usage.CacheRead != nil {
		line += fmt.Sprintf(" cache_read=%d", *s.Usage.CacheRead)
	}
	line += fmt.Sprintf(" out=%d", s.Usage.Output)
	if !s.Complete {
		line += " (incomplete)"
	}
	return line
}

// versionLine is what `janus version` prints. A release build has its
// version set by the linker; one made with `go install ...@v1.2.3` takes it
// from the module; anything else is "dev".
func versionLine() string {
	v := version
	if v == "dev" {
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
	}
	return "janus " + v
}

// routingSummary describes, one line each, what the config routes where:
// the aliases, then the rules in the order they are tried.
func routingSummary(cfg config.Config) []string {
	names := make([]string, 0, len(cfg.Local.Models))
	for name := range cfg.Local.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	var lines []string
	for _, name := range names {
		lines = append(lines, fmt.Sprintf("model %q -> local %s", name, cfg.Local.Models[name].Model))
	}
	for i, r := range cfg.Routing {
		var conditions []string
		if r.Class != "" {
			conditions = append(conditions, "class "+r.Class)
		}
		if r.Model != "" {
			conditions = append(conditions, "model "+r.Model)
		}
		if r.MaxTokens > 0 {
			conditions = append(conditions, fmt.Sprintf("up to %d tokens", r.MaxTokens))
		}
		if r.Tools != nil {
			if *r.Tools {
				conditions = append(conditions, "with tools")
			} else {
				conditions = append(conditions, "without tools")
			}
		}
		when := "everything else"
		if len(conditions) > 0 {
			when = strings.Join(conditions, ", ")
		}
		target := r.Route
		switch r.Route {
		case route.RouteUpstream:
		case route.RouteShadow:
			target = "upstream, shadowed on " + cfg.Local.Models[r.Local].Model
		case route.RouteEscalate:
			target = fmt.Sprintf("local %s first, Claude if its answer fails the checks or takes over %s",
				cfg.Local.Models[r.Local].Model, trialTimeout(r))
		default:
			target += " " + cfg.Local.Models[r.Local].Model
		}
		name := r.Name
		if name == "" {
			name = fmt.Sprintf("%d", i+1)
		}
		if r.IfDown != "" {
			target += ", or local " + cfg.Local.Models[r.IfDown].Model + " if Claude cannot answer"
		}
		lines = append(lines, fmt.Sprintf("rule %s: %s -> %s", name, when, target))
	}
	return lines
}

// printClaudeCommand gives the exact command that points Claude Code at
// janus, so nobody has to look up the variable.
//
// ENABLE_TOOL_SEARCH is set because Claude Code turns tool search off when
// its base URL is not Anthropic's own, and then sends every tool schema in
// every request; setting it keeps the traffic shaped the way it is without
// a proxy.
func printClaudeCommand(w io.Writer, base string) {
	fmt.Fprintf(w, "  # bash / zsh / fish\n")
	fmt.Fprintf(w, "  ANTHROPIC_BASE_URL=%s ENABLE_TOOL_SEARCH=true claude\n\n", base)
	fmt.Fprintf(w, "  # PowerShell\n")
	fmt.Fprintf(w, "  $env:ANTHROPIC_BASE_URL=\"%s\"; $env:ENABLE_TOOL_SEARCH=\"true\"; claude\n\n", base)
}

// printBanner says where janus is listening and what it will write.
func printBanner(w io.Writer, cfg config.Config, addr string) {
	base := "http://" + addr
	fmt.Fprintf(w, "%s\n\n", versionLine())
	fmt.Fprintf(w, "  Listening   %s\n", base)
	fmt.Fprintf(w, "  Upstream    %s\n", cfg.Upstream.BaseURL)
	fmt.Fprintf(w, "  Local       %s\n", cfg.Local.BaseURL)
	if !cfg.Features.Route {
		fmt.Fprintf(w, "  Routing     off; every request goes upstream\n")
	} else {
		for i, line := range routingSummary(cfg) {
			label := "  Routing     "
			if i > 0 {
				label = "              "
			}
			fmt.Fprintf(w, "%s%s\n", label, line)
		}
	}
	if cfg.Features.Guard {
		if cfg.Guard.Redact {
			fmt.Fprintf(w, "  Guard       replacing secrets with placeholders in requests going upstream, and putting them back in answers\n")
		} else {
			fmt.Fprintf(w, "  Guard       looking for secrets in requests going upstream; detect only, nothing is changed\n")
		}
	}
	if lo := cfg.Guard.LocalOnly; cfg.Features.Guard && lo.On() {
		what := strings.Join(append(append([]string{}, lo.Paths...), lo.Markers...), ", ")
		if lo.Action == "local" {
			fmt.Fprintf(w, "  Local-only  %s: requests holding these are answered by the local model, or refused\n", what)
		} else {
			fmt.Fprintf(w, "  Local-only  %s: withheld from anything sent upstream\n", what)
		}
	}
	fmt.Fprintf(w, "  Ledger      %s\n", filepath.Join(cfg.Logging.Dir, "ledger"))
	if cfg.Logging.Capture {
		fmt.Fprintf(w, "  Captures    %s (full prompts and responses)\n\n", filepath.Join(cfg.Logging.Dir, "captures"))
	} else {
		fmt.Fprintf(w, "  Captures    off\n\n")
	}
	fmt.Fprintf(w, "Point Claude Code at janus:\n\n")
	printClaudeCommand(w, base)
	fmt.Fprintf(w, "Press Ctrl+C to stop.\n\n")
}
