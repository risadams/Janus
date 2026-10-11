package route

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/proxy"
)

const modelList = `{"data":[
	{"id":"qwen/qwen3.8-27b","state":"loaded","loaded_context_length":61696,"max_context_length":262144},
	{"id":"gemma-4-e4b","state":"not-loaded","max_context_length":131072}]}`

const localStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_local","type":"message","role":"assistant","content":[],"model":"qwen/qwen3.8-27b","usage":{"input_tokens":363,"output_tokens":0,"cache_read_input_tokens":256}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Two lines."}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":30}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// received is a request as a fake backend saw it.
type received struct {
	Path   string
	Header http.Header
	Body   []byte
}

// lmStudio is a fake LM Studio: a model list, and a Messages endpoint that
// answers with a stream naming its own model.
func lmStudio(t *testing.T, messages http.HandlerFunc) (*backend.Local, chan received) {
	t.Helper()
	got := make(chan received, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v0/models" {
			io.WriteString(w, modelList)
			return
		}
		body, _ := io.ReadAll(r.Body)
		got <- received{r.URL.Path, r.Header.Clone(), body}
		if messages != nil {
			messages(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, localStream)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return backend.NewLocal(u, nil), got
}

func router(local *backend.Local, aliases map[string]LocalModel) *Router {
	up, _ := url.Parse("https://api.anthropic.com")
	return &Router{Upstream: backend.NewUpstream(up, nil), Local: local, Aliases: aliases}
}

func parse(t *testing.T, body string) *anthropic.Request {
	t.Helper()
	req, err := anthropic.ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

var qwen = map[string]LocalModel{"janus-local": {ID: "qwen/qwen3.8-27b", Timeout: time.Minute}}

func TestPlanLeavesOtherModelsUpstream(t *testing.T) {
	local, _ := lmStudio(t, nil)
	req := parse(t, `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`)

	plan, refusal := router(local, qwen).Plan(context.Background(), http.Header{}, req)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	if plan.Backend.Name() != "upstream" || plan.Route != "upstream" || plan.Reason != "passthrough" {
		t.Errorf("plan = %+v", plan)
	}
	if plan.Body != nil || plan.ResponseModel != "" || len(plan.Stages) != 0 || req.Modified() {
		t.Errorf("an upstream request was altered: body=%v stages=%v modified=%v", plan.Body != nil, plan.Stages, req.Modified())
	}
}

func TestPlanSendsAnAliasLocal(t *testing.T) {
	local, _ := lmStudio(t, nil)
	raw, _ := os.ReadFile("../anthropic/testdata/request.json")
	req := parse(t, strings.Replace(string(raw), `"model":"claude-opus-5-5"`, `"model":"janus-local"`, 1))

	plan, refusal := router(local, qwen).Plan(context.Background(), http.Header{}, req)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	if plan.Backend.Name() != "local" || plan.Route != "local" || plan.Reason != "model alias janus-local" {
		t.Errorf("plan = %+v", plan)
	}
	if plan.BackendModel != "qwen/qwen3.8-27b" || plan.ResponseModel != "janus-local" || plan.Timeout != time.Minute {
		t.Errorf("models and timeout: %+v", plan)
	}
	if got := strings.Join(plan.Stages, ","); got != "set_model,lift_tool_additions,drop_deferred_tools" {
		t.Errorf("stages = %s", got)
	}
	sent := parse(t, string(plan.Body))
	if sent.Model() != "qwen/qwen3.8-27b" || strings.Contains(string(plan.Body), "tool_addition") {
		t.Errorf("the body to send was not normalised: model %q", sent.Model())
	}
}

func TestPlanRefusals(t *testing.T) {
	local, _ := lmStudio(t, nil)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL, _ := url.Parse(dead.URL)
	dead.Close()

	big := `{"model":"janus-local","messages":[{"role":"user","content":"` + strings.Repeat("word ", 40000) + `"}]}` // 200 kB, about 66,700 tokens by estimate
	small := `{"model":"janus-local","messages":[{"role":"user","content":"hi"}]}`

	cases := map[string]struct {
		router     *Router
		body       string
		wantStatus int
		wantType   string
		wantIn     string
	}{
		"model LM Studio does not have": {router(local, map[string]LocalModel{"janus-local": {ID: "no-such-model"}}), small,
			400, "invalid_request_error", `LM Studio does not have local model "no-such-model"`},
		"LM Studio unreachable": {router(backend.NewLocal(deadURL, nil), qwen), small,
			502, "api_error", "could not reach LM Studio"},
		"larger than the loaded context": {router(local, qwen), big,
			400, "invalid_request_error", "more than the 61696-token context"},
		"larger than a configured limit": {router(local, map[string]LocalModel{"janus-local": {ID: "qwen/qwen3.8-27b", ContextTokens: 10}}), small,
			400, "invalid_request_error", "more than the 10-token context"},
		"a tool_addition janus cannot read": {router(local, qwen), `{"model":"janus-local","messages":[{"role":"system","content":[{"type":"tool_addition"}]}]}`,
			400, "invalid_request_error", "cannot adapt this request"},
	}
	for name, c := range cases {
		_, refusal := c.router.Plan(context.Background(), http.Header{}, parse(t, c.body))
		if refusal == nil {
			t.Errorf("%s: not refused", name)
			continue
		}
		if refusal.Status != c.wantStatus || refusal.Type != c.wantType || !strings.Contains(refusal.Message, c.wantIn) {
			t.Errorf("%s: refusal = %d %s %q, want %d %s containing %q", name, refusal.Status, refusal.Type, refusal.Message, c.wantStatus, c.wantType, c.wantIn)
		}
		if !strings.HasPrefix(refusal.Message, "janus: ") || refusal.Route != "local" {
			t.Errorf("%s: refusal should name janus and the local route: %+v", name, refusal)
		}
	}
}

func TestPlanLimits(t *testing.T) {
	local, _ := lmStudio(t, nil)
	big := `{"model":"janus-local","messages":[{"role":"user","content":"` + strings.Repeat("word ", 40000) + `"}]}`

	// A configured limit overrides the loaded context in both directions.
	raised := router(local, map[string]LocalModel{"janus-local": {ID: "qwen/qwen3.8-27b", ContextTokens: 100000}})
	if _, refusal := raised.Plan(context.Background(), http.Header{}, parse(t, big)); refusal != nil {
		t.Errorf("refused under a raised limit: %s", refusal.Message)
	}
	// A model that is not loaded has no context to check against yet; the
	// request goes, and LM Studio loads the model.
	unloaded := router(local, map[string]LocalModel{"janus-local": {ID: "gemma-4-e4b"}})
	if _, refusal := unloaded.Plan(context.Background(), http.Header{}, parse(t, big)); refusal != nil {
		t.Errorf("refused for a model with no loaded context: %s", refusal.Message)
	}
}

func TestEstimateTokens(t *testing.T) {
	for size, want := range map[int]int{0: 0, 1: 1, 3: 1, 4: 2, 300000: 100000} {
		if got := EstimateTokens(make([]byte, size)); got != want {
			t.Errorf("EstimateTokens(%d bytes) = %d, want %d", size, got, want)
		}
	}
}

// through runs janus with routing on in front of a fake upstream and a fake
// LM Studio, sends one request, and returns everything each party saw.
type result struct {
	Status   int
	Body     string
	Exchange proxy.Exchange
}

func through(t *testing.T, local *backend.Local, aliases map[string]LocalModel, upstream http.HandlerFunc, body string) result {
	t.Helper()
	return throughRouter(t, local, aliases, nil, upstream, body)
}

func throughRules(t *testing.T, local *backend.Local, rules []Rule, upstream http.HandlerFunc, body string) result {
	t.Helper()
	return throughRouter(t, local, nil, rules, upstream, body)
}

func throughRouter(t *testing.T, local *backend.Local, aliases map[string]LocalModel, rules []Rule, upstream http.HandlerFunc, body string) result {
	t.Helper()
	up := httptest.NewServer(upstream)
	defer up.Close()
	u, _ := url.Parse(up.URL)
	r := &Router{Upstream: backend.NewUpstream(u, nil), Local: local, Aliases: aliases, Rules: rules}

	reported := make(chan proxy.Exchange, 1)
	janus := httptest.NewServer(proxy.New(proxy.Options{
		Upstream: r.Upstream, Plan: r.Plan, KeepBodies: true,
		Done: func(ex proxy.Exchange) { reported <- ex },
	}))
	defer janus.Close()

	req, _ := http.NewRequest("POST", janus.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("X-Api-Key", "sk-secret")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("X-Claude-Code-Session-Id", "s1")
	req.Header.Set("X-Claude-Code-Agent-Id", "a1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	select {
	case ex := <-reported:
		return result{resp.StatusCode, string(respBody), ex}
	case <-time.After(3 * time.Second):
		t.Fatal("no exchange reported")
		return result{}
	}
}

func TestAliasRequestIsAnsweredLocally(t *testing.T) {
	local, sawLocal := lmStudio(t, nil)
	upstreamHit := false
	raw, _ := os.ReadFile("../anthropic/testdata/request.json")
	body := strings.Replace(string(raw), `"model":"claude-opus-5-5"`, `"model":"janus-local"`, 1)

	res := through(t, local, qwen, func(http.ResponseWriter, *http.Request) { upstreamHit = true }, body)

	if upstreamHit {
		t.Fatal("a request for a local alias reached the upstream")
	}
	got := <-sawLocal

	// What LM Studio received: the local model, no tool_addition blocks,
	// and none of the user's Anthropic credentials.
	if got.Path != "/v1/messages" {
		t.Errorf("path = %s", got.Path)
	}
	sent := parse(t, string(got.Body))
	if sent.Model() != "qwen/qwen3.8-27b" || strings.Contains(string(got.Body), "tool_addition") {
		t.Errorf("LM Studio got model %q; tool_addition present: %v", sent.Model(), strings.Contains(string(got.Body), "tool_addition"))
	}
	for _, h := range []string{"Authorization", "X-Api-Key"} {
		if got.Header.Get(h) != "" {
			t.Errorf("LM Studio was sent the %s header", h)
		}
	}
	if got.Header.Get("Anthropic-Version") != "2023-06-01" {
		t.Error("a harmless header was dropped on the way to LM Studio")
	}

	// What the client received: the same stream, naming the model it asked
	// for.
	want := strings.Replace(localStream, `"model":"qwen/qwen3.8-27b"`, `"model":"janus-local"`, 1)
	if res.Status != 200 || res.Body != want {
		t.Errorf("status %d, body:\n%s\nwant:\n%s", res.Status, res.Body, want)
	}

	// What janus recorded: the route, the real model, and the usage.
	ex := res.Exchange
	if ex.Backend != "local" || ex.Route != "local" || ex.Reason != "model alias janus-local" || ex.BackendModel != "qwen/qwen3.8-27b" {
		t.Errorf("exchange decision = %+v", ex)
	}
	if ex.Summary == nil || ex.Summary.Model != "qwen/qwen3.8-27b" || ex.Summary.Usage.Output != 30 || !ex.Summary.Complete {
		t.Errorf("summary = %+v, want the local model's real name and usage", ex.Summary)
	}
	// The kept request is what the client sent, alias and all.
	if kept := parse(t, string(ex.RequestBody)); kept.Model() != "janus-local" {
		t.Errorf("kept request model = %q, want what the client sent", kept.Model())
	}
}

func TestOtherRequestsStillGoUpstreamUntouched(t *testing.T) {
	local, sawLocal := lmStudio(t, nil)
	var upstreamSaw received
	body := `{"model":"claude-opus-5-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	res := through(t, local, qwen, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstreamSaw = received{r.URL.Path, r.Header.Clone(), b}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}, body)

	select {
	case <-sawLocal:
		t.Fatal("an upstream request reached LM Studio")
	default:
	}
	if string(upstreamSaw.Body) != body || upstreamSaw.Header.Get("Authorization") != "Bearer secret-token" {
		t.Errorf("upstream saw body %q, auth %q", upstreamSaw.Body, upstreamSaw.Header.Get("Authorization"))
	}
	if res.Exchange.Backend != "upstream" || res.Exchange.Route != "upstream" || !strings.Contains(res.Body, `"model":"claude-opus-5-5"`) {
		t.Errorf("exchange = %+v, body = %s", res.Exchange, res.Body)
	}
}

func TestRefusalIsAnAnthropicShapedError(t *testing.T) {
	local, sawLocal := lmStudio(t, nil)
	res := through(t, local, map[string]LocalModel{"janus-local": {ID: "no-such-model"}},
		func(http.ResponseWriter, *http.Request) { t.Error("a refused request reached the upstream") },
		`{"model":"janus-local","messages":[{"role":"user","content":"hi"}]}`)

	var body struct {
		Type  string
		Error struct{ Type, Message string }
	}
	if err := json.Unmarshal([]byte(res.Body), &body); err != nil {
		t.Fatalf("error body is not JSON: %s", res.Body)
	}
	if res.Status != 400 || body.Type != "error" || body.Error.Type != "invalid_request_error" || !strings.Contains(body.Error.Message, "janus: ") {
		t.Errorf("status %d, body %+v", res.Status, body)
	}
	if ex := res.Exchange; ex.Backend != "none" || ex.Route != "local" || ex.Status != 400 || !strings.Contains(ex.Reason, "does not have") {
		t.Errorf("exchange = backend %q route %q status %d reason %q", ex.Backend, ex.Route, ex.Status, ex.Reason)
	}
	select {
	case <-sawLocal:
		t.Error("a refused request reached LM Studio")
	default:
	}
}

func TestLocalJSONResponseIsRenamedAndRelengthed(t *testing.T) {
	local, _ := lmStudio(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","content":[{"type":"text","text":"Ready."}],"model":"qwen/qwen3.8-27b","stop_reason":"end_turn","usage":{"input_tokens":17,"output_tokens":3}}`)
	})
	res := through(t, local, qwen, nil, `{"model":"janus-local","max_tokens":20,"messages":[{"role":"user","content":"hi"}]}`)

	// The body is a different length from what LM Studio sent; if the old
	// Content-Length had been kept the client would have failed to read it.
	want := `{"id":"msg_1","type":"message","content":[{"type":"text","text":"Ready."}],"model":"janus-local","stop_reason":"end_turn","usage":{"input_tokens":17,"output_tokens":3}}`
	if res.Status != 200 || res.Body != want {
		t.Errorf("status %d, body %s", res.Status, res.Body)
	}
}

