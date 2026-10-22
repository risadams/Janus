// Command janus is a proxy between Claude Code and the Anthropic API. With
// nothing switched on it forwards every request untouched and records what
// each one was and what it cost. The config can switch on routing to a
// local model, a guard that keeps secrets and protected files from leaving,
// and transforms that send fewer tokens.
//
// Run with no arguments, it serves. The subcommands are version, stats,
// doctor, route, replay, audit, mcp, claude, service, update, config, and
// completion;
// each answers -h.
package main

import (
	"context"
	"errors"
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
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/capture"
	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/digest"
	"github.com/risadams/janus/internal/guard"
	"github.com/risadams/janus/internal/instance"
	"github.com/risadams/janus/internal/ledger"
	"github.com/risadams/janus/internal/openai"
	"github.com/risadams/janus/internal/proxy"
	"github.com/risadams/janus/internal/route"
	"github.com/risadams/janus/internal/shadow"
	"github.com/risadams/janus/internal/transform"
	"github.com/risadams/janus/internal/worker"
)

// version is overridden at build time with
// -ldflags "-X main.version=v1.2.3"; "dev" marks a plain `go build`.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// command is one of janus's subcommands. The table in commands is both
// what run dispatches on and what janus completion offers, so the two
// cannot disagree.
type command struct {
	name string
	// about is the line a shell shows beside the name as it completes.
	about string
	// verbs are the words the command takes first, for one that has them;
	// each verb has its own flag set, named "janus <name> <verb>".
	verbs []verb
	run   func(args []string, stdout, stderr io.Writer) int
}

// verb is a word a command takes first, such as service's install.
type verb struct{ name, about string }

