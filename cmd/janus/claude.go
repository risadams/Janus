// claude.go is `janus claude`: it runs Claude Code through Janus when Janus
// is up, and decides what to do when it is not, so that everyday sessions
// can go through Janus without Claude Code's settings pointing at it. A
// setting cuts Claude Code off from Anthropic whenever Janus is stopped; a
// launcher can look first (#232).

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/config"
)

// claudeProgram is the command janus claude runs, with any arguments that
// come before the user's. Tests replace it with a stand-in.
var claudeProgram = []string{"claude"}

// dialTimeout is how long janus claude waits for Janus to accept a
// connection before taking it to be down. Janus on loopback accepts in
// well under a millisecond.
const dialTimeout = time.Second

func runClaude(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return startClaude("janus claude", "", args, stdin, stdout, stderr)
}

// runLocal is `janus local`: Claude Code on the local model only, for
// this session, through the same running Janus.
func runLocal(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return startClaude("janus local", "local", args, stdin, stdout, stderr)
}

// providers maps what -provider accepts to the X-Janus-Route value that
// asks for it. "codex" is OpenAI by the name of its coding models.
var providers = map[string]string{
	"claude": "upstream",
	"openai": "openai",
	"codex":  "openai",
	"local":  "local",
}

// startClaude runs Claude Code through Janus. provider, if set, is the session's
// choice of provider, given to every request in the route header, which
// Janus never forwards: "openai" goes to OpenAI first and then along the
// rule's list, "local" to the local model only, "claude" to Claude only.
func startClaude(name, provider string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	ours, theirs := splitClaudeArgs(args)
	fs := newFlagSet(name, stderr)
	fs.Usage = func() {
		providerFlag := " [-provider claude|openai|local]"
		if provider != "" {
			providerFlag = ""
		}
		fmt.Fprintf(stderr, "Usage: %s [-config file] [-require]%s [--] [claude arguments]\n\n", name, providerFlag)
		if provider == "local" {
			fmt.Fprintf(stderr, "Runs Claude Code on the local model only, through Janus, for this session.\n\n")
		}
		fmt.Fprintf(stderr, ""+
			"Runs Claude Code through Janus if Janus is answering on its listen address.\n"+
			"If it is not, runs Claude Code directly, unless the guard is on or -require\n"+
			"is given. Arguments after janus's own go to claude unchanged; for Claude\n"+
			"Code's help, run janus claude -- -h.\n\n")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", config.DefaultPath, "path to the config file")
	require := fs.Bool("require", false, "refuse to run Claude Code when Janus is not answering")
	chosen := &provider
	if provider == "" {
		chosen = fs.String("provider", "", "send this session to claude, openai (or codex), or local first, instead of following the rules")
	}
	if err := fs.Parse(ours); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	// Read, not Load: starting Claude Code must not write a config file.
	cfg, err := config.Read(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, name+": %v\n", err)
		return 1
	}
	cfg = config.Apply(cfg, config.Overrides{}, os.Getenv)
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, name+": %s has problems:\n%v\n", *configPath, err)
		return 1
	}

	route := ""
	if *chosen != "" {
		var known bool
		if route, known = providers[*chosen]; !known {
			fmt.Fprintf(stderr, "%s: -provider %q is not claude, openai, codex, or local\n", name, *chosen)
			return 2
		}
	}

	base := clientURL(cfg.Listen)
	env := os.Environ()
	if route != "" {
		// The route header is an instruction to Janus, removed before
		// anything is sent on.
		headers := lookupEnv(env, "ANTHROPIC_CUSTOM_HEADERS")
		if headers != "" {
			headers += "\n"
		}
		env = setEnv(env, "ANTHROPIC_CUSTOM_HEADERS", headers+backend.RouteHeader+": "+route)
	}
	if up(cfg.Listen) {
		env = setEnv(env, "ANTHROPIC_BASE_URL", base)
		env = setEnv(env, "ENABLE_TOOL_SEARCH", "true")
		if cfg.Upstream.Off {
			env = setEnv(env, "ANTHROPIC_AUTH_TOKEN", placeholderToken)
		}
	} else {
		switch {
		case route != "":
			fmt.Fprintf(stderr, "%s: janus is not answering on %s, and only janus can send this session to\n"+
				"%s. Start janus, then run this again.\n", name, cfg.Listen, *chosen)
			return 1
		case cfg.Upstream.Off:
			fmt.Fprintf(stderr, name+": janus is not answering on %s, and upstream is off in %s:\n"+
				"there is no Anthropic account to go straight to, so Claude Code was not started.\n"+
				"Start janus, then run this again.\n", cfg.Listen, *configPath)
			return 1
		case cfg.Features.Guard:
			fmt.Fprintf(stderr, name+": janus is not answering on %s, and the guard is on in %s.\n"+
				"Going straight to Anthropic would send what the guard keeps back, so Claude Code\n"+
				"was not started. Start janus, then run this again.\n", cfg.Listen, *configPath)
			return 1
		case *require:
			fmt.Fprintf(stderr, name+": janus is not answering on %s, and -require was given.\n"+
				"Start janus, then run this again.\n", cfg.Listen)
			return 1
		}
		fmt.Fprintf(stderr, name+": janus is not answering on %s, so Claude Code is going straight\n"+
			"to Anthropic and this session will not be in the ledger. Start janus to change that.\n", cfg.Listen)
		// A variable naming the stopped Janus would leave Claude Code
		// unable to reach anything.
		if config.PointsAtListen(cfg.Listen, lookupEnv(env, "ANTHROPIC_BASE_URL")) {
			env = unsetEnv(env, "ANTHROPIC_BASE_URL")
		}
	}

	argv := append(append([]string(nil), claudeProgram...), theirs...)
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintf(stderr, name+": cannot find Claude Code (%q) on the PATH: %v\n", argv[0], err)
		return 1
	}
	cmd := exec.Command(path, argv[1:]...) // #nosec G204 G702 -- running the Claude Code the user asked for, with their arguments, is what janus claude is for
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, stdin, stdout, stderr

	// Ctrl-C reaches Claude Code directly, from the terminal. Janus waits
	// for it to decide what to do rather than dying underneath it.
	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)
	err = cmd.Run()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return exit.ExitCode()
	case err != nil:
		fmt.Fprintf(stderr, name+": %v\n", err)
		return 1
	}
	return 0
}