func TestLocalTimeout(t *testing.T) {
	local, _ := lmStudio(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never answers
	})
	slow := map[string]LocalModel{"janus-local": {ID: "qwen/qwen3.8-27b", Timeout: 100 * time.Millisecond}}
	res := through(t, local, slow, nil, `{"model":"janus-local","messages":[{"role":"user","content":"hi"}]}`)

	if res.Status != http.StatusGatewayTimeout || !strings.Contains(res.Body, "janus: local did not answer within 100ms") {
		t.Errorf("status %d, body %s", res.Status, res.Body)
	}
	if res.Exchange.Cancelled {
		t.Error("a backend timeout was recorded as the client cancelling")
	}
}

func TestLocalUnreachableAtSendTime(t *testing.T) {
	// The model list answers, so the plan is made; then the Messages call
	// drops the connection.
	local, _ := lmStudio(t, func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) })
	res := through(t, local, qwen, nil, `{"model":"janus-local","messages":[{"role":"user","content":"hi"}]}`)

	if res.Status != http.StatusBadGateway || !strings.Contains(res.Body, "janus: could not reach local") {
		t.Errorf("status %d, body %s", res.Status, res.Body)
	}
}

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

func subagentHeader() http.Header {
	h := http.Header{}
	h.Set("X-Claude-Code-Session-Id", "s1")
	h.Set("X-Claude-Code-Agent-Id", "a1")
	return h
}