func commands() []command {
	return []command{
		{name: "version", about: "print the version, or check for a newer one", run: runVersion},
		{name: "stats", about: "summarize the ledger: requests, tokens, and cost", run: runStats},
		{name: "doctor", about: "check the config, the upstream, and LM Studio", run: runDoctor},
		{name: "route", about: "show where a request would be sent, and why", run: runRoute},
		{name: "replay", about: "send captured requests again, to compare", run: runReplay},
		{name: "audit", about: "report what reached Anthropic and what the guard did", run: runAudit},
		{name: "mcp", about: "serve the local model to Claude Code as MCP tools", run: func(args []string, stdout, stderr io.Writer) int {
			return runMCP(args, os.Stdin, stdout, stderr)
		}},
		{name: "claude", about: "run Claude Code through janus", run: func(args []string, stdout, stderr io.Writer) int {
			return runClaude(args, os.Stdin, stdout, stderr)
		}},
		{name: "local", about: "run Claude Code on the local model only, through janus", run: func(args []string, stdout, stderr io.Writer) int {
			return runLocal(args, os.Stdin, stdout, stderr)
		}},
		{name: "service", about: "run janus when you log in", verbs: serviceVerbs, run: runService},
		{name: "update", about: "replace this janus with the latest release", run: runUpdate},
		{name: "config", about: "rewrite an old config in this version's form", verbs: configVerbs, run: runConfig},
		{name: "completion", about: "print a tab-completion script for a shell", verbs: completionShells, run: runCompletion},
	}
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		if args[0] == "-version" || args[0] == "--version" {
			return runVersion(args[1:], stdout, stderr)
		}
		for _, c := range commands() {
			if c.name == args[0] {
				return c.run(args[1:], stdout, stderr)
			}
		}
	}

	fs := newFlagSet("janus", stderr)
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
	if len(cfg.Upgraded) > 0 {
		fmt.Fprintf(stdout, "%s\n\n", upgradedNote(*configPath, cfg.Upgraded))
	}

	// Validate has already established that these parse.
	upstreamURL, _ := url.Parse(cfg.Upstream.BaseURL)
	localURL, _ := url.Parse(cfg.Local.BaseURL)
	var upstream backend.Backend = backend.NewUpstream(upstreamURL, nil)
	if cfg.Upstream.Off {
		upstream = backend.Nowhere{}
	}

	local := backend.NewLocal(localURL, nil)

	// The key is read once, here, and only if a rule can send a request
	// to OpenAI.
	var oa *openai.Backend
	if namesOpenAI(cfg) {
		key, err := openai.LoadKey(cfg.OpenAI, os.LookupEnv)
		if err == nil {
			oa, err = openai.NewBackend(cfg.OpenAI, key, nil)
		}
		if err != nil {
			fmt.Fprintf(stderr, "janus: %v. A routing rule's fallback names openai, which needs it.\n", err)
			return 1
		}
	}

	var plan proxy.Planner
	var router *route.Router
	if cfg.Features.Route {
		router = newRouter(cfg, upstream, local, oa)
		plan = router.Plan
	}

	// One Janus per log folder: a second would write the same ledger and
	// captures. The lock is taken before listening, so a second Janus on
	// the same port hears about the first rather than about the port.
	lock, err := instance.Acquire(cfg.Logging.Dir)
	if err != nil {
		var held *instance.HeldError
		if errors.As(err, &held) {
			fmt.Fprintf(stderr, "janus: %v.\nStop that one first, or give this one its own logging.dir in %s.\n", err, *configPath)
		} else {
			fmt.Fprintf(stderr, "janus: %v\n", err)
		}
		return 1
	}
	defer lock.Release()
	if router != nil {
		// Under the lock: one janus per log folder keeps this file.
		if err := router.Remember(filepath.Join(cfg.Logging.Dir, route.LimitsFile)); err != nil {
			fmt.Fprintf(stderr, "janus: %v\n", err)
			return 1
		}
	}

	// The placeholder key is kept with the logs, so a secret keeps its
	// placeholder across restarts and the prompt cache stays valid.
	var redactKey []byte
	if cfg.Features.Guard && cfg.Guard.Redact {
		if redactKey, err = guard.LoadKey(filepath.Join(cfg.Logging.Dir, guard.KeyFile)); err != nil {
			fmt.Fprintf(stderr, "janus: %v\n", err)
			return 1
		}
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "janus: could not listen on %s: %v\n", cfg.Listen, err)
		if instance.AddrInUse(err) {
			fmt.Fprintf(stderr, "Another program is listening there, perhaps a janus with a different log folder. Stop it, or pick another address with -listen.\n")
		}
		return 1
	}
	if err := lock.Describe(fmt.Sprintf("pid %d, listening on http://%s, started %s",
		os.Getpid(), ln.Addr(), time.Now().Format("2006-01-02 15:04"))); err != nil {
		fmt.Fprintf(stderr, "janus: %v\n", err)
	}
	printBanner(stdout, cfg, ln.Addr().String(), *configPath)

	console := NewConsole(stderr, bool(newPainter(stderr)), prices(cfg))
	console.WarnAtContext(cfg.Console.ContextWarn())
	logger := console.Logger()
	led := ledger.Open(filepath.Join(cfg.Logging.Dir, "ledger"))
	var captures *capture.Writer
	if cfg.Logging.Capture {
		captures = capture.New(filepath.Join(cfg.Logging.Dir, "captures"), cfg.Logging.RetainDays, cfg.Logging.MaxMegabytes)
		if err := captures.Prune(time.Now()); err != nil {
			console.Printf("janus: %v", err)
		}
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
			redactor = guard.NewRedactorWithKey(detector, redactKey)
		}
		policy = newPolicy(cfg)
		if policy != nil && cfg.Guard.LocalOnly.Action == "local" {
			// The policy needs a way to the local model whether or not
			// routing is on.
			planner := router
			if planner == nil {
				planner = newRouter(cfg, upstream, local, nil)
			}
			keepLocal = planner.LocalOnly(cfg.Guard.LocalOnly.Local)
		}
	}

	if cfg.Features.Compress {
		if router == nil {
			// Compressing without routing: everything goes upstream, as
			// it would with no router, and only the transforms apply.
			router = &route.Router{Upstream: upstream, Local: local}
			plan = router.Plan
		}
		router.Transforms = newPipeline(cfg, local, policy, logger)
	}

	var barred func(string, *anthropic.Request) string
	if kf := newKeepFrom(cfg); kf != nil {
		barred = kf.Barred
		if router != nil {
			router.Barred = barred // a rule's list passes over what it bars
		}
	}
	var itself func(*http.Request, []byte, proxy.Plan) proxy.Reply
	if cfg.Upstream.Off {
		itself = router.Itself // Validate requires routing with upstream off
	}
	handler := proxy.New(proxy.Options{
		Upstream:     upstream,
		Plan:         plan,
		Redactor:     redactor,
		Policy:       policy,
		PolicyAction: cfg.Guard.LocalOnly.Action,
		LocalOnly:    keepLocal,
		KeepBodies:   captures != nil,
		Itself:       itself,
		Barred:       barred,
		Done: func(ex proxy.Exchange) {
			rec := record(ex, led, captures, detector, console)
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
	go noticeUpdate(ctx, cfg, &http.Client{Timeout: 10 * time.Second}, func(line string) {
		console.Printf("%s", console.paint(yellow, line))
	})
	go func() {
		<-ctx.Done()
		// Let the requests in flight finish, as a restart after janus
		// update relies on, but not forever. From here a second Ctrl-C
		// has its usual effect and stops janus at once.
		stop()
		console.Printf("Stopping once the requests in flight finish, for up to a minute. Press Ctrl-C again to stop now.")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			_ = srv.Close() // the grace period is over; stopping is all that is left
		}
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "janus: %v\n", err)
		return 1
	}
	return 0
}

