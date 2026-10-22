package route

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/anthropic"
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
	oa, srv := fakeOpenAI(t, openaitest.RateLimited("insufficient_quota", ""))
	local, sawLocal := lmStudio(t, nil)

	res := along(t, oa, local, limitReached, opusRequest, "upstream", "openai", "janus-local")

	if res.Status != http.StatusOK || !strings.Contains(res.Body, `"model":"claude-opus-5-5"`) {
		t.Fatalf("status %d, body %s", res.Status, res.Body)
	}
	if len(srv.Requests()) != 1 || len(sawLocal) != 1 {
		t.Errorf("OpenAI was asked %d times and LM Studio %d; want once each", len(srv.Requests()), len(sawLocal))
	}
	ex := res.Exchange
	if ex.Backend != "local" || !strings.Contains(ex.Reason, "upstream answered HTTP 429") || !strings.Contains(ex.Reason, "openai answered HTTP 429") {
		t.Errorf("backend %s, reason %q", ex.Backend, ex.Reason)
	}
	if strings.Count(ex.Reason, "rule all") != 1 {
		t.Errorf("the reason repeats the rule: %q", ex.Reason)
	}
	if len(ex.Tried) != 2 || ex.Tried[0].Backend != "upstream" || !ex.Tried[0].Sent || ex.Tried[1].Backend != "openai" || !ex.Tried[1].Sent ||
		!strings.Contains(ex.Tried[1].Why, "openai answered HTTP 429") {
		t.Errorf("tried = %+v, want Claude and then OpenAI, each sent the request and failing it", ex.Tried)
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
	if tried := res.Exchange.Tried; len(tried) != 1 || tried[0].Backend != "openai" || tried[0].Sent || !strings.Contains(tried[0].Why, "cannot be translated") {
		t.Errorf("tried = %+v, want OpenAI passed over", tried)
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

// claudeAnswers is Claude answering every request, keeping what it was
// sent.
func claudeAnswers(sent *[]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*sent = append(*sent, string(b))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"From Claude."}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}
}

// What retrying will not fix moves the request on at once, with the
// request as it arrived; what needs the owner takes OpenAI out until a
// restart, so the next request does not ask again.
func TestOpenAIIsLeftAtOnceForWhatRetryingWillNotFix(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply openaitest.Reply
		out   bool
	}{
		{"too long", openaitest.Fail(http.StatusBadRequest, "invalid_request_error", "context_length_exceeded", "Your input exceeds the context window of this model."), false},
		{"a request janus translated wrongly", openaitest.Fail(http.StatusBadRequest, "invalid_request_error", "", "Invalid value for 'input[3].content'."), false},
		{"key refused", openaitest.Fail(http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "Incorrect API key provided."), true},
		{"quota used up", openaitest.RateLimited("insufficient_quota", ""), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			oa, srv := fakeOpenAI(t, c.reply, c.reply)
			var sent []string
			up := httptest.NewServer(claudeAnswers(&sent))
			defer up.Close()
			r := listRouter(t, up, oa, "openai", "upstream")

			for turn := 1; turn <= 2; turn++ {
				res := r.send(t, opusRequest)
				if res.Status != http.StatusOK || !strings.Contains(res.Body, "From Claude.") {
					t.Fatalf("turn %d: status %d, body %s", turn, res.Status, res.Body)
				}
				if turn == 2 && c.out && !strings.Contains(res.Exchange.Reason, "OpenAI is out until janus restarts") {
					t.Errorf("turn 2 reason %q", res.Exchange.Reason)
				}
			}
			if sent[0] != opusRequest {
				t.Errorf("Claude was sent %s, not the request as it arrived", sent[0])
			}
			want := 2
			if c.out {
				want = 1
			}
			if got := len(srv.Requests()); got != want {
				t.Errorf("OpenAI was asked %d times, want %d", got, want)
			}
		})
	}
}

// A failure that passes by itself is Claude Code's to retry, as Claude's
// is, until it has gone on long enough to be an outage (#150).
func TestABriefOpenAIFailureIsClaudeCodesToRetry(t *testing.T) {
	serverError := openaitest.Fail(http.StatusInternalServerError, "server_error", "server_error", "The server had an error.")
	oa, srv := fakeOpenAI(t, openaitest.RateLimited("rate_limit_exceeded", "7"), serverError, serverError)
	var sent []string
	up := httptest.NewServer(claudeAnswers(&sent))
	defer up.Close()
	r := listRouter(t, up, oa, "openai", "upstream")

	if res := r.send(t, opusRequest); res.Status != http.StatusTooManyRequests || res.Exchange.Backend != "openai" {
		t.Errorf("a per-minute limit: status %d from %s", res.Status, res.Exchange.Backend)
	}
	if res := r.send(t, opusRequest); res.Status != http.StatusInternalServerError {
		t.Errorf("a second failure: status %d", res.Status)
	}
	if res := r.send(t, opusRequest); res.Status != http.StatusOK || !strings.Contains(res.Body, "From Claude.") {
		t.Errorf("the third failure in two minutes: status %d, body %s", res.Status, res.Body)
	}
	if len(srv.Requests()) != 3 || len(sent) != 1 {
		t.Errorf("OpenAI was asked %d times and Claude %d", len(srv.Requests()), len(sent))
	}
}

func TestAPerMinuteLimitWithALongWaitMovesOnAtOnce(t *testing.T) {
	oa, _ := fakeOpenAI(t, openaitest.RateLimited("rate_limit_exceeded", "120"))
	var sent []string
	up := httptest.NewServer(claudeAnswers(&sent))
	defer up.Close()
	if res := listRouter(t, up, oa, "openai", "upstream").send(t, opusRequest); res.Status != http.StatusOK || len(sent) != 1 {
		t.Errorf("status %d; Claude was asked %d times", res.Status, len(sent))
	}
}