func mainHeader() http.Header {
	h := http.Header{}
	h.Set("X-Claude-Code-Session-Id", "s1")
	return h
}

const opusRequest = `{"model":"claude-opus-5-5","tools":[{"name":"Read"}],"messages":[{"role":"user","content":"hi"}]}`

func TestRuleMatching(t *testing.T) {
	facts := Facts{Class: Subagent, Model: "claude-opus-5-5", Tokens: 5000, Tools: true}
	cases := map[string]struct {
		rule Rule
		want string // "" means it matches
	}{
		"no conditions":       {Rule{}, ""},
		"class":               {Rule{Class: Subagent}, ""},
		"other class":         {Rule{Class: Main}, "class is subagent, not main"},
		"model":               {Rule{Model: "claude-opus-5-5"}, ""},
		"other model":         {Rule{Model: "claude-haiku-5-5"}, "model is claude-opus-5-5, not claude-haiku-5-5"},
		"within size":         {Rule{MaxTokens: 5000}, ""},
		"over size":           {Rule{MaxTokens: 4999}, "about 5000 tokens, over the rule's 4999"},
		"wants tools":         {Rule{Tools: yes()}, ""},
		"wants no tools":      {Rule{Tools: no()}, "the request offers tools"},
		"every condition met": {Rule{Class: Subagent, Model: "claude-opus-5-5", MaxTokens: 9000, Tools: yes()}, ""},
		"one condition unmet": {Rule{Class: Subagent, Model: "claude-opus-5-5", MaxTokens: 100, Tools: yes()}, "about 5000 tokens, over the rule's 100"},
	}
	for name, c := range cases {
		if got := c.rule.mismatch(facts); got != c.want {
			t.Errorf("%s: mismatch = %q, want %q", name, got, c.want)
		}
	}
	if got := (Rule{Tools: yes()}).mismatch(Facts{}); got != "the request offers no tools" {
		t.Errorf("mismatch = %q", got)
	}
}