// shutdownGrace is how long janus waits, once asked to stop, for the
// requests in flight to finish. A long answer from Claude can stream for
// minutes; past this it is cut. The launchd agent's ExitTimeOut and
// systemd's TimeoutStopSec leave it this long.
const shutdownGrace = time.Minute

// namesOpenAI reports whether a routing rule in force can send a request
// to OpenAI.
func namesOpenAI(cfg config.Config) bool {
	if !cfg.Features.Route {
		return false
	}
	for _, r := range cfg.Routing {
		if slices.Contains(r.Fallback, route.RouteOpenAI) {
			return true
		}
	}
	return false
}

// newRouter builds the router from the config's local models and rules.
// oa is the OpenAI backend, or nil if no rule names it.
func newRouter(cfg config.Config, upstream backend.Backend, local *backend.Local, oa *openai.Backend) *route.Router {
	models := make(map[string]route.LocalModel, len(cfg.Local.Models))
	for name, m := range cfg.Local.Models {
		seconds := m.TimeoutSeconds
		if seconds == 0 {
			seconds = config.DefaultLocalTimeoutSeconds
		}
		concurrent := m.MaxConcurrent
		if concurrent == 0 {
			concurrent = config.DefaultMaxConcurrent
		}
		models[name] = route.LocalModel{ID: m.Model, ContextTokens: m.ContextTokens, Timeout: time.Duration(seconds) * time.Second, MaxConcurrent: concurrent}
	}
	rules := make([]route.Rule, len(cfg.Routing))
	for i, r := range cfg.Routing {
		rules[i] = route.Rule{
			Name: r.Name, Class: route.Class(r.Class), Model: r.Model, MaxTokens: r.MaxTokens, Tools: r.Tools,
			Route: r.Route, Local: models[r.Local], SkipChecks: r.SkipChecks, TrialTimeout: trialTimeout(r),
			IfUsageAbove: r.IfUsageAbove,
		}
		for _, name := range r.Fallback {
			p := route.Provider{Name: name}
			if m, ok := models[name]; ok {
				p.Local = &m
			}
			rules[i].Fallback = append(rules[i].Fallback, p)
		}
	}
	return &route.Router{Upstream: upstream, Local: local, OpenAI: oa, Aliases: models, Rules: rules, NoUpstream: cfg.Upstream.Off}
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
// a failure here is reported and nothing more: logging must never be the
// reason a request fails. It returns the ledger record it wrote.
func record(ex proxy.Exchange, led *ledger.Ledger, captures *capture.Writer, detector *guard.Detector, console *Console) ledger.Record {
	rec := ledger.FromExchange(ex)
	var guardNotes []note
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
		guardNotes = append(guardNotes, note{level: warn, text: fmt.Sprintf("guard: %d found going upstream: %s", report.Findings, report.Summary())})
	}
	switch {
	case g.Blocked != "":
		guardNotes = []note{{level: bad, text: "guard: stopped: " + g.Blocked}}
	case g.Replaced > 0:
		// The same secret is replaced on every turn that carries it; that
		// is said once per conversation. Putting one back in an answer is
		// news each time.
		if g.Restored > 0 {
			guardNotes = []note{{level: info, text: fmt.Sprintf("guard: %d kept from going upstream (%s), %d put back in the answer", g.Replaced, g.Report.Summary(), g.Restored)}}
		} else {
			text := fmt.Sprintf("guard: %d kept from going upstream (%s)", g.Replaced, g.Report.Summary())
			guardNotes = []note{{level: info, text: text, once: text}}
		}
	}
	switch {
	case len(g.LocalOnly) == 0 || g.Blocked != "":
	case g.Withheld > 0:
		text := fmt.Sprintf("local-only: %d withheld from upstream (%s)", g.Withheld, strings.Join(g.LocalOnly, "; "))
		guardNotes = append(guardNotes, note{level: info, text: text, once: text})
	case ex.Backend == "local":
		text := fmt.Sprintf("local-only: kept on this machine (%s)", strings.Join(g.LocalOnly, "; "))
		guardNotes = append(guardNotes, note{level: info, text: text, once: text})
	}
	outageNotes := autoModeNotes(ex, rec, console)
	if n := handBack(ex, &rec, console); n != nil {
		outageNotes = append(outageNotes, *n)
	}
	if captures != nil {
		// A name with an error is a capture saved and a prune that failed.
		name, err := captures.Write(ex)
		if err != nil {
			console.Printf("janus: %v", err)
		}
		rec.Capture = name
		if a := ex.Attempt; a != nil && !a.Accepted && len(a.ResponseBody) > 0 && name != "" {
			if err := captures.WriteAttempt(name, a.ResponseBody); err != nil {
				console.Printf("janus: %v", err)
			}
		}
	}
	if err := led.Append(rec); err != nil {
		console.Printf("janus: %v", err)
	}
	console.Request(ex, rec, slices.Concat(exchangeNotes(ex), guardNotes, outageNotes, console.QuotaNotes(rec.Quota), console.ContextNotes(rec)))
	return rec
}

