package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/ledger"
	"github.com/risadams/janus/internal/openai"
	"github.com/risadams/janus/internal/openai/openaitest"
	"github.com/risadams/janus/internal/proxy"
)

// With no Anthropic account behind janus (upstream.off), a Claude Code
// session still starts and works: every request it makes through its base
// URL is answered, by OpenAI or by janus itself, and nothing reaches
// Anthropic (#376).
func TestAClaudeCodeSessionRunsWithNoAnthropicAccount(t *testing.T) {
	anthropicHit := false
	anthropic := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { anthropicHit = true }))
	defer anthropic.Close()

	srv := openaitest.New(t,
		openaitest.Answer(openaitest.Text{Text: "Done."}),
		openaitest.Answer(openaitest.Text{Text: `{"decision":"allow"}`}),
		openaitest.Answer(openaitest.Text{Text: "Summary of the work."}),
	)
	cfg := config.Default()
	cfg.Upstream = config.Backend{BaseURL: anthropic.URL, Off: true}
	cfg.OpenAI = config.OpenAI{BaseURL: srv.URL, Key: "env:OPENAI_API_KEY"}
	cfg.Features.Route = true
	cfg.Routing = []config.Rule{{Name: "everything", Fallback: []string{"openai"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	key, err := openai.LoadKey(cfg.OpenAI, func(string) (string, bool) { return "janus-test-key", true })
	if err != nil {
		t.Fatal(err)
	}
	oa, err := openai.NewBackend(cfg.OpenAI, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	unusedLocal, _ := url.Parse("http://127.0.0.1:1")
	router := newRouter(cfg, backend.Nowhere{}, backend.NewLocal(unusedLocal, nil), oa)

	led := ledger.Open(filepath.Join(t.TempDir(), "ledger"))
	var console bytes.Buffer
	reported := make(chan proxy.Exchange, 1)
	janus := httptest.NewServer(proxy.New(proxy.Options{
		Upstream: backend.Nowhere{}, Plan: router.Plan, Itself: router.Itself,
		Done: func(ex proxy.Exchange) { router.Report(ex); reported <- ex },
	}))
	defer janus.Close()

	type step struct {
		name, method, path, body string
		backend, says            string
	}
	messages := func(system, user string, maxTokens int) string {
		b, _ := json.Marshal(map[string]any{
			"model": "claude-opus-5-5", "max_tokens": maxTokens, "stream": false,
			"system":   []map[string]any{{"type": "text", "text": system}},
			"messages": []map[string]any{{"role": "user", "content": user}},
		})
		return string(b)
	}
	session := []step{
		{"connectivity", http.MethodGet, "/", "", "janus", ""},
		{"connectivity", http.MethodHead, "/api/hello", "", "janus", ""},
		{"quota check", http.MethodPost, "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`, "janus", `"msg_janus_quota"`},
		{"main turn", http.MethodPost, "/v1/messages", messages("You are Claude Code.", "Fix the test.", 32000), "openai", "Done."},
		{"auto mode's safety check", http.MethodPost, "/v1/messages", messages("You are a security monitor for autonomous AI coding agents.", "May it run go test?", 4096), "openai", "allow"},
		{"compaction", http.MethodPost, "/v1/messages", messages("You are Claude Code.", "Your task is to create a detailed summary of the conversation so far.", 32000), "openai", "Summary of the work."},
		{"token count", http.MethodPost, "/v1/messages/count_tokens", `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"` + strings.Repeat("word ", 300) + `"}]}`, "janus", `"input_tokens":`},
	}
	for _, s := range session {
		req, _ := http.NewRequest(s.method, janus.URL+s.path, strings.NewReader(s.body))
		req.Header.Set("Authorization", "Bearer "+placeholderToken)
		req.Header.Set("X-Claude-Code-Session-Id", "s1")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var ex proxy.Exchange
		select {
		case ex = <-reported:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no exchange reported", s.name)
		}
		if resp.StatusCode != http.StatusOK || ex.Backend != s.backend || !bytes.Contains(got, []byte(s.says)) {
			t.Errorf("%s: status %d from %s, body %s", s.name, resp.StatusCode, ex.Backend, got)
		}
		rec := record(ex, led, nil, nil, testConsole(&console))
		if s.name == "token count" && !strings.Contains(rec.Reason, "estimated by janus") {
			t.Errorf("the token count is not labelled an estimate: reason %q", rec.Reason)
		}
	}
	if !strings.Contains(console.String(), "quota      janus") {
		t.Errorf("the console names a model for the quota check janus answered:\n%s", console.String())
	}
	if strings.Contains(console.String(), "refused") {
		t.Errorf("the console calls something janus answered refused:\n%s", console.String())
	}
	if anthropicHit {
		t.Error("a request reached Anthropic")
	}
	for _, r := range srv.Requests() {
		for name, values := range r.Header {
			if strings.Contains(strings.Join(values, " "), placeholderToken) {
				t.Errorf("OpenAI was sent the placeholder token in %s", name)
			}
		}
	}
	if len(srv.Requests()) != 3 {
		t.Errorf("OpenAI was asked %d times, want 3: the turn, the safety check, and the compaction", len(srv.Requests()))
	}
}

func TestARequestNoRuleSendsAnywhereIsRefusedWithNoUpstream(t *testing.T) {
	cfg := config.Default()
	cfg.Upstream.Off = true
	cfg.OpenAI = config.OpenAI{Key: "env:OPENAI_API_KEY"}
	cfg.Features.Route = true
	cfg.Routing = []config.Rule{{Class: "subagent", Fallback: []string{"openai"}}}
	unusedLocal, _ := url.Parse("http://127.0.0.1:1")
	router := newRouter(cfg, backend.Nowhere{}, backend.NewLocal(unusedLocal, nil), nil)
	janus := httptest.NewServer(proxy.New(proxy.Options{Upstream: backend.Nowhere{}, Plan: router.Plan, Itself: router.Itself, Done: func(proxy.Exchange) {}}))
	defer janus.Close()

	req, _ := http.NewRequest(http.MethodPost, janus.URL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Claude-Code-Session-Id", "s1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(got), "janus: no routing rule sends this request anywhere, and upstream is off") {
		t.Errorf("status %d, body %s", resp.StatusCode, got)
	}

	resp, err = http.Get(janus.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(got), "janus: upstream is off") {
		t.Errorf("an endpoint janus does not answer: status %d, body %s", resp.StatusCode, got)
	}
}

func TestUpstreamOffNeedsEveryRuleToBeAListWithoutUpstream(t *testing.T) {
	base := func() config.Config {
		cfg := config.Default()
		cfg.Upstream.Off = true
		cfg.OpenAI = config.OpenAI{Key: "env:OPENAI_API_KEY"}
		cfg.Features.Route = true
		return cfg
	}
	for _, c := range []struct {
		name  string
		rules []config.Rule
		want  string
	}{
		{"no rules", nil, "upstream.off needs features.route on and a routing rule"},
		{"a route", []config.Rule{{Route: "upstream"}}, "routing[0].route is upstream, but upstream.off is set"},
		{"a list naming upstream", []config.Rule{{Fallback: []string{"openai", "upstream"}}}, "routing[0].fallback names upstream, but upstream.off is set"},
		{"a list", []config.Rule{{Fallback: []string{"openai"}}}, ""},
	} {
		cfg := base()
		cfg.Routing = c.rules
		err := cfg.Validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}
