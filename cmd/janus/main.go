// main.go — entry point for the janus binary.
//
// For now janus is a transparent proxy: it loads its config, listens on
// loopback, and forwards everything to the upstream API untouched. Routing
// to the local model, and everything built on it, arrives in later
// milestones.
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
	"syscall"
	"time"

	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/proxy"
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

	// Validate has already established that this parses.
	upstream, _ := url.Parse(cfg.Upstream.BaseURL)

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "janus: could not listen on %s: %v\n", cfg.Listen, err)
		return 1
	}
	printBanner(stdout, cfg, ln.Addr().String())

	logger := log.New(stderr, "", log.LstdFlags)
	srv := &http.Server{Handler: proxy.New(upstream, logger.Printf)}

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

func versionLine() string {
	return "janus " + version
}

// printBanner says where janus is listening and gives the exact command
// that points Claude Code at it, so nobody has to look up the variable.
//
// ENABLE_TOOL_SEARCH is set because Claude Code turns tool search off when
// its base URL is not Anthropic's own, and then sends every tool schema in
// every request; setting it keeps the traffic shaped the way it is without
// a proxy.
func printBanner(w io.Writer, cfg config.Config, addr string) {
	base := "http://" + addr
	fmt.Fprintf(w, "%s\n\n", versionLine())
	fmt.Fprintf(w, "  Listening   %s\n", base)
	fmt.Fprintf(w, "  Upstream    %s\n", cfg.Upstream.BaseURL)
	fmt.Fprintf(w, "  Local       %s (not used yet)\n\n", cfg.Local.BaseURL)
	fmt.Fprintf(w, "Point Claude Code at janus:\n\n")
	fmt.Fprintf(w, "  # bash / zsh / fish\n")
	fmt.Fprintf(w, "  ANTHROPIC_BASE_URL=%s ENABLE_TOOL_SEARCH=true claude\n\n", base)
	fmt.Fprintf(w, "  # PowerShell\n")
	fmt.Fprintf(w, "  $env:ANTHROPIC_BASE_URL=\"%s\"; $env:ENABLE_TOOL_SEARCH=\"true\"; claude\n\n", base)
	fmt.Fprintf(w, "Press Ctrl+C to stop.\n\n")
}