func TestFactsOf(t *testing.T) {
	facts := FactsOf(subagentHeader(), parse(t, opusRequest))
	if facts.Class != Subagent || facts.Model != "claude-opus-5-5" || !facts.Tools || facts.Tokens != EstimateTokens([]byte(opusRequest)) {
		t.Errorf("facts = %+v", facts)
	}
	if FactsOf(mainHeader(), parse(t, `{"model":"m","messages":[]}`)).Tools {
		t.Error("a request with no tools reports tools")
	}
}

func TestFirstMatchingRuleDecides(t *testing.T) {
	local, _ := lmStudio(t, nil)
	r := router(local, nil)
	r.Rules = []Rule{
		{Name: "haiku", Model: "claude-haiku-5-5", Route: RouteLocal, Local: qwen["janus-local"]},
		{Name: "subagents", Class: Subagent, Route: RouteLocal, Local: qwen["janus-local"]},
		{Name: "catch-all", Route: RouteUpstream},
	}

	plan, refusal, trace := r.Explain(context.Background(), subagentHeader(), parse(t, opusRequest))
	if refusal != nil || plan.Backend.Name() != "local" || plan.Reason != "rule subagents" {
		t.Fatalf("plan = %+v, refusal = %+v", plan, refusal)
	}
	// The client asked for a real model and is told that model answered;
	// the ledger records which one really did.
	if plan.ResponseModel != "claude-opus-5-5" || plan.BackendModel != "qwen/qwen3.8-27b" {
		t.Errorf("response model %q, backend model %q", plan.ResponseModel, plan.BackendModel)
	}
	wantTrace := "rule haiku: no match (model is claude-opus-5-5, not claude-haiku-5-5)\nrule subagents: local, to qwen/qwen3.8-27b"
	if got := strings.Join(trace, "\n"); got != wantTrace {
		t.Errorf("trace:\n%s\nwant:\n%s", got, wantTrace)
	}

	// The same request from the main loop falls through to the catch-all.
	plan, _, trace = r.Explain(context.Background(), mainHeader(), parse(t, opusRequest))
	if plan.Backend.Name() != "upstream" || plan.Reason != "rule catch-all" || plan.Body != nil {
		t.Errorf("plan = %+v", plan)
	}
	if len(trace) != 3 || trace[2] != "rule catch-all: upstream" {
		t.Errorf("trace = %q", trace)
	}
}