// stoodInForClaude reports whether another provider answered a request
// because Claude could not: Claude was tried and failed, or passed over
// while its limit resets (#378).
func stoodInForClaude(ex proxy.Exchange) bool {
	if ex.Backend != "local" && ex.Backend != "openai" {
		return false
	}
	if len(ex.Tried) > 0 {
		return ex.Tried[0].Backend == "upstream"
	}
	a := ex.Attempt
	return a != nil && !a.Accepted && a.Backend == "upstream"
}

// autoModeNotes says what to do when auto mode's safety check cannot reach
// Claude while the local model is carrying the conversation (#173). Auto
// mode asks Claude whether each tool call is safe, and those checks stay
// with Claude (#162); Claude Code then refuses every tool call as
// "temporarily unavailable", and the way through is to approve them. It
// also notes, for later requests, when the local model stood in.
func autoModeNotes(ex proxy.Exchange, rec ledger.Record, console *Console) []note {
	if stoodInForClaude(ex) {
		console.StoodIn(rec.Session)
		if rec.Class == string(route.Main) {
			console.StoodInTurn(rec.Session, ex.Backend)
		}
		return nil
	}
	if rec.Class != string(route.Safety) || ex.Backend != "upstream" || !claudeCouldNotAnswer(ex) || !console.StandingIn(rec.Session) {
		return nil
	}
	text := "auto mode's safety check could not reach Claude, so Claude Code will refuse tool calls; press Shift+Tab to leave auto mode and approve them yourself while the local model stands in"
	return []note{{level: warn, text: text, once: text}}
}

// handBack marks the first answer Claude gives a conversation's main loop
// after the local model stood in for it (#241). Claude reads the local
// turns as its own, so they are said, and recorded, rather than left
// mixed in unremarked.
func handBack(ex proxy.Exchange, rec *ledger.Record, console *Console) *note {
	if rec.Class != string(route.Main) || rec.Session == "" || ex.Backend != "upstream" ||
		ex.Status <= 0 || ex.Status >= 400 || claudeCouldNotAnswer(ex) {
		return nil
	}
	by, since := console.HandBack(rec.Session)
	if len(by) == 0 {
		return nil
	}
	rec.Handback = &ledger.Handback{LocalTurns: by["local"], OpenAITurns: by["openai"], Since: since.UTC()}
	var from []string
	for _, provider := range []string{"openai", "local"} {
		if n := by[provider]; n > 0 {
			from = append(from, fmt.Sprintf("%d turns from %s", n, provider))
		}
	}
	text := strings.Join(from, " and ")
	text = strings.Replace(strings.Replace(text, "from openai", "from OpenAI", 1), "from local", "from the local model", 1)
	return &note{level: warn, text: fmt.Sprintf("Claude is answering again, after %s since %s; it reads them as its own, so check what they did",
		text, since.Local().Format("15:04"))}
}