// splitClaudeArgs separates janus claude's own flags, which come first,
// from Claude Code's. The split is at "--", or at the first argument that
// is not one of janus claude's flags, so `janus claude --resume` passes
// --resume to Claude Code rather than failing on it.
func splitClaudeArgs(args []string) (ours, theirs []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return args[:i], args[i+1:]
		}
		if !strings.HasPrefix(arg, "-") {
			return args[:i], args[i:]
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		switch name {
		case "config", "provider":
			if !hasValue && i+1 < len(args) {
				i++
			}
		case "require", "h", "help":
		default:
			return args[:i], args[i:]
		}
	}
	return args, nil
}

// up reports whether something accepts connections on Janus's listen
// address. It sends nothing, so it neither reaches Anthropic nor adds to
// the ledger.
func up(listen string) bool {
	conn, err := net.DialTimeout("tcp", dialAddr(listen), dialTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close() // only opened to see that it could be
	return true
}

// dialAddr is the address a client on this machine uses to reach a
// listen address: one on every interface is reached on loopback.
func dialAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// clientURL is the base URL Claude Code is given for Janus.
func clientURL(listen string) string { return "http://" + dialAddr(listen) }

func lookupEnv(env []string, key string) string {
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == key {
			return v
		}
	}
	return ""
}

func unsetEnv(env []string, key string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != key {
			out = append(out, kv)
		}
	}
	return out
}

func setEnv(env []string, key, value string) []string {
	return append(unsetEnv(env, key), key+"="+value)
}
