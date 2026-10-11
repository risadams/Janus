package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/capture"
	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/ledger"
	"github.com/risadams/janus/internal/proxy"
)

func TestVersionLine(t *testing.T) {
	if got, want := versionLine(), "janus dev"; got != want {
		t.Errorf("versionLine() = %q, want %q", got, want)
	}
}

func TestRunVersion(t *testing.T) {
	for _, arg := range []string{"version", "-version", "--version"} {
		var out, errOut bytes.Buffer
		if code := run([]string{arg}, &out, &errOut); code != 0 || out.String() != "janus dev\n" {
			t.Errorf("run(%q) = %d, stdout %q, stderr %q", arg, code, out.String(), errOut.String())
		}
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	cfgPath := filepath.Join(t.TempDir(), "c.json")
	if code := run([]string{"-config", cfgPath, "bogus"}, &out, &errOut); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), `unknown command "bogus"`) {
		t.Errorf("stderr = %q", errOut.String())
	}
	if _, err := os.Stat(cfgPath); err == nil {
		t.Error("a starter config was written for a command that was rejected")
	}
}

func TestRunReportsInvalidConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(cfgPath, []byte(`{"listen":"0.0.0.0:8787","features":{"route":true}}`), 0o600)

	var out, errOut bytes.Buffer
	if code := run([]string{"-config", cfgPath}, &out, &errOut); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	for _, want := range []string{cfgPath, "allowRemote", "features.route"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr = %q, missing %q", errOut.String(), want)
		}
	}
}

func TestBannerGivesTheClaudeCodeCommand(t *testing.T) {
	var out bytes.Buffer
	printBanner(&out, config.Default(), "127.0.0.1:8787")
	for _, want := range []string{
		"ANTHROPIC_BASE_URL=http://127.0.0.1:8787 ENABLE_TOOL_SEARCH=true claude",
		`$env:ANTHROPIC_BASE_URL="http://127.0.0.1:8787"`,
		"https://api.anthropic.com",
		"http://studio.tuesday.local:1234",
		filepath.Join("logs", "ledger"),
		filepath.Join("logs", "captures"),
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("banner is missing %q:\n%s", want, out.String())
		}
	}
}

func TestFormatExchange(t *testing.T) {
	n := func(i int) *int { return &i }
	base := proxy.Exchange{Method: "POST", Path: "/v1/messages", Status: 200, Duration: 2065 * time.Millisecond}
	with := func(edit func(*proxy.Exchange)) proxy.Exchange { ex := base; edit(&ex); return ex }

	cases := map[string]struct {
		ex   proxy.Exchange
		want string
	}{
		"not a Messages response": {
			proxy.Exchange{Method: "HEAD", Path: "/api/hello", Status: 200, Duration: 154 * time.Millisecond},
			"HEAD /api/hello -> 200 (154ms)",
		},
		"upstream usage": {
			with(func(ex *proxy.Exchange) {
				ex.Summary = &anthropic.Summary{Model: "claude-opus-5-5", StopReason: "tool_use", Complete: true,
					Usage: anthropic.Usage{Input: 2, Output: 364, CacheWrite: n(876), CacheRead: n(28730)}}
			}),
			"POST /v1/messages -> 200 (2.065s) claude-opus-5-5 tool_use in=2 cache_write=876 cache_read=28730 out=364",
		},
		"usage fields the server did not send are left out": {
			with(func(ex *proxy.Exchange) {
				ex.Summary = &anthropic.Summary{Model: "qwen/qwen3.8-27b", StopReason: "end_turn", Complete: true,
					Usage: anthropic.Usage{Input: 17, Output: 3}}
			}),
			"POST /v1/messages -> 200 (2.065s) qwen/qwen3.8-27b end_turn in=17 out=3",
		},
		"cut off": {
			with(func(ex *proxy.Exchange) {
				ex.Summary = &anthropic.Summary{Model: "claude-opus-5-5", Usage: anthropic.Usage{Input: 2, Output: 4}}
			}),
			"POST /v1/messages -> 200 (2.065s) claude-opus-5-5 in=2 out=4 (incomplete)",
		},
		"api error": {
			with(func(ex *proxy.Exchange) {
				ex.Status = 429
				ex.Summary = &anthropic.Summary{Complete: true, Error: &anthropic.APIError{Type: "rate_limit_error"}}
			}),
			"POST /v1/messages -> 429 (2.065s) error=rate_limit_error",
		},
		"answered locally": {
			with(func(ex *proxy.Exchange) {
				ex.Backend = "local"
				ex.Attempt = &proxy.Attempt{Backend: "local", Accepted: true}
				ex.Summary = &anthropic.Summary{Model: "qwen/qwen3.8-27b", StopReason: "end_turn", Complete: true, Usage: anthropic.Usage{Input: 17, Output: 3}}
			}),
			"POST /v1/messages -> 200 (2.065s) [local] qwen/qwen3.8-27b end_turn in=17 out=3",
		},
		"escalated": {
			with(func(ex *proxy.Exchange) {
				ex.Backend = "upstream"
				ex.Attempt = &proxy.Attempt{Backend: "local", Why: "the local answer has no text and no tool call", Duration: 9340 * time.Millisecond}
				ex.Summary = &anthropic.Summary{Model: "claude-opus-5-5", StopReason: "end_turn", Complete: true, Usage: anthropic.Usage{Input: 2, Output: 9}}
			}),
			"POST /v1/messages -> 200 (2.065s) [after local tried for 9.3s: the local answer has no text and no tool call] claude-opus-5-5 end_turn in=2 out=9",
		},
		"unreadable encoding": {
			with(func(ex *proxy.Exchange) { ex.Unread = "content-encoding br" }),
			"POST /v1/messages -> 200 (2.065s) response not read: content-encoding br",
		},
	}
	for name, c := range cases {
		if got := formatExchange(c.ex); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", name, got, c.want)
		}
	}
}