func TestAProviderTheRequestMayNotReachIsPassedOver(t *testing.T) {
	oa, srv := fakeOpenAI(t)
	var sent []string
	up := httptest.NewServer(claudeAnswers(&sent))
	defer up.Close()
	r := listRouter(t, up, oa, "openai", "upstream")
	r.Barred = func(provider string, _ *anthropic.Request) string {
		if provider == "openai" {
			return "the employer has not approved OpenAI"
		}
		return ""
	}
	res := r.send(t, opusRequest)
	if res.Status != http.StatusOK || len(srv.Requests()) != 0 || !strings.Contains(res.Exchange.Reason, "the employer has not approved OpenAI") {
		t.Errorf("status %d, OpenAI asked %d times, reason %q", res.Status, len(srv.Requests()), res.Exchange.Reason)
	}

	r.Barred = func(string, *anthropic.Request) string { return "nowhere is approved" }
	res = r.send(t, opusRequest)
	if res.Status != http.StatusForbidden || len(sent) != 1 || len(srv.Requests()) != 0 {
		t.Errorf("with every provider barred: status %d; Claude asked %d times, OpenAI %d", res.Status, len(sent), len(srv.Requests()))
	}
}

// listJanus is janus with one rule, whose list is names, kept for several
// requests so the router's state carries from one to the next.
type listJanus struct {
	*Router
	url      string
	reported chan proxy.Exchange
}

func listRouter(t *testing.T, up *httptest.Server, oa *openai.Backend, names ...string) *listJanus {
	t.Helper()
	u, _ := url.Parse(up.URL)
	list := make([]Provider, len(names))
	for i, n := range names {
		list[i] = Provider{Name: n}
	}
	j := &listJanus{Router: &Router{Upstream: backend.NewUpstream(u, nil), OpenAI: oa, Rules: []Rule{{Name: "all", Fallback: list}}}, reported: make(chan proxy.Exchange, 1)}
	srv := httptest.NewServer(proxy.New(proxy.Options{Upstream: j.Upstream, Plan: j.Plan, Done: func(ex proxy.Exchange) { j.Report(ex); j.reported <- ex }}))
	t.Cleanup(srv.Close)
	j.url = srv.URL
	return j
}

func (j *listJanus) send(t *testing.T, body string) result {
	t.Helper()
	resp, err := http.Post(j.url+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	select {
	case ex := <-j.reported:
		return result{resp.StatusCode, string(got), ex}
	case <-time.After(5 * time.Second):
		t.Fatal("no exchange reported")
		return result{}
	}
}

// Three sessions on one janus, each with its own provider: one following
// the rules, one asking for OpenAI first, one for the local model only
// (#383). OpenAI first still falls down the rule's list.
func TestEachSessionCanAskForItsOwnProvider(t *testing.T) {
	oa, srv := fakeOpenAI(t, openaitest.Answer(openaitest.Text{Text: "From OpenAI."}), openaitest.RateLimited("insufficient_quota", ""))
	local, sawLocal := lmStudio(t, nil)
	var sent []string
	up := httptest.NewServer(claudeAnswers(&sent))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	model := qwen["janus-local"]
	r := &Router{Upstream: backend.NewUpstream(u, nil), Local: local, OpenAI: oa, Aliases: qwen,
		Rules: []Rule{{Name: "main", Route: RouteUpstream, Fallback: []Provider{{Name: RouteUpstream}, {Name: "janus-local", Local: &model}}}}}
	reported := make(chan proxy.Exchange, 1)
	janus := httptest.NewServer(proxy.New(proxy.Options{Upstream: r.Upstream, Plan: r.Plan, Done: func(ex proxy.Exchange) { r.Report(ex); reported <- ex }}))
	defer janus.Close()
	send := func(session, route string) proxy.Exchange {
		req, _ := http.NewRequest(http.MethodPost, janus.URL+"/v1/messages", strings.NewReader(opusRequest))
		req.Header.Set("X-Claude-Code-Session-Id", session)
		if route != "" {
			req.Header.Set(backend.RouteHeader, route)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return <-reported
	}

	if ex := send("rules", ""); ex.Backend != "upstream" {
		t.Errorf("the session following the rules went to %s", ex.Backend)
	}
	if ex := send("codex", RouteOpenAI); ex.Backend != "openai" || !strings.HasPrefix(ex.Reason, "override openai, then rule main") {
		t.Errorf("the OpenAI session went to %s: %q", ex.Backend, ex.Reason)
	}
	if ex := send("local", RouteLocal); ex.Backend != "local" {
		t.Errorf("the local session went to %s", ex.Backend)
	}
	// OpenAI out of quota: the OpenAI session goes on down the rule's list.
	if ex := send("codex", RouteOpenAI); ex.Backend != "upstream" || len(ex.Tried) != 1 || ex.Tried[0].Backend != "openai" {
		t.Errorf("with OpenAI out, the OpenAI session went to %s, tried %+v", ex.Backend, ex.Tried)
	}
	if len(sent) != 2 || len(srv.Requests()) != 2 || len(sawLocal) != 1 {
		t.Errorf("Claude asked %d times, OpenAI %d, LM Studio %d", len(sent), len(srv.Requests()), len(sawLocal))
	}
	for _, r := range srv.Requests() {
		if r.Header.Get(backend.RouteHeader) != "" {
			t.Error("the route header reached OpenAI")
		}
	}
}
