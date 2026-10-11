// route.go — the `janus route` command: say where a saved request would be
// sent, and why.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/route"
)

func runRoute(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("janus route", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: janus route [-config file] [-class main|subagent|unknown] <request file>")
		fmt.Fprintln(stderr, "\nSays where a saved Messages request would be sent, and why. The request file")
		fmt.Fprintln(stderr, "is a request body, such as a capture's .request.txt.")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", config.DefaultPath, "path to the config file")
	class := fs.String("class", "", "treat the request as this class instead of working it out")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}

	cfg, err := config.Read(*configPath)
	if err == nil {
		err = config.Apply(cfg, config.Overrides{}, os.Getenv).Validate()
	}
	if err != nil {
		fmt.Fprintf(stderr, "janus route: %v\n", err)
		return 1
	}
	cfg = config.Apply(cfg, config.Overrides{}, os.Getenv)

	body, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "janus route: %v\n", err)
		return 1
	}
	req, err := anthropic.ParseRequest(body)
	if err != nil {
		fmt.Fprintf(stderr, "janus route: %s is not a Messages request body: %v\n", fs.Arg(0), err)
		return 1
	}

	header, how, err := headerFor(*class, req)
	if err != nil {
		fmt.Fprintf(stderr, "janus route: %v\n", err)
		return 2
	}
	facts := route.FactsOf(header, req)
	tools := "no tools"
	if n := len(req.Tools()); n > 0 {
		tools = fmt.Sprintf("%d tools", n)
	}
	fmt.Fprintf(stdout, "Request   %s, class %s (%s), about %s tokens, %s\n\n", facts.Model, facts.Class, how, commas(facts.Tokens), tools)

	if !cfg.Features.Route {
		fmt.Fprintf(stdout, "Routing is off (features.route), so this request goes upstream untouched.\nWith it on:\n\n")
	}

	upstreamURL, _ := url.Parse(cfg.Upstream.BaseURL)
	localURL, _ := url.Parse(cfg.Local.BaseURL)
	router := newRouter(cfg, backend.NewUpstream(upstreamURL, nil), backend.NewLocal(localURL, nil))
	plan, refusal, trace := router.Explain(context.Background(), header, req)

	for _, line := range trace {
		fmt.Fprintf(stdout, "  %s\n", line)
	}
	fmt.Fprintln(stdout)
	switch {
	case refusal != nil:
		fmt.Fprintf(stdout, "Decision  refused with HTTP %d: %s\n", refusal.Status, refusal.Message)
	case plan.Backend.Name() == "local" && plan.Accept != nil:
		fmt.Fprintf(stdout, "Decision  local first: %s answers, and its whole answer is checked before the client\n", plan.BackendModel)
		fmt.Fprintf(stdout, "          sees any of it. If it passes it is reported as %s; if not, Claude answers.\n", plan.ResponseModel)
		fmt.Fprintf(stdout, "          changes on the way: %s\n", strings.Join(plan.Stages, ", "))
	case plan.Backend.Name() == "local":
		fmt.Fprintf(stdout, "Decision  local: %s answers, reported to the client as %s\n", plan.BackendModel, plan.ResponseModel)
		fmt.Fprintf(stdout, "          changes on the way: %s\n", strings.Join(plan.Stages, ", "))
		if plan.Else != nil {
			fmt.Fprintf(stdout, "          if it cannot be reached or answers with an error, Claude answers instead\n")
		}
	default:
		fmt.Fprintf(stdout, "Decision  upstream, untouched\n")
		if plan.Shadow != nil {
			fmt.Fprintf(stdout, "          then run again on %s for comparison; that answer is only recorded\n", plan.Shadow.Model)
		}
	}
	return 0
}

// headerFor builds the headers that give a saved request its class. A
// request body does not carry its headers, so the class is either given or
// read from the billing line Claude Code puts at the top of the system
// prompt, which says whether the request came from a subagent.
func headerFor(class string, req *anthropic.Request) (header http.Header, how string, err error) {
	how = "as given"
	if class == "" {
		how = "from the request's first system block"
		first := ""
		if system := req.System(); len(system) > 0 {
			first = system[0].Text()
		}
		switch {
		case strings.Contains(first, "cc_is_subagent"):
			class = string(route.Subagent)
		case strings.Contains(first, "cc_entrypoint"):
			class = string(route.Main)
		default:
			class, how = string(route.Unknown), "it does not look like a Claude Code request"
		}
	}

	header = http.Header{}
	switch route.Class(class) {
	case route.Subagent:
		header.Set("X-Claude-Code-Session-Id", "explain")
		header.Set("X-Claude-Code-Agent-Id", "explain")
	case route.Main:
		header.Set("X-Claude-Code-Session-Id", "explain")
	case route.Unknown:
	default:
		return nil, "", fmt.Errorf("-class %q is not one of main, subagent, unknown", class)
	}
	return header, how, nil
}