func messagesExchange() proxy.Exchange {
	header := http.Header{}
	header.Set("X-Claude-Code-Session-Id", "sess-1")
	header.Set("Authorization", "Bearer secret-token")
	return proxy.Exchange{
		ID: "20261011T143927.123-ab12cd", Start: time.Date(2026, 10, 11, 14, 39, 27, 0, time.UTC),
		Method: "POST", Path: "/v1/messages", Header: header, Status: 200, Duration: time.Second,
		RequestBody:  []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`),
		ResponseBody: []byte(`{"type":"message"}`),
		Summary:      &anthropic.Summary{Model: "claude-opus-5-5", StopReason: "end_turn", Complete: true},
	}
}

func TestRecordLinksLedgerToCapture(t *testing.T) {
	dir := t.TempDir()
	var console bytes.Buffer
	led := ledger.Open(filepath.Join(dir, "ledger"))
	captures := capture.New(filepath.Join(dir, "captures"), 7, 1024)

	record(messagesExchange(), led, captures, log.New(&console, "", 0))

	line, err := os.ReadFile(filepath.Join(dir, "ledger", "2026-10-11.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var rec ledger.Record
	if err := json.Unmarshal(line, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Class != "main" || rec.Capture == "" {
		t.Fatalf("record = %+v, want class main and a capture name", rec)
	}
	if _, err := os.Stat(filepath.Join(dir, "captures", rec.Capture+".md")); err != nil {
		t.Errorf("the ledger names a capture that is not there: %v", err)
	}
	if !strings.HasPrefix(console.String(), "POST /v1/messages -> 200") {
		t.Errorf("console = %q", console.String())
	}
}

func TestRecordWithoutCaptures(t *testing.T) {
	dir := t.TempDir()
	record(messagesExchange(), ledger.Open(filepath.Join(dir, "ledger")), nil, log.New(io.Discard, "", 0))

	line, _ := os.ReadFile(filepath.Join(dir, "ledger", "2026-10-11.jsonl"))
	if !bytes.Contains(line, []byte(`"class":"main"`)) || bytes.Contains(line, []byte(`"capture"`)) {
		t.Errorf("ledger line = %s, want a record with no capture field", line)
	}
	if _, err := os.Stat(filepath.Join(dir, "captures")); err == nil {
		t.Error("a captures directory was created with capture off")
	}
}

// A disk that cannot be written to must cost the log lines and nothing
// else: record has no way to fail the request, and must not panic.
func TestRecordSurvivesUnwritableDisk(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o600)
	var console bytes.Buffer

	record(messagesExchange(), ledger.Open(filepath.Join(blocker, "ledger")),
		capture.New(filepath.Join(blocker, "captures"), 7, 1024), log.New(&console, "", 0))

	out := console.String()
	if strings.Count(out, "janus: ") != 2 || !strings.Contains(out, "POST /v1/messages -> 200") {
		t.Errorf("console = %q, want two failures reported and the request line still printed", out)
	}
}

func testConfigWithRules() config.Config {
	yes := true
	cfg := config.Default()
	cfg.Features.Route = true
	cfg.Local.Models = map[string]config.LocalModel{"janus-local": {Model: "qwen/qwen3.8-27b"}}
	cfg.Routing = []config.Rule{
		{Name: "small-subagents", Class: "subagent", MaxTokens: 40000, Tools: &yes, Route: "local", Local: "janus-local"},
		{Model: "claude-haiku-5-5", Route: "local-then-escalate", Local: "janus-local"},
		{Name: "rest", Route: "upstream"},
	}
	return cfg
}

func TestNewRouterResolvesRulesAndTimeouts(t *testing.T) {
	cfg := testConfigWithRules()
	cfg.Local.Models["quick"] = config.LocalModel{Model: "gemma", TimeoutSeconds: 30, ContextTokens: 8000}
	r := newRouter(cfg, nil, nil)

	if got := r.Aliases["janus-local"]; got.ID != "qwen/qwen3.8-27b" || got.Timeout != time.Duration(config.DefaultLocalTimeoutSeconds)*time.Second {
		t.Errorf("janus-local = %+v, want the default timeout", got)
	}
	if got := r.Aliases["quick"]; got.Timeout != 30*time.Second || got.ContextTokens != 8000 {
		t.Errorf("quick = %+v", got)
	}
	if len(r.Rules) != 3 || r.Rules[0].Local.ID != "qwen/qwen3.8-27b" || r.Rules[0].Class != "subagent" || r.Rules[0].MaxTokens != 40000 || *r.Rules[0].Tools != true {
		t.Errorf("rules[0] = %+v", r.Rules[0])
	}
	if r.Rules[1].TrialTimeout != time.Duration(config.DefaultTrialSeconds)*time.Second || r.Rules[0].TrialTimeout != 0 {
		t.Errorf("trial timeouts: escalate rule %s (want the default), local rule %s (want none)", r.Rules[1].TrialTimeout, r.Rules[0].TrialTimeout)
	}
	if r.Rules[2].IfDown != nil {
		t.Errorf("a rule with no ifDown got one: %+v", r.Rules[2].IfDown)
	}
	cfg.Routing[2].IfDown = "quick"
	if got := newRouter(cfg, nil, nil).Rules[2].IfDown; got == nil || got.ID != "gemma" {
		t.Errorf("ifDown was not resolved: %+v", got)
	}
	if r.Rules[2].Route != "upstream" || r.Rules[2].Local.ID != "" {
		t.Errorf("rules[2] = %+v", r.Rules[2])
	}
}

func TestRecordSavesARejectedLocalAnswer(t *testing.T) {
	dir := t.TempDir()
	captures := capture.New(filepath.Join(dir, "captures"), 7, 1024)
	ex := messagesExchange()
	ex.Backend, ex.Route = "upstream", "upstream"
	ex.Attempt = &proxy.Attempt{Backend: "local", Model: "qwen/qwen3.8-27b", Why: "empty", ResponseBody: []byte(`{"content":[]}`)}

	rec := record(ex, ledger.Open(filepath.Join(dir, "ledger")), captures, log.New(io.Discard, "", 0))
	if rec.Attempt == nil || rec.Attempt.Verdict != ledger.VerdictRejected {
		t.Errorf("record attempt = %+v", rec.Attempt)
	}
	saved, err := os.ReadFile(filepath.Join(dir, "captures", rec.Capture+".attempt.response.txt"))
	if err != nil || string(saved) != `{"content":[]}` {
		t.Errorf("the rejected answer was not saved: %q, %v", saved, err)
	}
}
