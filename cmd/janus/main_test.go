package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/capture"
	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/guard"
	"github.com/risadams/janus/internal/instance"
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

// A second Janus on a log folder already in use stops before listening and
// says which Janus holds it.
func TestRunRefusesALogFolderInUse(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "janus.config.json")
	os.WriteFile(cfgPath, []byte(`{"listen":"127.0.0.1:0","logging":{"dir":"logs"}}`), 0o600)
	held, err := instance.Acquire(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	held.Describe("pid 4242, listening on http://127.0.0.1:8787")

	var out, errOut bytes.Buffer
	if code := run([]string{"-config", cfgPath}, &out, &errOut); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	for _, want := range []string{"another janus (pid 4242, listening on http://127.0.0.1:8787)", filepath.Join(root, "logs"), "Stop that one first", "logging.dir"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr = %q, missing %q", errOut.String(), want)
		}
	}
	if strings.Contains(out.String(), "Listening") {
		t.Errorf("printed a banner: %q", out.String())
	}
}

func TestRunExplainsABusyPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfgPath := filepath.Join(t.TempDir(), "janus.config.json")
	os.WriteFile(cfgPath, []byte(`{"listen":"`+ln.Addr().String()+`"}`), 0o600)

	var out, errOut bytes.Buffer
	if code := run([]string{"-config", cfgPath}, &out, &errOut); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "perhaps a janus with a different log folder") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestBannerGivesTheClaudeCodeCommand(t *testing.T) {
	var out bytes.Buffer
	printBanner(&out, config.Default(), "127.0.0.1:8787", config.DefaultPath)
	for _, want := range []string{
		"  janus claude\n",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:8787 ENABLE_TOOL_SEARCH=true claude",
		`$env:ANTHROPIC_BASE_URL="http://127.0.0.1:8787"`,
		"https://api.anthropic.com",
		"http://studio.local:1234",
		filepath.Join(config.StateDir(), "ledger"),
		filepath.Join(config.StateDir(), "captures"),
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("banner is missing %q:\n%s", want, out.String())
		}
	}
}

func TestBannerPassesJanusClaudeTheSameConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "elsewhere.json")
	var out bytes.Buffer
	printBanner(&out, config.Default(), "127.0.0.1:8787", path)
	if want := "janus claude -config " + path + "\n"; !strings.Contains(out.String(), want) {
		t.Errorf("banner is missing %q:\n%s", want, out.String())
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

	record(messagesExchange(), led, captures, nil, testConsole(&console))

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
	if !strings.Contains(console.String(), "main       Opus 5.5") {
		t.Errorf("console = %q", console.String())
	}
}

func TestRecordWithoutCaptures(t *testing.T) {
	dir := t.TempDir()
	record(messagesExchange(), ledger.Open(filepath.Join(dir, "ledger")), nil, nil, testConsole(io.Discard))

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
		capture.New(filepath.Join(blocker, "captures"), 7, 1024), nil, testConsole(&console))

	out := console.String()
	if strings.Count(out, "janus: ") != 2 || !strings.Contains(out, "main       Opus 5.5") {
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
	r := newRouter(cfg, nil, nil, nil)

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
	if r.Rules[2].Fallback != nil {
		t.Errorf("a rule with no fallback got one: %+v", r.Rules[2].Fallback)
	}
	cfg.Routing[2].Fallback = []string{"upstream", "quick"}
	got := newRouter(cfg, nil, nil, nil).Rules[2].Fallback
	if len(got) != 2 || got[0].Name != "upstream" || got[0].Local != nil || got[1].Local == nil || got[1].Local.ID != "gemma" {
		t.Errorf("fallback was not resolved: %+v", got)
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

	rec := record(ex, ledger.Open(filepath.Join(dir, "ledger")), captures, nil, testConsole(io.Discard))
	if rec.Attempt == nil || rec.Attempt.Verdict != ledger.VerdictRejected {
		t.Errorf("record attempt = %+v", rec.Attempt)
	}
	saved, err := os.ReadFile(filepath.Join(dir, "captures", rec.Capture+".attempt.response.txt"))
	if err != nil || string(saved) != `{"content":[]}` {
		t.Errorf("the rejected answer was not saved: %q, %v", saved, err)
	}
}

// A key assembled at run time, so that nothing credential-shaped sits in
// the repository.
var fakeAWSKey = strings.Join([]string{"AKIA", "Q7RZ2LP9XVB4NKD8"}, "")

func exchangeCarrying(secret string) proxy.Exchange {
	ex := messagesExchange()
	ex.Backend, ex.Route = "upstream", "upstream"
	body, _ := json.Marshal(map[string]any{
		"model": "claude-opus-5-5",
		"messages": []map[string]any{
			{"role": "user", "content": "deploy it"},
			{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "t1", "content": "aws_access_key_id = " + secret}}},
		},
	})
	ex.RequestBody = body
	return ex
}