func TestNoRuleMatchesGoesUpstream(t *testing.T) {
	local, _ := lmStudio(t, nil)
	r := router(local, nil)
	r.Rules = []Rule{{Class: Subagent, Route: RouteLocal, Local: qwen["janus-local"]}}

	plan, refusal, trace := r.Explain(context.Background(), mainHeader(), parse(t, opusRequest))
	if refusal != nil || plan.Backend.Name() != "upstream" || plan.Reason != "passthrough" {
		t.Errorf("plan = %+v", plan)
	}
	if got := strings.Join(trace, " | "); got != "rule 1: no match (class is main, not subagent) | no rule matched: upstream" {
		t.Errorf("trace = %s", got)
	}
}

// A rule sends a real model's request local. When local cannot take it,
// the request must go to that real model untouched, not fail.
func TestRuleFallsBackUpstreamWhenLocalCannotTakeIt(t *testing.T) {
	local, _ := lmStudio(t, nil)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL, _ := url.Parse(dead.URL)
	dead.Close()
	big := `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"` + strings.Repeat("word ", 40000) + `"}]}`

	cases := map[string]struct {
		local  *backend.Local
		model  LocalModel
		body   string
		wantIn string
	}{
		"LM Studio down":    {backend.NewLocal(deadURL, nil), qwen["janus-local"], opusRequest, "could not reach LM Studio"},
		"model missing":     {local, LocalModel{ID: "no-such-model"}, opusRequest, `LM Studio does not have local model "no-such-model"`},
		"request too large": {local, qwen["janus-local"], big, "more than the 61696-token context"},
		"cannot be adapted": {local, qwen["janus-local"], `{"model":"claude-opus-5-5","messages":[{"role":"system","content":[{"type":"tool_addition"}]}]}`, "cannot adapt this request"},
	}
	for name, c := range cases {
		r := router(c.local, nil)
		r.Rules = []Rule{{Name: "subagents", Class: Subagent, Route: RouteLocal, Local: c.model}}
		req := parse(t, c.body)

		plan, refusal, _ := r.Explain(context.Background(), subagentHeader(), req)
		if refusal != nil {
			t.Errorf("%s: refused (%s); a real model's request must fall back upstream", name, refusal.Message)
			continue
		}
		if plan.Backend.Name() != "upstream" || plan.Body != nil || plan.ResponseModel != "" || req.Modified() {
			t.Errorf("%s: plan = %+v, request modified = %v; want upstream with the request untouched", name, plan, req.Modified())
		}
		if !strings.HasPrefix(plan.Reason, "rule subagents: ") || !strings.Contains(plan.Reason, c.wantIn) || !strings.HasSuffix(plan.Reason, "; sent upstream") {
			t.Errorf("%s: reason = %q", name, plan.Reason)
		}
	}
}