// claudeCouldNotAnswer reports whether Claude's response says it could not
// answer: a status that would have brought in a stand-in, or the same
// errors arriving inside a 200 stream.
func claudeCouldNotAnswer(ex proxy.Exchange) bool {
	if route.UpstreamDown(ex.Status) {
		return true
	}
	if s := ex.Summary; s != nil && s.Error != nil {
		switch s.Error.Type {
		case "rate_limit_error", "overloaded_error", "api_error":
			return true
		}
	}
	return false
}

// exchangeNotes are what is worth saying about a request beyond who
// answered and what it cost: a refusal, a stand-in or an escalation and
// why, a fault, a transform's decision, a response that could not be read.
func exchangeNotes(ex proxy.Exchange) []note {
	var notes []note
	if ex.Backend == "none" {
		notes = append(notes, note{level: bad, text: "refused: " + ex.Reason})
	}
	switch a := ex.Attempt; {
	case a != nil && !a.Accepted && a.Backend == "local" && len(ex.Tried) <= 1:
		notes = append(notes, note{level: info, text: fmt.Sprintf("%s tried for %s first, then it went to %s: %s", a.Backend, humanDuration(a.Duration), provider(ex.Backend), a.Why) + nextStep(a.Cause)})
	case len(ex.Tried) > 0 && ex.NoFallback == "":
		notes = append(notes, triedNote(ex))
	}
	if ex.NoFallback != "" {
		notes = append(notes, note{level: warn, text: ex.NoFallback + nextStep(ex.LocalCause)})
	} else if ex.LocalCause != "" && ex.Attempt == nil && len(ex.Tried) == 0 {
		// A rule meant this for the local model and sent it to Claude
		// without trying. Busy and the like recur on every parallel turn,
		// so each cause is said once per conversation.
		text := "not sent to the local model (" + strings.ReplaceAll(ex.LocalCause, "_", " ") + ")" + nextStep(ex.LocalCause)
		notes = append(notes, note{level: info, text: text, once: "local-cause " + ex.LocalCause})
	}
	if ex.Fault != "" {
		notes = append(notes, note{level: bad, text: "fault: " + ex.Fault})
	}
	// Transforms are noted on the turn that decides something; later turns
	// carry the same output without comment.
	for _, e := range ex.Transforms {
		if e.Rewritten+e.Revised > 0 {
			notes = append(notes, note{level: info, text: fmt.Sprintf("%s: %d rewritten, %d revised, about %s tokens fewer", e.Name, e.Rewritten, e.Revised, humanCount(e.Tokens))})
		}
	}
	if ex.Unread != "" {
		notes = append(notes, note{level: warn, text: "response not read: " + ex.Unread})
	}
	return notes
}

// provider names a backend for a person.
func provider(backend string) string {
	switch backend {
	case "upstream":
		return "Claude"
	case "openai":
		return "OpenAI"
	case "local":
		return "the local model"
	}
	return backend
}

// triedNote says which providers on a rule's list came before the one
// that answered, and why each did not (#429). Claude failing and the local
// model standing in is said as it always was.
func triedNote(ex proxy.Exchange) note {
	parts := make([]string, 0, len(ex.Tried))
	level, cause := info, ""
	for _, step := range ex.Tried {
		if step.Sent {
			parts = append(parts, fmt.Sprintf("%s could not answer (%s)", provider(step.Backend), step.Why))
			level = warn
		} else {
			parts = append(parts, fmt.Sprintf("%s was passed over (%s)", provider(step.Backend), step.Why))
		}
		if cause == "" && step.Backend == "local" {
			cause = step.Cause
		}
	}
	text := strings.Join(parts, "; ")
	if ex.Backend == "local" && ex.Tried[0].Backend == "upstream" && ex.Tried[0].Sent {
		text += ", so the local model stood in"
		if slices.Contains(ex.Stages, route.StageLeaveOut) {
			text += ", with older turns left out to fit its context"
		}
	} else {
		text += ", so it went to " + provider(ex.Backend)
	}
	n := note{level: level, text: text + nextStep(cause)}
	if level == info {
		// Passed over without being tried happens again on every turn
		// like it, so it is said once per conversation.
		n.once = n.text
	}
	return n
}