func TestRecordNotesSecretsGoingUpstreamWithoutRepeatingThem(t *testing.T) {
	dir := t.TempDir()
	var console bytes.Buffer
	rec := record(exchangeCarrying(fakeAWSKey), ledger.Open(filepath.Join(dir, "ledger")), nil, &guard.Detector{}, testConsole(&console))

	g := rec.Guard
	if g == nil || g.Findings != 1 || g.Kinds["aws_access_key"] != 1 || len(g.Where) != 1 || !strings.Contains(g.Where[0], "messages[1] user (tool_result)") {
		t.Fatalf("guard = %+v", g)
	}
	if !strings.Contains(console.String(), "↳ guard: 1 found going upstream: aws_access_key") {
		t.Errorf("console = %q", console.String())
	}
	line, _ := os.ReadFile(filepath.Join(dir, "ledger", "2026-10-11.jsonl"))
	if !bytes.Contains(line, []byte(`"guard":{"findings":1`)) {
		t.Errorf("ledger line has no guard field: %s", line)
	}
	for name, text := range map[string]string{"console": console.String(), "ledger": string(line)} {
		if strings.Contains(text, fakeAWSKey) || strings.Contains(text, fakeAWSKey[:10]) {
			t.Errorf("the %s contains the secret", name)
		}
	}
}

func TestGuardScansOnlyWhatWentUpstream(t *testing.T) {
	d := &guard.Detector{}
	upstream := exchangeCarrying(fakeAWSKey)

	local := upstream
	local.Backend = "local"
	stoodIn := local
	stoodIn.Attempt = &proxy.Attempt{Backend: "upstream", Fault: true}
	escalated := upstream
	escalated.Attempt = &proxy.Attempt{Backend: "local", Why: "empty"}
	notMessages := upstream
	notMessages.Path = "/v1/messages/count_tokens"
	truncated := upstream
	truncated.RequestTruncated = true
	clean := exchangeCarrying("nothing-secret-here")

	cases := map[string]struct {
		ex          proxy.Exchange
		detector    *guard.Detector
		wantScanned bool
		wantFound   int
	}{
		"sent upstream":                         {upstream, d, true, 1},
		"answered locally, never sent upstream": {local, d, false, 0},
		"sent upstream, then local stood in":    {stoodIn, d, true, 1},
		"tried locally, then sent upstream":     {escalated, d, true, 1},
		"not a Messages call":                   {notMessages, d, false, 0},
		"too large to have been kept whole":     {truncated, d, false, 0},
		"nothing in it":                         {clean, d, true, 0},
		"guard off":                             {upstream, nil, false, 0},
	}
	for name, c := range cases {
		report, scanned := scanForSecrets(c.detector, c.ex)
		if scanned != c.wantScanned || report.Findings != c.wantFound {
			t.Errorf("%s: scanned=%v findings=%d, want %v and %d", name, scanned, report.Findings, c.wantScanned, c.wantFound)
		}
	}

	// A clean request leaves no guard field at all.
	rec := record(clean, ledger.Open(filepath.Join(t.TempDir(), "ledger")), nil, d, testConsole(io.Discard))
	if rec.Guard != nil {
		t.Errorf("a clean request got a guard field: %+v", rec.Guard)
	}
}

func TestNewDetectorUsesTheConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Guard = config.Guard{Patterns: []string{`ACME-[0-9]{6}`}, Allow: []string{fakeAWSKey}, Entropy: true}
	d := newDetector(cfg)

	text := "order ACME-204857 with key " + fakeAWSKey
	found := d.Scan(text)
	if len(found) != 1 || found[0].Kind != "custom" || !d.Entropy {
		t.Errorf("findings = %+v, entropy = %v; want the custom pattern only, the key being allowed", found, d.Entropy)
	}
}

func TestRecordUsesTheRedactorsOwnAccount(t *testing.T) {
	dir := t.TempDir()
	var console bytes.Buffer
	ex := exchangeCarrying(fakeAWSKey)
	ex.Guard = &proxy.Guard{Scanned: true, Report: guard.Report{Findings: 1, Kinds: map[string]int{"aws_access_key": 1}, Where: []string{"messages[1] user (tool_result)"}}, Replaced: 1, Restored: 2}

	// A detector is passed too, to show the redactor's account takes
	// precedence over scanning the request again.
	rec := record(ex, ledger.Open(filepath.Join(dir, "ledger")), nil, &guard.Detector{}, testConsole(&console))
	g := rec.Guard
	if g == nil || g.Findings != 1 || g.Redacted == nil || *g.Redacted != 1 || g.Restored != 2 || g.Blocked != "" {
		t.Fatalf("guard = %+v", g)
	}
	if !strings.Contains(console.String(), "↳ guard: 1 kept from going upstream (aws_access_key), 2 put back in the answer") {
		t.Errorf("console = %q", console.String())
	}
	if strings.Contains(console.String(), fakeAWSKey) {
		t.Error("the console contains the secret")
	}

	blocked := exchangeCarrying("x")
	blocked.Guard = &proxy.Guard{Blocked: "the request could not be read to check it for secrets"}
	console.Reset()
	rec = record(blocked, ledger.Open(filepath.Join(dir, "ledger")), nil, nil, testConsole(&console))
	if rec.Guard == nil || rec.Guard.Blocked == "" || !strings.Contains(console.String(), "↳ guard: stopped: the request could not be read") {
		t.Errorf("guard = %+v, console = %q", rec.Guard, console.String())
	}

	// A redactor that found nothing leaves no guard field.
	clean := exchangeCarrying("x")
	clean.Guard = &proxy.Guard{}
	if rec := record(clean, ledger.Open(filepath.Join(dir, "ledger")), nil, nil, testConsole(io.Discard)); rec.Guard != nil {
		t.Errorf("a clean request got a guard field: %+v", rec.Guard)
	}
}