func TestEscalateRouteGoesUpstreamForNow(t *testing.T) {
	local, _ := lmStudio(t, nil)
	r := router(local, nil)
	r.Rules = []Rule{{Name: "careful", Route: RouteEscalate, Local: qwen["janus-local"]}}

	plan, _, _ := r.Explain(context.Background(), mainHeader(), parse(t, opusRequest))
	if plan.Backend.Name() != "upstream" || plan.Reason != "rule careful: local-then-escalate is not available yet; sent upstream" {
		t.Errorf("plan = %+v", plan)
	}
}

func TestAliasBeatsRules(t *testing.T) {
	local, _ := lmStudio(t, nil)
	r := router(local, qwen)
	r.Rules = []Rule{{Name: "everything upstream", Route: RouteUpstream}}

	plan, refusal, trace := r.Explain(context.Background(), mainHeader(), parse(t, `{"model":"janus-local","messages":[{"role":"user","content":"hi"}]}`))
	if refusal != nil || plan.Backend.Name() != "local" || plan.Reason != "model alias janus-local" {
		t.Errorf("plan = %+v", plan)
	}
	if len(trace) != 1 || trace[0] != "model alias janus-local: local, to qwen/qwen3.8-27b" {
		t.Errorf("trace = %q", trace)
	}
}

