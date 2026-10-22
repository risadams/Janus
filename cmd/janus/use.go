// use.go is `janus use`: it moves the next turn of a session, or of every
// session, to a provider, by asking the running janus (#379).

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/route"
)

func runUse(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("janus use", stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: janus use [flags] claude|codex|openai|local")
		fmt.Fprintln(stderr, "       janus use -clear [-session id]")
		fmt.Fprintln(stderr, "       janus use -list")
		fmt.Fprintln(stderr, "\nMoves the next turn of every session, or of one with -session, to a provider,")
		fmt.Fprintln(stderr, "until -clear or until that provider cannot answer. The session id is the one")
		fmt.Fprintln(stderr, "the ledger records as \"session\".")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", config.DefaultPath, "path to the config file")
	session := fs.String("session", "", "the session to move; every session if left out")
	clear := fs.Bool("clear", false, "drop the choice, so the rules decide again")
	list := fs.Bool("list", false, "print the choices in force")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var u route.Use
	switch {
	case *list && fs.NArg() == 0 && !*clear:
	case *clear && fs.NArg() == 0:
		u = route.Use{Clear: true, Session: *session}
	case !*clear && !*list && fs.NArg() == 1:
		if _, ok := route.UseProviders[fs.Arg(0)]; !ok {
			fmt.Fprintf(stderr, "janus use: %q is not claude, codex, openai, or local\n", fs.Arg(0))
			return 2
		}
		u = route.Use{Provider: fs.Arg(0), Session: *session}
	default:
		fs.Usage()
		return 2
	}

	cfg, err := config.Read(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "janus use: %v\n", err)
		return 1
	}
	cfg = config.Apply(cfg, config.Overrides{}, os.Getenv)
	url := clientURL(cfg.Listen) + route.ControlPath
	client := &http.Client{Timeout: 5 * time.Second}
	var resp *http.Response
	if *list {
		resp, err = client.Get(url)
	} else {
		body, _ := json.Marshal(u) // strings and a bool
		resp, err = client.Post(url, "application/json", bytes.NewReader(body))
	}
	if err != nil {
		fmt.Fprintf(stderr, "janus use: janus is not answering on %s: %v. Start janus first.\n", cfg.Listen, err)
		return 1
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "janus use: %s\n", strings.TrimPrefix(strings.TrimSpace(string(data)), "janus: "))
		return 1
	}
	var chosen map[string]string
	if err := json.Unmarshal(data, &chosen); err != nil {
		fmt.Fprintf(stderr, "janus use: the answer could not be read: %v\n", err)
		return 1
	}
	lines := route.Choices(chosen)
	if len(lines) == 0 {
		fmt.Fprintln(stdout, "No provider is chosen by hand; the routing rules decide.")
		return 0
	}
	fmt.Fprintln(stdout, "Chosen by hand, until cleared or until the provider cannot answer:")
	for _, line := range lines {
		fmt.Fprintf(stdout, "  %s\n", line)
	}
	return 0
}
