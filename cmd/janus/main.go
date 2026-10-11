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
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/capture"
	"github.com/risadams/janus/internal/config"
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
	if cfg.Features.Route {
		plan = newRouter(cfg, upstream, local).Plan
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

	srv := &http.Server{Handler: proxy.New(proxy.Options{
		Upstream:   upstream,
		Plan:       plan,
		KeepBodies: captures != nil,
		Done: func(ex proxy.Exchange) {
			rec := record(ex, led, captures, logger)
			if shadows != nil {
				shadows.Submit(ex, rec)
			}
		},
	})}

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
			Route: r.Route, Local: models[r.Local],
		}
	}
	return &route.Router{Upstream: upstream, Local: local, Aliases: models, Rules: rules}
}

// record writes down one finished request: its capture, its ledger line,
// and a line on the console. It runs after the response has gone out, and
// a failure here is logged and nothing more: logging must never be the
// reason a request fails. It returns the ledger record it wrote.
func record(ex proxy.Exchange, led *ledger.Ledger, captures *capture.Writer, logger *log.Logger) ledger.Record {
	rec := ledger.FromExchange(ex)
	if captures != nil {
		name, err := captures.Write(ex)
		if err != nil {
			logger.Printf("janus: %v", err)
		}
		rec.Capture = name
	}
	if err := led.Append(rec); err != nil {
		logger.Printf("janus: %v", err)
	}
	logger.Print(formatExchange(ex))
	return rec
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

func versionLine() string {
	return "janus " + version
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
		default:
			target += " " + cfg.Local.Models[r.Local].Model
		}
		name := r.Name
		if name == "" {
			name = fmt.Sprintf("%d", i+1)
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
