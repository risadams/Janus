// mcp.go — the `janus mcp` command: an MCP server offering Claude tools
// that the local model carries out.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/digest"
	"github.com/risadams/janus/internal/ledger"
	"github.com/risadams/janus/internal/mcp"
	"github.com/risadams/janus/internal/worker"
)

func runMCP(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("janus mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: janus mcp -config <file> [-local name] [-root dir]")
		fmt.Fprintln(stderr, "\nRuns an MCP server on standard input and output, for Claude Code to start.")
		fmt.Fprintln(stderr, "It offers tools that the local model carries out, so that the files they read")
		fmt.Fprintln(stderr, "never enter Claude's context.")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", config.DefaultPath, "path to the config file; give it in full, since Claude Code starts this in your project directory")
	modelName := fs.String("local", "", "which entry of local.models does the work (default: the only one)")
	root := fs.String("root", "", "the directory the tools may read (default: the directory this is started in)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}

	// Standard output belongs to the protocol. Everything janus has to say
	// goes to standard error, which Claude Code keeps in its MCP log.
	logger := log.New(stderr, "janus mcp: ", 0)

	cfg, err := config.Read(*configPath)
	if err == nil {
		cfg = config.Apply(cfg, config.Overrides{}, os.Getenv)
		err = cfg.Validate()
	}
	if err != nil {
		logger.Print(err)
		return 1
	}
	name, model, err := pickLocalModel(cfg, *modelName)
	if err != nil {
		logger.Printf("%v (looked in %s)", err, *configPath)
		return 2
	}
	if *root == "" {
		if *root, err = os.Getwd(); err != nil {
			logger.Print(err)
			return 1
		}
	}
	if info, err := os.Stat(*root); err != nil || !info.IsDir() {
		logger.Printf("-root %s is not a directory", *root)
		return 2
	}

	// The ledger goes where the proxy's does: logging.dir, which if it is
	// relative is relative to the config file and not to the project this
	// was started in.
	logDir := cfg.Logging.Dir
	if !filepath.IsAbs(logDir) {
		abs, _ := filepath.Abs(*configPath)
		logDir = filepath.Join(filepath.Dir(abs), logDir)
	}
	led := ledger.Open(filepath.Join(logDir, "ledger"))

	seconds := model.TimeoutSeconds
	if seconds == 0 {
		seconds = config.DefaultLocalTimeoutSeconds
	}
	localURL, _ := url.Parse(cfg.Local.BaseURL)
	w := &worker.Worker{
		Model: &worker.Model{Local: backend.NewLocal(localURL, nil), ID: model.Model, Timeout: time.Duration(seconds) * time.Second},
		Root:  *root,
		Record: func(c worker.Call) {
			if err := led.Append(workerRecord(c, model.Model)); err != nil {
				logger.Print(err)
			}
			logger.Printf("%s: %d files, %d bytes read, %d model calls, %s, failed=%v", c.Tool, c.Files, c.BytesRead, c.ModelCalls, c.Duration.Round(100*time.Millisecond), c.Failed)
		},
	}
	logger.Printf("serving tools on %s (%s) at %s, reading under %s", model.Model, name, cfg.Local.BaseURL, *root)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Serve returns when the client closes its end. A signal has to stop
	// janus too, and a read from standard input cannot be interrupted, so
	// the wait is for whichever comes first.
	served := make(chan error, 1)
	// tool_output fetches the originals of tool results the proxy
	// digested; it reads them from where the proxy keeps them.
	tools := append(w.Tools(), digest.Tool(digest.NewStore(filepath.Join(logDir, "originals"), cfg.Logging.RetainDays)))
	go func() { served <- mcp.Serve(ctx, stdin, stdout, "janus", version, tools) }()
	select {
	case err := <-served:
		if err != nil {
			logger.Print(err)
			return 1
		}
	case <-ctx.Done():
	}
	return 0
}

// workerRecord is the ledger record for one piece of delegated work.
func workerRecord(c worker.Call, model string) ledger.Record {
	var suffix [3]byte
	rand.Read(suffix[:])
	rec := ledger.Record{
		V:          ledger.SchemaVersion,
		ID:         c.Start.UTC().Format("20060102T150405.000") + "-" + hex.EncodeToString(suffix[:]),
		Time:       c.Start.UTC(),
		Method:     "TOOL",
		Path:       c.Tool,
		Class:      "worker",
		Route:      "local",
		Reason:     "mcp tool",
		Stages:     []string{},
		Backend:    "local",
		Outcome:    ledger.OutcomeOK,
		DurationMs: c.Duration.Milliseconds(),
		Response:   &ledger.Response{Model: model, Blocks: []string{}},
		Usage:      &ledger.Usage{Input: c.Usage.Input, Output: c.Usage.Output},
		Worker: &ledger.Worker{
			Tool: c.Tool, Files: c.Files, ReadBytes: c.BytesRead, ModelCalls: c.ModelCalls, AnswerBytes: c.AnswerBytes,
		},
	}
	if c.Failed {
		rec.Outcome, rec.Reason = ledger.OutcomeToolError, "mcp tool: "+c.Why
	} else {
		rec.Worker.AvoidedTokens = ledger.AvoidedTokens(c.BytesRead, c.AnswerBytes)
	}
	return rec
}
