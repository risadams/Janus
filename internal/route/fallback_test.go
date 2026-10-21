package route

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/openai"
	"github.com/risadams/janus/internal/openai/openaitest"
	"github.com/risadams/janus/internal/proxy"
)

// A rule's fallback list names providers in order, and a request goes to
// the first that can take it, moving down the list when one cannot answer
// (#427).

func fakeOpenAI(t *testing.T, replies ...openaitest.Reply) (*openai.Backend, *openaitest.Server) {
	t.Helper()
	srv := openaitest.New(t, replies...)
	cfg := config.OpenAI{BaseURL: srv.URL, Key: "env:OPENAI_API_KEY"}
	key, err := openai.LoadKey(cfg, func(string) (string, bool) { return "janus-test-key", true })
	if err != nil {
		t.Fatal(err)
	}
	b, err := openai.NewBackend(cfg, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b, srv
}

// along sends body through janus with one rule, whose list is names.
func along(t *testing.T, oa *openai.Backend, local *backend.Local, upstream http.HandlerFunc, body string, names ...string) result {
	t.Helper()
	up := httptest.NewServer(upstream)
	defer up.Close()
	u, _ := url.Parse(up.URL)
	model := qwen["janus-local"]
	list := make([]Provider, len(names))
	for i, n := range names {
		list[i] = Provider{Name: n}
		if n == "janus-local" {
			list[i].Local = &model
		}
	}
	r := &Router{Upstream: backend.NewUpstream(u, nil), Local: local, OpenAI: oa, Rules: []Rule{{Name: "all", Fallback: list}}}
	reported := make(chan proxy.Exchange, 1)
	janus := httptest.NewServer(proxy.New(proxy.Options{Upstream: r.Upstream, Plan: r.Plan, Done: func(ex proxy.Exchange) { reported <- ex }}))
	defer janus.Close()
	req, _ := http.NewRequest(http.MethodPost, janus.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("X-Api-Key", "sk-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	select {
	case ex := <-reported:
		return result{resp.StatusCode, string(got), ex}
	case <-time.After(5 * time.Second):
		t.Fatal("no exchange reported")
		return result{}
	}
}

// limitReached is Claude's answer when a usage limit is reached, with a
// reset hours away: the #150 rules leave Claude for it at once.
func limitReached(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "7200")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"limit reached"}}`)
}

func never(t *testing.T) http.HandlerFunc {
	return func(http.ResponseWriter, *http.Request) { t.Error("the request reached Claude") }
}

func TestAListIsFollowedPastEveryProviderThatCannotAnswer(t *testing.T) {
	oa, srv := fakeOpenAI(t, openaitest.Fail(http.StatusInternalServerError, "server_error", "server_error", "The server had an error."))
	local, sawLocal := lmStudio(t, nil)

	res := along(t, oa, local, limitReached, opusRequest, "upstream", "openai", "janus-local")

	if res.Status != http.StatusOK || !strings.Contains(res.Body, `"model":"claude-opus-5-5"`) {
		t.Fatalf("status %d, body %s", res.Status, res.Body)
	}
	if len(srv.Requests()) != 1 || len(sawLocal) != 1 {
		t.Errorf("OpenAI was asked %d times and LM Studio %d; want once each", len(srv.Requests()), len(sawLocal))
	}
	ex := res.Exchange
	if ex.Backend != "local" || !strings.Contains(ex.Reason, "upstream answered HTTP 429") || !strings.Contains(ex.Reason, "openai answered HTTP 500") {
		t.Errorf("backend %s, reason %q", ex.Backend, ex.Reason)
	}
	if strings.Count(ex.Reason, "rule all") != 1 {
		t.Errorf("the reason repeats the rule: %q", ex.Reason)
	}
}

func TestOpenAIOnlyIsAListOfOne(t *testing.T) {
	oa, srv := fakeOpenAI(t, openaitest.Answer(openaitest.Text{Text: "From OpenAI."}))

	res := along(t, oa, nil, never(t), opusRequest, "openai")

	if res.Status != http.StatusOK || !strings.Contains(res.Body, "From OpenAI.") || res.Exchange.Backend != "openai" || res.Exchange.Route != RouteOpenAI {
		t.Errorf("status %d, backend %s, body %s", res.Status, res.Exchange.Backend, res.Body)
	}
	if got := srv.Requests()[0].Header.Get("X-Api-Key"); got != "" {
		t.Errorf("Claude Code's credential reached OpenAI: %q", got)
	}
}

// A web search runs on Anthropic's servers, which OpenAI cannot stand in
// for.
const webSearchRequest = `{"model":"claude-opus-5-5","tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"hi"}]}`

func TestAProviderThatCannotTakeTheRequestIsPassedOver(t *testing.T) {
	oa, srv := fakeOpenAI(t)
	answered := false
	res := along(t, oa, nil, func(w http.ResponseWriter, r *http.Request) {
		answered = true
		upstreamSays(200, `{"type":"message","content":[]}`)(w, r)
	}, webSearchRequest, "openai", "upstream")

	if !answered || len(srv.Requests()) != 0 {
		t.Fatalf("Claude answered: %v; OpenAI was sent %d requests", answered, len(srv.Requests()))
	}
	if !strings.Contains(res.Exchange.Reason, "the request cannot be translated for OpenAI") || !strings.HasSuffix(res.Exchange.Reason, "; sent upstream") {
		t.Errorf("reason %q", res.Exchange.Reason)
	}
}

func TestARequestNoProviderOnTheListCanTakeIsRefused(t *testing.T) {
	oa, srv := fakeOpenAI(t)
	res := along(t, oa, nil, never(t), webSearchRequest, "openai")
	if res.Status != http.StatusBadRequest || !strings.Contains(res.Body, "janus: no provider on the list can take this request") || len(srv.Requests()) != 0 {
		t.Errorf("status %d, body %s", res.Status, res.Body)
	}
}

// Claude stays first for a failure it will likely get past: a brief one is
// Claude Code's to retry (#150).
func TestABriefFailureOfClaudeDoesNotMoveTheConversation(t *testing.T) {
	oa, srv := fakeOpenAI(t)
	res := along(t, oa, nil, upstreamSays(529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`), opusRequest, "upstream", "openai")
	if res.Status != 529 || len(srv.Requests()) != 0 {
		t.Errorf("status %d; OpenAI was sent %d requests", res.Status, len(srv.Requests()))
	}
}