func TestSubagentRuleEndToEnd(t *testing.T) {
	local, sawLocal := lmStudio(t, nil)
	rules := []Rule{{Name: "subagents", Class: Subagent, Route: RouteLocal, Local: qwen["janus-local"]}}

	// through() sends Claude Code's session and agent headers, so the
	// request is a subagent turn.
	res := throughRules(t, local, rules, func(http.ResponseWriter, *http.Request) {
		t.Error("a request a rule sent local reached the upstream")
	}, opusRequest)

	got := <-sawLocal
	if parse(t, string(got.Body)).Model() != "qwen/qwen3.8-27b" || got.Header.Get("Authorization") != "" {
		t.Errorf("LM Studio got model %q, auth %q", parse(t, string(got.Body)).Model(), got.Header.Get("Authorization"))
	}
	if !strings.Contains(res.Body, `"model":"claude-opus-5-5"`) || strings.Contains(res.Body, "qwen") {
		t.Errorf("the client should see the model it asked for:\n%s", res.Body)
	}
	if ex := res.Exchange; ex.Backend != "local" || ex.Reason != "rule subagents" || ex.Summary.Model != "qwen/qwen3.8-27b" {
		t.Errorf("exchange = backend %q reason %q model %q", ex.Backend, ex.Reason, ex.Summary.Model)
	}
}

func TestSubagentRuleFallsBackEndToEnd(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL, _ := url.Parse(dead.URL)
	dead.Close()
	rules := []Rule{{Name: "subagents", Class: Subagent, Route: RouteLocal, Local: qwen["janus-local"]}}
	var upstreamBody string

	res := throughRules(t, backend.NewLocal(deadURL, nil), rules, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstreamBody = string(b)
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			t.Error("the fallback request lost its credentials")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}, opusRequest)

	if upstreamBody != opusRequest {
		t.Errorf("upstream got %q, want the request exactly as the client sent it", upstreamBody)
	}
	if res.Status != 200 || res.Exchange.Backend != "upstream" || !strings.Contains(res.Exchange.Reason, "could not reach LM Studio") {
		t.Errorf("status %d, exchange backend %q reason %q", res.Status, res.Exchange.Backend, res.Exchange.Reason)
	}
}

func TestShadowRuleSendsUpstreamUntouchedAndAsksForAShadow(t *testing.T) {
	local, _ := lmStudio(t, nil)
	r := router(local, nil)
	r.Rules = []Rule{{Name: "watch", Class: Subagent, Route: RouteShadow, Local: LocalModel{ID: "qwen/qwen3.8-27b", ContextTokens: 5000, Timeout: time.Minute}}}
	req := parse(t, opusRequest)

	plan, refusal, trace := r.Explain(context.Background(), subagentHeader(), req)
	if refusal != nil || plan.Backend.Name() != "upstream" || plan.Body != nil || plan.ResponseModel != "" || req.Modified() {
		t.Fatalf("plan = %+v; a shadowed request must go upstream exactly as it arrived", plan)
	}
	want := proxy.Shadow{Model: "qwen/qwen3.8-27b", ContextTokens: 5000, Timeout: time.Minute, Reason: "rule watch"}
	if plan.Shadow == nil || *plan.Shadow != want {
		t.Errorf("shadow = %+v, want %+v", plan.Shadow, want)
	}
	if plan.Route != "upstream" || plan.Reason != "rule watch" || trace[0] != "rule watch: upstream, and shadowed on qwen/qwen3.8-27b" {
		t.Errorf("route %q reason %q trace %q", plan.Route, plan.Reason, trace)
	}

	// A request the rule does not match asks for no shadow.
	if plan, _, _ := r.Explain(context.Background(), mainHeader(), parse(t, opusRequest)); plan.Shadow != nil {
		t.Error("an unmatched request was given a shadow")
	}
}