// nextStep is a cause's next step, as the tail of a console note.
func nextStep(cause string) string {
	if hint := proxy.CauseHint(cause); hint != "" {
		return "; next: " + hint
	}
	return ""
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

// newPipeline builds the transforms for features.compress, of which there
// is one: the digest of large tool results. Validate has already established that
// there is a local model to write them.
func newPipeline(cfg config.Config, local *backend.Local, policy *guard.Policy, logger *log.Logger) *transform.Pipeline {
	d := cfg.Compress.Digest
	_, model, _ := cfg.DigestModel()
	seconds, minBytes := d.MaxSeconds, d.MinBytes
	if seconds == 0 {
		seconds = config.DefaultDigestSeconds
	}
	if minBytes == 0 {
		minBytes = config.DefaultDigestMinBytes
	}
	digester := &digest.Digest{
		Model:    &worker.Model{Local: local, ID: model.Model, Timeout: time.Duration(seconds) * time.Second},
		Store:    digest.NewStore(filepath.Join(cfg.Logging.Dir, "originals"), cfg.Logging.RetainDays, logger.Printf),
		MinBytes: minBytes,
		Tools:    d.Tools,
		Logf:     logger.Printf,
	}
	if policy != nil {
		digester.Protected = func(req *anthropic.Request, message, block int, toolUseID string) bool {
			return policy.Check(req).Covers(message, block, toolUseID)
		}
	}

	pipeline := transform.New(digester)
	table := prices(cfg)
	pipeline.Pricing = func(model string) (transform.Rates, bool) {
		p, ok := table.Lookup(model)
		return transform.Rates{Read: p.CacheRead, Write5m: p.CacheWrite5m, Write1h: p.CacheWrite1h}, ok
	}
	pipeline.Notify = func(name, why string) {
		logger.Printf("transform %s may no longer change what was already sent: %s", name, why)
	}
	return pipeline
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

// newKeepFrom builds guard.keepFrom's check, or nil if it keeps nothing.
// Validate has already established that the paths and markers compile.
func newKeepFrom(cfg config.Config) *guard.KeepFrom {
	if !cfg.Features.Guard || len(cfg.Guard.KeepFrom) == 0 {
		return nil
	}
	kf := guard.NewKeepFrom()
	for name, k := range cfg.Guard.KeepFrom {
		_ = kf.Add(name, k.Paths, k.Markers, k.Everything) // compiled once already, by Validate
	}
	return kf
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
		windows := make([]string, 0, len(r.IfUsageAbove))
		for w := range r.IfUsageAbove {
			windows = append(windows, w)
		}
		sort.Strings(windows)
		for _, w := range windows {
			conditions = append(conditions, fmt.Sprintf("once the %s limit is over %.0f%% used", w, r.IfUsageAbove[w]*100))
		}
		when := "everything else"
		if len(conditions) > 0 {
			when = strings.Join(conditions, ", ")
		}
		target := r.Route
		switch r.Route {
		case "":
			target = describeProvider(cfg, r.Fallback[0])
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
		if len(r.Fallback) > 1 {
			rest := make([]string, 0, len(r.Fallback)-1)
			for _, name := range r.Fallback[1:] {
				rest = append(rest, describeProvider(cfg, name))
			}
			first := "it"
			if r.Fallback[0] == route.RouteUpstream {
				first = "Claude"
			}
			target += fmt.Sprintf(", or %s if %s cannot answer", strings.Join(rest, ", then "), first)
		}
		lines = append(lines, fmt.Sprintf("rule %s: %s -> %s", name, when, target))
	}
	return lines
}

// describeProvider names an entry of a fallback list for the banner.
func describeProvider(cfg config.Config, name string) string {
	if m, ok := cfg.Local.Models[name]; ok {
		return "local " + m.Model
	}
	return name
}

// printClaudeCommand gives the exact command that starts Claude Code
// through janus, so nobody has to look up the variables: janus claude
// first, which checks that janus is answering, then the variables it sets,
// for anyone setting them another way. configPath is the config janus was
// given, so that janus claude reads the same one.
//
// ENABLE_TOOL_SEARCH is set because Claude Code turns tool search off when
// its base URL is not Anthropic's own, and then sends every tool schema in
// every request; setting it keeps the traffic shaped the way it is without
// a proxy. With upstream off, ANTHROPIC_AUTH_TOKEN is a placeholder, so
// that Claude Code starts without an Anthropic login; janus never
// forwards it anywhere (#376).
func printClaudeCommand(w io.Writer, base, configPath string, upstreamOff bool) {
	launch := "janus claude"
	if configPath != config.DefaultPath {
		if abs, err := filepath.Abs(configPath); err == nil {
			configPath = abs
		}
		launch += " -config " + configPath
	}
	fmt.Fprintf(w, "  %s\n\n", launch)
	token, psToken := "", ""
	if upstreamOff {
		token = "ANTHROPIC_AUTH_TOKEN=" + placeholderToken + " "
		psToken = "$env:ANTHROPIC_AUTH_TOKEN=\"" + placeholderToken + "\"; "
	}
	fmt.Fprintf(w, "  # or set the variables yourself: bash / zsh / fish\n")
	fmt.Fprintf(w, "  %sANTHROPIC_BASE_URL=%s ENABLE_TOOL_SEARCH=true claude\n\n", token, base)
	fmt.Fprintf(w, "  # PowerShell\n")
	fmt.Fprintf(w, "  %s$env:ANTHROPIC_BASE_URL=\"%s\"; $env:ENABLE_TOOL_SEARCH=\"true\"; claude\n\n", psToken, base)
}

// placeholderToken is the credential janus claude gives Claude Code when
// there is no Anthropic account behind janus. Claude Code will not start
// without one; janus sends it nowhere: nothing goes to Anthropic, and
// OpenAI and LM Studio are sent none of Claude Code's headers.
const placeholderToken = "janus-no-anthropic-account" // #nosec G101 -- a placeholder that grants nothing, and is sent nowhere

// printBanner says where janus is listening, what it will write, and how
// to start Claude Code through it.
func printBanner(w io.Writer, cfg config.Config, addr, configPath string) {
	base := "http://" + addr
	fmt.Fprintf(w, "%s\n\n", versionLine())
	fmt.Fprintf(w, "  Listening   %s\n", base)
	if cfg.Upstream.Off {
		fmt.Fprintf(w, "  Upstream    off; nothing is sent to Anthropic\n")
	} else {
		fmt.Fprintf(w, "  Upstream    %s\n", cfg.Upstream.BaseURL)
	}
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
	if cfg.Features.Compress {
		d := cfg.Compress.Digest
		minBytes := d.MinBytes
		if minBytes == 0 {
			minBytes = config.DefaultDigestMinBytes
		}
		which := "tool results"
		if len(d.Tools) > 0 {
			which = strings.Join(d.Tools, ", ") + " results"
		}
		_, model, _ := cfg.DigestModel()
		fmt.Fprintf(w, "  Compress    %s over %d bytes are digested by %s; originals kept in %s for janus mcp's tool_output\n",
			which, minBytes, model.Model, filepath.Join(cfg.Logging.Dir, "originals"))
	}
	fmt.Fprintf(w, "  Ledger      %s\n", filepath.Join(cfg.Logging.Dir, "ledger"))
	if cfg.Logging.Capture {
		fmt.Fprintf(w, "  Captures    %s (full prompts and responses)\n\n", filepath.Join(cfg.Logging.Dir, "captures"))
	} else {
		fmt.Fprintf(w, "  Captures    off\n\n")
	}
	fmt.Fprintf(w, "Start Claude Code through janus:\n\n")
	printClaudeCommand(w, base, configPath, cfg.Upstream.Off)
	fmt.Fprintf(w, "Press Ctrl+C to stop.\n\n")
}