const mixedHistory = `{"model":"claude-opus-5-5","messages":[` +
	`{"role":"user","content":"count the lines"},` +
	`{"role":"assistant","content":[{"type":"thinking","thinking":"local scratch work","signature":""},{"type":"tool_use","id":"587714761","name":"Bash","input":{}}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"587714761","content":"2"}]}]}`

// Every way a request can end up upstream repairs a history a local model
// had a turn in.
func TestUpstreamPlansRepairMixedHistory(t *testing.T) {
	local, _ := lmStudio(t, nil)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL, _ := url.Parse(dead.URL)
	dead.Close()

	cases := map[string]*Router{
		"no rule matches":              router(local, nil),
		"an upstream rule":             {Upstream: router(local, nil).Upstream, Local: local, Rules: []Rule{{Route: RouteUpstream}}},
		"a shadow rule":                {Upstream: router(local, nil).Upstream, Local: local, Rules: []Rule{{Route: RouteShadow, Local: qwen["janus-local"]}}},
		"an escalate rule":             {Upstream: router(local, nil).Upstream, Local: local, Rules: []Rule{{Route: RouteEscalate, Local: qwen["janus-local"]}}},
		"a local rule that falls back": {Upstream: router(local, nil).Upstream, Local: backend.NewLocal(deadURL, nil), Rules: []Rule{{Route: RouteLocal, Local: qwen["janus-local"]}}},
	}
	for name, r := range cases {
		plan, refusal, trace := r.Explain(context.Background(), subagentHeader(), parse(t, mixedHistory))
		if refusal != nil || plan.Backend.Name() != "upstream" {
			t.Errorf("%s: plan = %+v, refusal = %+v", name, plan, refusal)
			continue
		}
		if plan.Body == nil || strings.Contains(string(plan.Body), "local scratch work") || !strings.Contains(string(plan.Body), `"id":"587714761"`) {
			t.Errorf("%s: the body to send still has the unsigned thinking, or lost the tool call:\n%s", name, plan.Body)
		}
		if strings.Join(plan.Stages, ",") != "drop_unsigned_thinking" {
			t.Errorf("%s: stages = %v", name, plan.Stages)
		}
		if !strings.Contains(strings.Join(trace, "\n"), "removed the unsigned thinking") {
			t.Errorf("%s: the trace does not mention the repair: %q", name, trace)
		}
	}
}

// The repair is for the upstream only. A request going to the local model
// keeps its history as it is: the local model wrote that thinking and has
// no signatures to check.
func TestLocalPlansKeepLocalThinking(t *testing.T) {
	local, _ := lmStudio(t, nil)
	r := router(local, nil)
	r.Rules = []Rule{{Route: RouteLocal, Local: qwen["janus-local"]}}

	plan, _, _ := r.Explain(context.Background(), subagentHeader(), parse(t, mixedHistory))
	if plan.Backend.Name() != "local" || !strings.Contains(string(plan.Body), "local scratch work") {
		t.Errorf("plan = %+v\n%s", plan, plan.Body)
	}
	for _, stage := range plan.Stages {
		if stage == "drop_unsigned_thinking" {
			t.Error("a local request was repaired for the upstream")
		}
	}
}

func TestMixedConversationReachesTheUpstreamRepaired(t *testing.T) {
	local, _ := lmStudio(t, nil)
	var upstreamBody string
	res := throughRules(t, local, nil, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstreamBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}, mixedHistory)

	if strings.Contains(upstreamBody, `"type":"thinking"`) || !strings.Contains(upstreamBody, `"tool_use_id":"587714761"`) {
		t.Errorf("the upstream received:\n%s", upstreamBody)
	}
	if got := strings.Join(res.Exchange.Stages, ","); got != "drop_unsigned_thinking" {
		t.Errorf("exchange stages = %q", got)
	}
	// The kept request is still what the client sent.
	if !strings.Contains(string(res.Exchange.RequestBody), "local scratch work") {
		t.Error("the kept request should be the client's original")
	}
}
