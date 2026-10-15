package route

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/guard"
	"github.com/risadams/janus/internal/proxy"
	"github.com/risadams/janus/internal/transform"
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
		if r.URL.Path == "/api/v1/models/load" {
			answerLoad(w, r)
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

// answerLoad answers a load the way LM Studio does, with the context it
// was asked for.
func answerLoad(w http.ResponseWriter, r *http.Request) {
	var asked struct {
		Model         string `json:"model"`
		ContextLength int    `json:"context_length"`
	}
	json.NewDecoder(r.Body).Decode(&asked)
	fmt.Fprintf(w, `{"type":"llm","instance_id":%q,"load_time_seconds":0.1,"status":"loaded","load_config":{"context_length":%d}}`, asked.Model, asked.ContextLength)
}

// unreachable is the address of a backend that refuses every connection.
// Nothing listens on port 1, and it is never given to a test server. A port
// freed by closing a server is not safe: the next server started can be
// given it, and a request meant for nobody reaches that server instead,
// or janus itself (#211).
func unreachable() *url.URL {
	u, _ := url.Parse("http://127.0.0.1:1")
	return u
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
	if got := strings.Join(plan.Stages, ","); got != "set_model,lift_tool_additions,fold_system_messages,drop_deferred_tools" {
		t.Errorf("stages = %s", got)
	}
	sent := parse(t, string(plan.Body))
	if sent.Model() != "qwen/qwen3.8-27b" || strings.Contains(string(plan.Body), "tool_addition") {
		t.Errorf("the body to send was not normalised: model %q", sent.Model())
	}
}

func TestPlanRefusals(t *testing.T) {
	local, _ := lmStudio(t, nil)
	deadURL := unreachable()

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
	// A model that is not loaded is loaded by Janus, at a context it
	// chose. A small request goes; a large one is not worth a load that
	// would then fail.
	unloaded := router(local, map[string]LocalModel{"janus-local": {ID: "gemma-4-e4b"}})
	small := `{"model":"janus-local","messages":[{"role":"user","content":"hi"}]}`
	if _, refusal := unloaded.Plan(context.Background(), http.Header{}, parse(t, small)); refusal != nil {
		t.Errorf("a small request was refused for a model that is not loaded: %s", refusal.Message)
	}
	_, refusal := unloaded.Plan(context.Background(), http.Header{}, parse(t, big))
	if refusal == nil || !strings.Contains(refusal.Message, `the 65536-token context Janus loads local model "gemma-4-e4b" with`) || !strings.Contains(refusal.Message, "contextTokens") {
		t.Errorf("a large request for a model that is not loaded: %+v", refusal)
	}
	// contextTokens sets the context it is loaded with.
	told := router(local, map[string]LocalModel{"janus-local": {ID: "gemma-4-e4b", ContextTokens: 100000}})
	if _, refusal := told.Plan(context.Background(), http.Header{}, parse(t, big)); refusal != nil {
		t.Errorf("refused with contextTokens set: %s", refusal.Message)
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
	deadURL := unreachable()
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

func TestEscalateRulePlansATrialWithAFallback(t *testing.T) {
	local, _ := lmStudio(t, nil)
	r := router(local, nil)
	r.Rules = []Rule{{Name: "careful", Route: RouteEscalate, Local: qwen["janus-local"]}}
	req := parse(t, opusRequest)

	plan, refusal, trace := r.Explain(context.Background(), mainHeader(), req)
	if refusal != nil || plan.Backend.Name() != "local" || plan.Reason != "rule careful" || plan.ResponseModel != "claude-opus-5-5" {
		t.Fatalf("plan = %+v, refusal = %+v", plan, refusal)
	}
	if plan.Accept == nil || plan.Fallback == nil {
		t.Fatalf("an escalate plan needs both a check and a fallback: accept %v, fallback %v", plan.Accept != nil, plan.Fallback != nil)
	}
	// The fallback is the request as it arrived, to the model it asked for.
	if e := fallbackOf(t, plan); e.Backend.Name() != "upstream" || e.Body != nil || e.ResponseModel != "" || e.Fallback != nil || e.Accept != nil || req.Modified() {
		t.Errorf("fallback = %+v", e)
	}
	if !strings.Contains(trace[0], "rule careful: local first, on qwen/qwen3.8-27b") {
		t.Errorf("trace = %q", trace)
	}

	// The check is the validators, run against the tools the request offers.
	good := proxy.Fetched{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Summary: anthropic.Summary{Complete: true},
		Body: []byte(`{"type":"message","stop_reason":"tool_use","content":[{"type":"tool_use","id":"1","name":"Read","input":{}}]}`)}
	if ok, _, why := plan.Accept(good); !ok {
		t.Errorf("a good answer was rejected: %s", why)
	}
	bad := good
	bad.Body = []byte(`{"type":"message","stop_reason":"tool_use","content":[{"type":"tool_use","id":"1","name":"Launch","input":{}}]}`)
	if ok, _, why := plan.Accept(bad); ok || !strings.Contains(why, `calls "Launch"`) {
		t.Errorf("a call to a tool that was not offered: ok=%v why=%q", ok, why)
	}
	empty := good
	empty.Body = []byte(`{"type":"message","stop_reason":"end_turn","content":[]}`)
	if ok, _, why := plan.Accept(empty); ok || !strings.Contains(why, "no text and no tool call") {
		t.Errorf("an empty answer: ok=%v why=%q", ok, why)
	}
}

func TestSkipChecksReachesTheValidators(t *testing.T) {
	local, _ := lmStudio(t, nil)
	r := router(local, nil)
	r.Rules = []Rule{{Route: RouteEscalate, Local: qwen["janus-local"], SkipChecks: []string{"refusal"}}}
	plan, _, _ := r.Explain(context.Background(), mainHeader(), parse(t, opusRequest))

	refusal := proxy.Fetched{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Summary: anthropic.Summary{Complete: true},
		Body: []byte(`{"type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"I cannot help with that."}]}`)}
	if ok, _, why := plan.Accept(refusal); !ok {
		t.Errorf("the refusal check was skipped but still failed: %s", why)
	}
}

func TestLocalRuleHasAFallbackAndAliasDoesNot(t *testing.T) {
	local, _ := lmStudio(t, nil)
	r := router(local, qwen)
	r.Rules = []Rule{{Name: "subagents", Class: Subagent, Route: RouteLocal, Local: qwen["janus-local"]}}

	plan, _, _ := r.Explain(context.Background(), subagentHeader(), parse(t, opusRequest))
	if plan.Fallback == nil || fallbackOf(t, plan).Backend.Name() != "upstream" || plan.Accept != nil {
		t.Errorf("a local rule should fall back upstream and not put the answer on trial: %+v", plan)
	}
	alias, _, _ := r.Explain(context.Background(), mainHeader(), parse(t, `{"model":"janus-local","messages":[{"role":"user","content":"hi"}]}`))
	if alias.Else != nil || alias.Fallback != nil || alias.Accept != nil {
		t.Errorf("an alias has no upstream model to fall back to: %+v", alias)
	}
}

// The fallback for a conversation a local model already had a turn in must
// be repaired, like any other request going upstream.
func TestFallbackPlanIsRepaired(t *testing.T) {
	local, _ := lmStudio(t, nil)
	for _, routeName := range []string{RouteLocal, RouteEscalate} {
		r := router(local, nil)
		r.Rules = []Rule{{Route: routeName, Local: qwen["janus-local"]}}
		plan, _, _ := r.Explain(context.Background(), subagentHeader(), parse(t, mixedHistory))

		if plan.Backend.Name() != "local" || !strings.Contains(string(plan.Body), "local scratch work") {
			t.Errorf("%s: the local attempt should keep the local thinking", routeName)
		}
		if e := fallbackOf(t, plan); e.Body == nil || strings.Contains(string(e.Body), "local scratch work") || strings.Join(e.Stages, ",") != "drop_unsigned_thinking" {
			t.Errorf("%s: the fallback was not repaired: %+v", routeName, e)
		}
	}
}

func TestBreakerOpensAfterRepeatedFaultsAndClosesOnSuccess(t *testing.T) {
	local, _ := lmStudio(t, nil)
	r := router(local, qwen)
	r.Rules = []Rule{{Name: "subagents", Class: Subagent, Route: RouteEscalate, Local: qwen["janus-local"]}}
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.Local)
	r.health.now = func() time.Time { return now }
	plan := func() proxy.Plan {
		p, _, _ := r.Explain(context.Background(), subagentHeader(), parse(t, opusRequest))
		return p
	}
	fault := proxy.Exchange{Backend: "upstream", Attempt: &proxy.Attempt{Backend: "local", Fault: true}}
	poor := proxy.Exchange{Backend: "upstream", Attempt: &proxy.Attempt{Backend: "local", Why: "empty"}}

	// A poor answer is the backend working. Any number of them leaves the
	// breaker closed.
	for i := 0; i < 10; i++ {
		r.Report(poor)
	}
	r.Report(fault)
	r.Report(fault)
	if plan().Backend.Name() != "local" {
		t.Fatal("the breaker opened before three faults in a row")
	}
	r.Report(fault)
	skipped := plan()
	if skipped.Backend.Name() != "upstream" || !strings.Contains(skipped.Reason, "failed 3 times in a row and is being skipped until 12:00:30") {
		t.Fatalf("after three faults: %+v", skipped)
	}
	// An alias is not skipped: there is nowhere else for it to go.
	if alias, _, _ := r.Explain(context.Background(), mainHeader(), parse(t, `{"model":"janus-local","messages":[{"role":"user","content":"hi"}]}`)); alias.Backend.Name() != "local" {
		t.Errorf("an alias request was skipped by the breaker: %+v", alias)
	}

	// After the cooldown one request is let through, and a success closes
	// the breaker.
	now = now.Add(31 * time.Second)
	if plan().Backend.Name() != "local" {
		t.Fatal("the breaker did not let a request through after the cooldown")
	}
	r.Report(fault) // the trial request failed too: straight back to skipping
	if plan().Backend.Name() != "upstream" {
		t.Fatal("a failure after the cooldown should reopen the breaker at once")
	}
	now = now.Add(31 * time.Second)
	r.Report(proxy.Exchange{Backend: "local", Status: 200, Attempt: &proxy.Attempt{Backend: "local", Accepted: true}})
	r.Report(fault)
	r.Report(fault)
	if plan().Backend.Name() != "local" {
		t.Error("a success should reset the count of faults")
	}
}

func TestReportCountsDirectLocalFailures(t *testing.T) {
	local, _ := lmStudio(t, nil)
	r := router(local, nil)
	r.Rules = []Rule{{Route: RouteLocal, Local: qwen["janus-local"]}}
	for _, ex := range []proxy.Exchange{
		{Backend: "local", Status: 502, BackendFault: true},
		{Backend: "local", Status: 500},
		{Backend: "local", Status: 504, BackendFault: true},
	} {
		r.Report(ex)
	}
	if p, _, _ := r.Explain(context.Background(), mainHeader(), parse(t, opusRequest)); p.Backend.Name() != "upstream" {
		t.Errorf("three failed local exchanges did not open the breaker: %+v", p)
	}
	// Upstream exchanges say nothing about the local backend.
	r2 := router(local, nil)
	r2.Rules = r.Rules
	for i := 0; i < 5; i++ {
		r2.Report(proxy.Exchange{Backend: "upstream", Status: 529})
	}
	if p, _, _ := r2.Explain(context.Background(), mainHeader(), parse(t, opusRequest)); p.Backend.Name() != "local" {
		t.Error("upstream failures opened the local breaker")
	}
}

// LM Studio being unreachable when the plan is made counts as a fault too.
func TestUnreachableAtPlanTimeFeedsTheBreaker(t *testing.T) {
	deadURL := unreachable()
	r := router(backend.NewLocal(deadURL, nil), nil)
	r.Rules = []Rule{{Name: "s", Route: RouteLocal, Local: qwen["janus-local"]}}

	var reasons []string
	for i := 0; i < 4; i++ {
		p, _, _ := r.Explain(context.Background(), mainHeader(), parse(t, opusRequest))
		reasons = append(reasons, p.Reason)
	}
	if !strings.Contains(reasons[2], "could not reach LM Studio") || !strings.Contains(reasons[3], "failed 3 times in a row") {
		t.Errorf("reasons:\n%s", strings.Join(reasons, "\n"))
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
	deadURL := unreachable()
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
	deadURL := unreachable()

	cases := map[string]*Router{
		"no rule matches":              router(local, nil),
		"an upstream rule":             {Upstream: router(local, nil).Upstream, Local: local, Rules: []Rule{{Route: RouteUpstream}}},
		"a shadow rule":                {Upstream: router(local, nil).Upstream, Local: local, Rules: []Rule{{Route: RouteShadow, Local: qwen["janus-local"]}}},
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

// escalate runs one subagent request through janus with a local-first
// rule, against the given LM Studio behaviour, and reports whether the
// upstream was asked.
func escalate(t *testing.T, routeName string, lms http.HandlerFunc, body string) (res result, sawLocal chan received, upstreamBody *string) {
	t.Helper()
	local, sawLocal := lmStudio(t, lms)
	upstreamBody = new(string)
	rules := []Rule{{Name: "subagents", Class: Subagent, Route: routeName, Local: LocalModel{ID: "qwen/qwen3.8-27b", Timeout: 300 * time.Millisecond}}}
	res = throughRules(t, local, rules, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*upstreamBody = string(b)
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			t.Error("the upstream request lost its credentials")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","model":"claude-opus-5-5","content":[{"type":"text","text":"Claude's answer."}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4}}`)
	}, body)
	return res, sawLocal, upstreamBody
}

func jsonAnswer(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}
}

func TestEscalateAcceptsAGoodLocalAnswer(t *testing.T) {
	res, sawLocal, upstreamBody := escalate(t, RouteEscalate, nil, opusRequest) // the default fake answers with a text stream

	if *upstreamBody != "" {
		t.Fatal("the upstream was asked although the local answer was good")
	}
	if got := <-sawLocal; got.Header.Get("Authorization") != "" || parse(t, string(got.Body)).Model() != "qwen/qwen3.8-27b" {
		t.Errorf("LM Studio got auth %q, model %q", got.Header.Get("Authorization"), parse(t, string(got.Body)).Model())
	}
	// The client gets the local answer whole, under the name it asked for.
	want := strings.Replace(localStream, `"model":"qwen/qwen3.8-27b"`, `"model":"claude-opus-5-5"`, 1)
	if res.Status != 200 || res.Body != want {
		t.Errorf("status %d, body:\n%s", res.Status, res.Body)
	}
	ex := res.Exchange
	if ex.Backend != "local" || ex.Route != "local" || ex.Reason != "rule subagents: local answer accepted" {
		t.Errorf("exchange = backend %q route %q reason %q", ex.Backend, ex.Route, ex.Reason)
	}
	if ex.Attempt == nil || !ex.Attempt.Accepted || ex.Attempt.Fault || ex.Attempt.Model != "qwen/qwen3.8-27b" {
		t.Errorf("attempt = %+v", ex.Attempt)
	}
	if ex.Summary == nil || ex.Summary.Model != "qwen/qwen3.8-27b" || ex.Summary.Usage.Output != 30 || string(ex.ResponseBody) != localStream {
		t.Errorf("the exchange should record the local answer as it really was: %+v", ex.Summary)
	}
}

func TestEscalateRejectsPoorAnswersAndAsksClaude(t *testing.T) {
	cases := map[string]struct {
		local  string
		whyHas string
		check  string
	}{
		"empty, though it claims success": {`{"type":"message","model":"q","content":[],"stop_reason":"end_turn","usage":{"input_tokens":65,"output_tokens":14}}`, "no text and no tool call", "empty"},
		"a tool that was not offered":     {`{"type":"message","model":"q","content":[{"type":"tool_use","id":"1","name":"Launch","input":{}}],"stop_reason":"tool_use"}`, `calls "Launch"`, "tools"},
		"ran out of tokens":               {`{"type":"message","model":"q","content":[{"type":"text","text":"The"}],"stop_reason":"max_tokens"}`, "ran out of tokens", "truncated"},
		"a refusal":                       {`{"type":"message","model":"q","content":[{"type":"text","text":"I cannot access files."}],"stop_reason":"end_turn"}`, "refusal", "refusal"},
		"not a message at all":            {`{"something":"else"}`, "could not be read", "malformed"},
	}
	for name, c := range cases {
		res, _, upstreamBody := escalate(t, RouteEscalate, jsonAnswer(c.local), opusRequest)

		// One clean response: Claude's, with no trace of the local attempt.
		if res.Status != 200 || !strings.Contains(res.Body, "Claude's answer.") || strings.Contains(res.Body, `"model":"q"`) {
			t.Errorf("%s: status %d, body %s", name, res.Status, res.Body)
		}
		if *upstreamBody != opusRequest {
			t.Errorf("%s: the upstream got %q, want the request exactly as the client sent it", name, *upstreamBody)
		}
		ex := res.Exchange
		if ex.Backend != "upstream" || ex.Route != "upstream" || !strings.HasPrefix(ex.Reason, "rule subagents: ") || !strings.HasSuffix(ex.Reason, "; sent upstream") || !strings.Contains(ex.Reason, c.whyHas) {
			t.Errorf("%s: exchange = backend %q route %q reason %q", name, ex.Backend, ex.Route, ex.Reason)
		}
		a := ex.Attempt
		if a == nil || a.Accepted || a.Fault || !strings.Contains(a.Why, c.whyHas) || string(a.ResponseBody) != c.local {
			t.Errorf("%s: attempt = %+v; a poor answer is not a fault, and should be kept for inspection", name, a)
		}
		if a != nil && (a.Cause != proxy.CauseRejected || a.Check != c.check) {
			t.Errorf("%s: cause %q, check %q; want rejected by %q", name, a.Cause, a.Check, c.check)
		}
		if ex.Summary == nil || ex.Summary.Model != "claude-opus-5-5" {
			t.Errorf("%s: the exchange's own summary should be Claude's: %+v", name, ex.Summary)
		}
	}
}

func TestEscalateTreatsALocalOutageAsAFaultAndAsksClaude(t *testing.T) {
	cases := map[string]struct {
		local  http.HandlerFunc
		whyHas string
		cause  string
	}{
		"server error": {func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500); io.WriteString(w, "boom") }, "local answered HTTP 500", proxy.CauseServerError},
		"context overflow": {func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			io.WriteString(w, `{"error":"The number of tokens to keep from the initial prompt is greater than the context length"}`)
		}, "local answered HTTP 500", proxy.CauseTooLarge},
		"dropped":  {func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) }, "could not reach local", proxy.CauseUnreachable},
		"too slow": {func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, "local did not answer within 300ms", proxy.CauseSlow},
		"slow, mid-answer": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, localStream[:150])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}, "broke off", proxy.CauseSlow},
		"cut off mid-answer": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, localStream[:150])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}, "broke off", proxy.CauseBrokeOff},
		"an error event": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"model crashed"}}`)
		}, "returned an error", proxy.CauseServerError},
	}
	for name, c := range cases {
		res, _, upstreamBody := escalate(t, RouteEscalate, c.local, opusRequest)
		if res.Status != 200 || !strings.Contains(res.Body, "Claude's answer.") || *upstreamBody != opusRequest {
			t.Errorf("%s: status %d, body %s", name, res.Status, res.Body)
		}
		a := res.Exchange.Attempt
		if a == nil || a.Accepted || !strings.Contains(a.Why, c.whyHas) || a.Cause != c.cause {
			t.Errorf("%s: attempt = %+v, want cause %q and a reason mentioning %q", name, a, c.cause, c.whyHas)
		}
		if res.Exchange.Cancelled {
			t.Errorf("%s: recorded as the client cancelling", name)
		}
	}
}

// A plain local rule streams the answer as it comes, so it can only fall
// back while nothing has reached the client: when the local model cannot
// be reached, times out before answering, or answers with an error.
func TestLocalRuleFallsBackWhenTheSendFails(t *testing.T) {
	cases := map[string]struct {
		local     http.HandlerFunc
		whyHas    string
		wantFault bool
	}{
		"dropped":      {func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) }, "could not reach local", true},
		"too slow":     {func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, "local did not answer within 300ms", true},
		"server error": {func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }, "local answered HTTP 503", true},
		"rejects the request": {func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
			io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"no"}}`)
		}, "local answered HTTP 400", false},
	}
	for name, c := range cases {
		res, _, upstreamBody := escalate(t, RouteLocal, c.local, opusRequest)
		if res.Status != 200 || !strings.Contains(res.Body, "Claude's answer.") || *upstreamBody != opusRequest {
			t.Errorf("%s: status %d, body %s, upstream got %q", name, res.Status, res.Body, *upstreamBody)
		}
		ex := res.Exchange
		if ex.Backend != "upstream" || !strings.Contains(ex.Reason, c.whyHas) || !strings.HasSuffix(ex.Reason, "; sent upstream") {
			t.Errorf("%s: backend %q, reason %q", name, ex.Backend, ex.Reason)
		}
		if a := ex.Attempt; a == nil || a.Accepted || a.Fault != c.wantFault {
			t.Errorf("%s: attempt = %+v, want fault %v", name, a, c.wantFault)
		}
	}
}

func TestLocalRuleThatWorksStreamsStraightThrough(t *testing.T) {
	res, _, upstreamBody := escalate(t, RouteLocal, nil, opusRequest)
	if *upstreamBody != "" || res.Exchange.Backend != "local" || res.Exchange.Attempt != nil || res.Exchange.Reason != "rule subagents" {
		t.Errorf("exchange = %+v, upstream asked: %v", res.Exchange, *upstreamBody != "")
	}
}

// An alias has nowhere to fall back to. Its failures reach the client, and
// are marked so the breaker can count them.
func TestAliasFailureIsNotHiddenAndIsMarkedAsAFault(t *testing.T) {
	local, _ := lmStudio(t, func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) })
	res := through(t, local, qwen, func(http.ResponseWriter, *http.Request) {
		t.Error("an alias request reached the upstream")
	}, `{"model":"janus-local","messages":[{"role":"user","content":"hi"}]}`)

	if res.Status != http.StatusBadGateway || !res.Exchange.BackendFault || res.Exchange.Attempt != nil {
		t.Errorf("status %d, fault %v, attempt %+v", res.Status, res.Exchange.BackendFault, res.Exchange.Attempt)
	}
}

// Three outages in a row and the router stops trying the local model; the
// fourth request goes straight to Claude without waiting on it.
func TestRepeatedOutagesStopLocalAttempts(t *testing.T) {
	hits := 0
	local, _ := lmStudio(t, func(w http.ResponseWriter, r *http.Request) { hits++; w.WriteHeader(500) })
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","model":"claude-opus-5-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	r := &Router{Upstream: backend.NewUpstream(u, nil), Local: local,
		Rules: []Rule{{Name: "subagents", Class: Subagent, Route: RouteEscalate, Local: qwen["janus-local"]}}}
	reasons := make(chan string, 8)
	janus := httptest.NewServer(proxy.New(proxy.Options{Upstream: r.Upstream, Plan: r.Plan, Done: func(ex proxy.Exchange) {
		r.Report(ex)
		reasons <- ex.Reason
	}}))
	defer janus.Close()

	var got []string
	for i := 0; i < 4; i++ {
		req, _ := http.NewRequest("POST", janus.URL+"/v1/messages", strings.NewReader(opusRequest))
		req.Header.Set("X-Claude-Code-Session-Id", "s")
		req.Header.Set("X-Claude-Code-Agent-Id", "a")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("request %d: status %d; every request should still be answered", i+1, resp.StatusCode)
		}
		got = append(got, <-reasons)
	}
	if hits != 3 {
		t.Errorf("the local model was tried %d times, want 3 and then no more", hits)
	}
	if !strings.Contains(got[2], "local answered HTTP 500") || !strings.Contains(got[3], "failed 3 times in a row and is being skipped") {
		t.Errorf("reasons:\n%s", strings.Join(got, "\n"))
	}
}

// If the client gives up while the local model is still working, nothing
// is sent to Claude on its behalf.
func TestClientLeavingDuringATrialAsksNobodyElse(t *testing.T) {
	started := make(chan struct{})
	local, _ := lmStudio(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the upstream was asked for a client that had already left")
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	r := &Router{Upstream: backend.NewUpstream(u, nil), Local: local,
		Rules: []Rule{{Route: RouteEscalate, Local: LocalModel{ID: "qwen/qwen3.8-27b", Timeout: 5 * time.Second}}}}
	reported := make(chan proxy.Exchange, 1)
	janus := httptest.NewServer(proxy.New(proxy.Options{Upstream: r.Upstream, Plan: r.Plan, Done: func(ex proxy.Exchange) { reported <- ex }}))
	defer janus.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", janus.URL+"/v1/messages", strings.NewReader(opusRequest))
	go http.DefaultClient.Do(req)
	<-started
	cancel()

	select {
	case ex := <-reported:
		if !ex.Cancelled || ex.Status != 0 {
			t.Errorf("exchange = cancelled %v status %d", ex.Cancelled, ex.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the abandoned request was never reported")
	}
}

func TestTrialTimeoutIsTheShorterOfTheTwo(t *testing.T) {
	local, _ := lmStudio(t, nil)
	cases := []struct {
		routeName    string
		model, trial time.Duration
		want         time.Duration
	}{
		{RouteEscalate, 15 * time.Minute, 2 * time.Minute, 2 * time.Minute},
		{RouteEscalate, 30 * time.Second, 2 * time.Minute, 30 * time.Second},
		{RouteEscalate, 0, 2 * time.Minute, 2 * time.Minute},
		{RouteEscalate, 15 * time.Minute, 0, 15 * time.Minute},
		{RouteLocal, 15 * time.Minute, 2 * time.Minute, 15 * time.Minute}, // a streamed answer is not on trial
	}
	for _, c := range cases {
		r := router(local, nil)
		r.Rules = []Rule{{Route: c.routeName, Local: LocalModel{ID: "qwen/qwen3.8-27b", Timeout: c.model}, TrialTimeout: c.trial}}
		plan, _, _ := r.Explain(context.Background(), mainHeader(), parse(t, opusRequest))
		if plan.Timeout != c.want {
			t.Errorf("%s with model %s and trial %s: timeout %s, want %s", c.routeName, c.model, c.trial, plan.Timeout, c.want)
		}
	}
}

func withOverride(h http.Header, value string) http.Header {
	h.Set("X-Janus-Route", value)
	return h
}

func TestOverrideHeaderBeatsTheRules(t *testing.T) {
	local, _ := lmStudio(t, nil)
	r := router(local, qwen)
	r.Rules = []Rule{{Name: "subagents", Class: Subagent, Route: RouteLocal, Local: qwen["janus-local"]}}

	// Upstream, though a rule says local.
	plan, refusal, trace := r.Explain(context.Background(), withOverride(subagentHeader(), "upstream"), parse(t, opusRequest))
	if refusal != nil || plan.Backend.Name() != "upstream" || plan.Reason != "override upstream" || plan.Body != nil || plan.Else != nil {
		t.Errorf("override upstream: plan = %+v, refusal = %+v", plan, refusal)
	}
	if trace[0] != "override upstream: upstream" {
		t.Errorf("trace = %q", trace)
	}

	// Local, though no rule matches the main loop. With one model
	// configured it need not be named.
	for _, value := range []string{"local", "local=janus-local", " local "} {
		plan, refusal, _ := r.Explain(context.Background(), withOverride(mainHeader(), value), parse(t, opusRequest))
		if refusal != nil || plan.Backend.Name() != "local" || plan.BackendModel != "qwen/qwen3.8-27b" || plan.ResponseModel != "claude-opus-5-5" {
			t.Errorf("override %q: plan = %+v, refusal = %+v", value, plan, refusal)
		}
		// Forced local is a promise not to go upstream: no fallback.
		if plan.Else != nil || plan.Fallback != nil || plan.Accept != nil {
			t.Errorf("override %q: a forced local plan has a fallback", value)
		}
	}
}

func TestOverrideRefusals(t *testing.T) {
	local, _ := lmStudio(t, nil)
	deadURL := unreachable()
	two := map[string]LocalModel{"small": {ID: "gemma-4-e4b"}, "large": {ID: "qwen/qwen3.8-27b"}}

	cases := map[string]struct {
		router     *Router
		value      string
		wantStatus int
		wantIn     string
	}{
		"not a route":                          {router(local, qwen), "sideways", 400, `"sideways" is not upstream, local, or local=`},
		"a local model that is not configured": {router(local, qwen), "local=other", 400, `"other" is not an entry of local.models`},
		"several models and none named":        {router(local, two), "local", 400, "several local models are configured"},
		"no local model at all":                {router(local, nil), "local", 400, "no local model is configured"},
		// The point of forcing local: if it cannot be done, the request
		// is refused, not quietly sent to Claude.
		"forced local with LM Studio down": {router(backend.NewLocal(deadURL, nil), qwen), "local", 502, "could not reach LM Studio"},
	}
	for name, c := range cases {
		plan, refusal, _ := c.router.Explain(context.Background(), withOverride(mainHeader(), c.value), parse(t, opusRequest))
		if refusal == nil {
			t.Errorf("%s: not refused; plan = %+v", name, plan)
			continue
		}
		if refusal.Status != c.wantStatus || !strings.Contains(refusal.Message, c.wantIn) {
			t.Errorf("%s: refusal = %d %q, want %d containing %q", name, refusal.Status, refusal.Message, c.wantStatus, c.wantIn)
		}
	}
	// Naming one of several works.
	if plan, refusal, _ := router(local, two).Explain(context.Background(), withOverride(mainHeader(), "local=large"), parse(t, opusRequest)); refusal != nil || plan.BackendModel != "qwen/qwen3.8-27b" {
		t.Errorf("local=large: plan = %+v, refusal = %+v", plan, refusal)
	}
}

// claudeDown runs one main-loop request through janus with a rule that
// sends it upstream and names a local model for when Claude is down.
func claudeDown(t *testing.T, upstream http.HandlerFunc, lms http.HandlerFunc, body string) (status int, respHeader http.Header, respBody string, ex proxy.Exchange, modelLookups *int) {
	t.Helper()
	return claudeDownWith(t, modelList, upstream, lms, body)
}

// claudeDownWith is claudeDown with LM Studio reporting models as its
// model list.
func claudeDownWith(t *testing.T, models string, upstream http.HandlerFunc, lms http.HandlerFunc, body string) (status int, respHeader http.Header, respBody string, ex proxy.Exchange, modelLookups *int) {
	t.Helper()
	modelLookups = new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v0/models" {
			*modelLookups++
			io.WriteString(w, models)
			return
		}
		if r.URL.Path == "/api/v1/models/load" {
			answerLoad(w, r)
			return
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Janus-Route") != "" {
			t.Errorf("LM Studio was sent a credential or the override header: %v", r.Header)
		}
		io.Copy(io.Discard, r.Body)
		if lms != nil {
			lms(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, localStream)
	}))
	t.Cleanup(srv.Close)
	localURL, _ := url.Parse(srv.URL)

	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	model := LocalModel{ID: "qwen/qwen3.8-27b", Timeout: time.Second}
	r := &Router{Upstream: backend.NewUpstream(u, nil), Local: backend.NewLocal(localURL, nil),
		Rules: []Rule{{Name: "main", Class: Main, Route: RouteUpstream, IfDown: &model}}}
	// Claude has already failed twice in the last two minutes, so the
	// failure under test makes it an outage and the stand-in answers. When
	// that is decided, and when not, is TestAStandInWaitsForClaudeToBeDown.
	r.down.failures = []time.Time{time.Now(), time.Now()}
	reported := make(chan proxy.Exchange, 1)
	janus := httptest.NewServer(proxy.New(proxy.Options{Upstream: r.Upstream, Plan: r.Plan, Done: func(ex proxy.Exchange) { reported <- ex }}))
	t.Cleanup(janus.Close)

	req, _ := http.NewRequest("POST", janus.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("X-Claude-Code-Session-Id", "s1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b), <-reported, modelLookups
}

func upstreamSays(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_up")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

func TestLocalStandsInWhenClaudeIsDown(t *testing.T) {
	cases := map[string]struct {
		upstream http.HandlerFunc
		whyHas   string
	}{
		"overloaded":   {upstreamSays(529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`), "upstream answered HTTP 529"},
		"rate limited": {upstreamSays(429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`), "upstream answered HTTP 429"},
		"server error": {upstreamSays(500, `{"type":"error","error":{"type":"api_error","message":"oops"}}`), "upstream answered HTTP 500"},
		"dropped":      {func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) }, "could not reach upstream"},
	}
	for name, c := range cases {
		status, header, body, ex, _ := claudeDown(t, c.upstream, nil, opusRequest)

		want := strings.Replace(localStream, `"model":"qwen/qwen3.8-27b"`, `"model":"claude-opus-5-5"`, 1)
		if status != 200 || body != want {
			t.Errorf("%s: status %d, body:\n%s", name, status, body)
		}
		// The stand-in is marked, on the response and in the record.
		if got := header.Get("X-Janus-Fallback"); !strings.HasPrefix(got, "local answered because ") || !strings.Contains(got, c.whyHas) {
			t.Errorf("%s: X-Janus-Fallback = %q", name, got)
		}
		if ex.Backend != "local" || ex.Route != "local" || !strings.HasPrefix(ex.Reason, "rule main: ") || !strings.HasSuffix(ex.Reason, "; sent local") {
			t.Errorf("%s: exchange = backend %q route %q reason %q", name, ex.Backend, ex.Route, ex.Reason)
		}
		if a := ex.Attempt; a == nil || a.Backend != "upstream" || a.Accepted || !a.Fault {
			t.Errorf("%s: attempt = %+v", name, a)
		}
		if ex.Summary == nil || ex.Summary.Model != "qwen/qwen3.8-27b" {
			t.Errorf("%s: summary = %+v", name, ex.Summary)
		}
	}
}

// The rate limit given up on is kept on the attempt, headers and body, so
// the record says what Claude said about when it would answer again.
func TestTheFailedAnswerIsKeptWhenLocalStandsIn(t *testing.T) {
	errBody := `{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."}}`
	upstream := func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "3600")
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.WriteHeader(429)
		io.WriteString(w, errBody)
	}
	status, _, _, ex, _ := claudeDown(t, upstream, nil, opusRequest)
	a := ex.Attempt
	if status != 200 || a == nil || a.Backend != "upstream" {
		t.Fatalf("status %d, attempt %+v", status, a)
	}
	if a.ResponseHeader.Get("Retry-After") != "3600" || a.ResponseHeader.Get("Anthropic-Ratelimit-Unified-Status") != "rejected" {
		t.Errorf("attempt header = %v", a.ResponseHeader)
	}
	if string(a.ResponseBody) != errBody {
		t.Errorf("attempt body = %q", a.ResponseBody)
	}
}

// Errors that mean the request was wrong, not that Claude was down, reach
// the client untouched and the local model is never consulted.
func TestRequestErrorsFromClaudeAreNotStoodInFor(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 413} {
		errBody := `{"type":"error","error":{"type":"invalid_request_error","message":"no"}}`
		got, header, body, ex, lookups := claudeDown(t, upstreamSays(status, errBody), func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("status %d: the local model was asked", status)
		}, opusRequest)

		if got != status || body != errBody || header.Get("Request-Id") != "req_up" || header.Get("X-Janus-Fallback") != "" {
			t.Errorf("status %d: client got %d %q, headers %v", status, got, body, header)
		}
		if ex.Backend != "upstream" || ex.Attempt != nil || *lookups != 0 {
			t.Errorf("status %d: backend %q, attempt %+v, LM Studio lookups %d", status, ex.Backend, ex.Attempt, *lookups)
		}
	}
}

// The fallback is planned only when it is needed. A healthy Claude never
// causes LM Studio to be contacted at all.
func TestHealthyClaudeNeverTouchesLMStudio(t *testing.T) {
	status, _, body, ex, lookups := claudeDown(t, upstreamSays(200, `{"type":"message","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`), nil, opusRequest)
	if status != 200 || !strings.Contains(body, "claude-opus-5-5") || ex.Backend != "upstream" || ex.Attempt != nil {
		t.Errorf("status %d, backend %q, attempt %+v", status, ex.Backend, ex.Attempt)
	}
	if *lookups != 0 {
		t.Errorf("LM Studio was contacted %d times for a request Claude answered", *lookups)
	}
}

// When Claude is down and the local model cannot help either, the client
// gets Claude's error exactly as Claude sent it.
func TestClaudesErrorStandsWhenLocalCannotHelp(t *testing.T) {
	errBody := `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
	big := `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"` + strings.Repeat("word ", 40000) + `"}]}`

	// Too large for the local model.
	status, header, body, ex, _ := claudeDown(t, upstreamSays(529, errBody), func(w http.ResponseWriter, r *http.Request) {
		t.Error("the local model was sent a request too large for it")
	}, big)
	if status != 529 || body != errBody || header.Get("Request-Id") != "req_up" {
		t.Errorf("too large: client got %d %q", status, body)
	}
	if ex.Backend != "upstream" || ex.Attempt != nil || ex.Summary == nil || ex.Summary.Error == nil || ex.Summary.Error.Type != "overloaded_error" {
		t.Errorf("too large: exchange = backend %q attempt %+v summary %+v", ex.Backend, ex.Attempt, ex.Summary)
	}
	// The record says the stand-in was considered, and why it did not
	// answer.
	wantReason := "rule main: upstream answered HTTP 529; no local stand-in: this request is about "
	if !strings.HasPrefix(ex.Reason, wantReason) || !strings.Contains(ex.Reason, `even its first and latest turns are more than the 46272 tokens local model "qwen/qwen3.8-27b" has room for`) {
		t.Errorf("reason = %q", ex.Reason)
	}
	if !strings.HasPrefix(ex.NoFallback, "no local stand-in: this request is about ") {
		t.Errorf("no fallback = %q", ex.NoFallback)
	}
	if ex.LocalCause != proxy.CauseTooLarge {
		t.Errorf("local cause = %q, want %q", ex.LocalCause, proxy.CauseTooLarge)
	}

	// Claude unreachable, and LM Studio's local model busy: the reason
	// names both.
	_, _, _, ex, _ = claudeDownWith(t, `{"data":[]}`, func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) }, nil, opusRequest)
	if !strings.Contains(ex.Reason, "could not reach upstream") || !strings.Contains(ex.Reason, `no local stand-in: LM Studio does not have local model "qwen/qwen3.8-27b"`) {
		t.Errorf("unreachable: reason = %q", ex.Reason)
	}
	if ex.LocalCause != proxy.CauseModelMissing {
		t.Errorf("unreachable: local cause = %q, want %q", ex.LocalCause, proxy.CauseModelMissing)
	}
}

// A healthy answer from Claude says nothing about a stand-in.
func TestNoStandInNoteWhenClaudeAnswers(t *testing.T) {
	_, _, _, ex, _ := claudeDown(t, upstreamSays(200, `{"type":"message"}`), nil, opusRequest)
	if ex.NoFallback != "" || strings.Contains(ex.Reason, "stand-in") {
		t.Errorf("reason %q, no fallback %q", ex.Reason, ex.NoFallback)
	}
}

// A stand-in for a model LM Studio has not loaded is worth a load only if
// the request fits the context Janus would load it with. A request too
// large is not: Claude's error stands, and LM Studio is never asked.
func TestAColdStartIsOnlyForARequestThatFits(t *testing.T) {
	notLoaded := `{"data":[{"id":"qwen/qwen3.8-27b","state":"not-loaded","max_context_length":262144}]}`
	errBody := `{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."}}`
	big := `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"` + strings.Repeat("word ", 40000) + `"}]}`

	status, _, body, ex, _ := claudeDownWith(t, notLoaded, upstreamSays(429, errBody), func(w http.ResponseWriter, r *http.Request) {
		t.Error("LM Studio was asked to load a model for a request too large for it")
	}, big)
	if status != 429 || body != errBody || ex.Backend != "upstream" {
		t.Errorf("too large: client got %d %q from %q", status, body, ex.Backend)
	}

	// A small request is worth the load.
	status, header, _, ex, _ := claudeDownWith(t, notLoaded, upstreamSays(429, errBody), nil, opusRequest)
	if status != 200 || ex.Backend != "local" || header.Get("X-Janus-Fallback") == "" {
		t.Errorf("small: status %d, backend %q", status, ex.Backend)
	}

	// A model whose largest context is under the cold-start limit is held
	// to that instead.
	small := `{"data":[{"id":"qwen/qwen3.8-27b","state":"not-loaded","max_context_length":4096}]}`
	medium := `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"` + strings.Repeat("word ", 8000) + `"}]}`
	status, _, _, ex, _ = claudeDownWith(t, small, upstreamSays(429, errBody), func(w http.ResponseWriter, r *http.Request) {
		t.Error("LM Studio was sent a request larger than the model can take")
	}, medium)
	if status != 429 || ex.Backend != "upstream" {
		t.Errorf("over the model's maximum: status %d, backend %q", status, ex.Backend)
	}
}

// A safety check decides whether the agent may act. Rules without a class
// and stand-ins for the main loop never send it to a local model; only a
// rule that names the class does.
func TestSafetyChecksStayWithClaudeUnlessARuleNamesThem(t *testing.T) {
	check := `{"model":"claude-sonnet-5","max_tokens":64,"system":[{"type":"text","text":"You are a security monitor for autonomous AI coding agents."}],"messages":[{"role":"user","content":"Respond with <severity>N</severity> ONLY."}]}`
	header := http.Header{"X-Claude-Code-Session-Id": {"s1"}}
	model := LocalModel{ID: "qwen/qwen3.8-27b", Timeout: time.Second}
	local, _ := lmStudio(t, nil)

	everything := router(local, nil)
	everything.Rules = []Rule{{Name: "all", Route: RouteLocal, Local: model}}
	plan, refusal := everything.Plan(context.Background(), header, parse(t, check))
	if refusal != nil || plan.Route != RouteUpstream || !strings.Contains(plan.Reason, "passthrough") {
		t.Errorf("a class-less local rule: plan route %q reason %q, refusal %+v", plan.Route, plan.Reason, refusal)
	}

	mainDown := router(local, nil)
	mainDown.Rules = []Rule{{Name: "main", Class: Main, Route: RouteUpstream, IfDown: &model}, {Name: "any", Route: RouteUpstream, IfDown: &model}}
	plan, _ = mainDown.Plan(context.Background(), header, parse(t, check))
	if plan.Fallback != nil || plan.Else != nil {
		t.Errorf("a safety check got a local stand-in: reason %q", plan.Reason)
	}

	named := router(local, nil)
	named.Rules = []Rule{{Name: "checks", Class: Safety, Route: RouteLocal, Local: model}}
	plan, refusal = named.Plan(context.Background(), header, parse(t, check))
	if refusal != nil || plan.Route != RouteLocal {
		t.Errorf("a rule naming class safety: plan route %q, refusal %+v", plan.Route, refusal)
	}
}

func TestBothDownGivesTheClientAnError(t *testing.T) {
	// Claude drops the connection and LM Studio fails too: there is
	// nothing to relay, so janus says what happened.
	status, _, body, ex, _ := claudeDown(t, func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) },
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }, opusRequest)
	if status != http.StatusBadGateway && status != 500 {
		t.Errorf("status %d, body %s", status, body)
	}
	if ex.Attempt == nil || ex.Attempt.Backend != "upstream" {
		t.Errorf("attempt = %+v", ex.Attempt)
	}
}

func TestOverrideHeaderIsNotForwarded(t *testing.T) {
	local, _ := lmStudio(t, nil)
	var seen http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","model":"claude-opus-5-5","content":[]}`)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	r := &Router{Upstream: backend.NewUpstream(u, nil), Local: local, Aliases: qwen}
	janus := httptest.NewServer(proxy.New(proxy.Options{Upstream: r.Upstream, Plan: r.Plan, Done: func(proxy.Exchange) {}}))
	defer janus.Close()

	req, _ := http.NewRequest("POST", janus.URL+"/v1/messages", strings.NewReader(opusRequest))
	req.Header.Set("X-Janus-Route", "upstream")
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seen.Get("X-Janus-Route") != "" || seen.Get("Authorization") != "Bearer secret-token" {
		t.Errorf("upstream saw X-Janus-Route %q, Authorization %q", seen.Get("X-Janus-Route"), seen.Get("Authorization"))
	}
}

func TestUpstreamDown(t *testing.T) {
	for status, want := range map[int]bool{200: false, 400: false, 401: false, 404: false, 429: true, 500: true, 503: true, 529: true} {
		if got := UpstreamDown(status); got != want {
			t.Errorf("UpstreamDown(%d) = %v, want %v", status, got, want)
		}
	}
}

// Redaction is for what goes to Anthropic. A request routed to the local
// model keeps its secrets: it does not leave the user's own machines, and
// the local model needs the real values to do the work.
func TestRedactionAppliesToUpstreamOnly(t *testing.T) {
	key := strings.Join([]string{"AKIA", "Q7RZ2LP9XVB4NKD8"}, "")
	body := `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"aws_access_key_id = ` + key + `"}]}`
	run := func(header map[string]string) (localSaw, upstreamSaw string, ex proxy.Exchange) {
		local, sawLocal := lmStudio(t, nil)
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			upstreamSaw = string(b)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"type":"message","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
		}))
		defer up.Close()
		u, _ := url.Parse(up.URL)
		r := &Router{Upstream: backend.NewUpstream(u, nil), Local: local,
			Rules: []Rule{{Name: "subagents", Class: Subagent, Route: RouteLocal, Local: qwen["janus-local"]}}}
		reported := make(chan proxy.Exchange, 1)
		janus := httptest.NewServer(proxy.New(proxy.Options{Upstream: r.Upstream, Plan: r.Plan, Redactor: guard.NewRedactor(&guard.Detector{}),
			Done: func(ex proxy.Exchange) { reported <- ex }}))
		defer janus.Close()

		req, _ := http.NewRequest("POST", janus.URL+"/v1/messages", strings.NewReader(body))
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		select {
		case got := <-sawLocal:
			localSaw = string(got.Body)
		default:
		}
		return localSaw, upstreamSaw, <-reported
	}

	// A subagent turn, sent local by the rule: the local model gets the key.
	localSaw, upstreamSaw, ex := run(map[string]string{"X-Claude-Code-Session-Id": "s", "X-Claude-Code-Agent-Id": "a"})
	if !strings.Contains(localSaw, key) || upstreamSaw != "" || ex.Guard != nil {
		t.Errorf("local route: local saw key=%v, upstream asked=%v, guard=%+v", strings.Contains(localSaw, key), upstreamSaw != "", ex.Guard)
	}

	// A main-loop turn, sent upstream: the key is replaced.
	localSaw, upstreamSaw, ex = run(map[string]string{"X-Claude-Code-Session-Id": "s"})
	if localSaw != "" || strings.Contains(upstreamSaw, key) || !strings.Contains(upstreamSaw, "JANUS_REDACTED_") || ex.Guard == nil || ex.Guard.Replaced != 1 {
		t.Errorf("upstream route: upstream saw key=%v, guard=%+v", strings.Contains(upstreamSaw, key), ex.Guard)
	}
}

// longRequest has a text block long enough for hush to shorten.
var longRequest = `{"model":"claude-opus-5-5","tools":[{"name":"Read"}],"messages":[{"role":"user","content":"` + strings.Repeat("please ", 50) + `"}]}`

// hush rewrites every text block, so a test can see where transforms ran.
type hush struct{}

func (hush) Name() string { return "hush" }

func (hush) Rewrite(at transform.Position, b anthropic.Block) (anthropic.Block, bool) {
	if b.Type != "text" {
		return b, false
	}
	return b.With("text", json.RawMessage(`"(hushed)"`)), true
}

func TestTransformsRewriteWhatGoesUpstream(t *testing.T) {
	r := router(nil, nil)
	r.Transforms = transform.New(hush{})
	plan, _, trace := r.Explain(context.Background(), subagentHeader(), parse(t, longRequest))

	if plan.Backend.Name() != "upstream" || !strings.Contains(string(plan.Body), "(hushed)") || strings.Join(plan.Stages, ",") != "hush" {
		t.Errorf("plan = %s %v %s, want upstream, rewritten by hush", plan.Backend.Name(), plan.Stages, plan.Body)
	}
	if !strings.Contains(strings.Join(trace, "\n"), "rewritten to send fewer tokens, by hush") {
		t.Errorf("trace does not say the request was rewritten:\n%s", strings.Join(trace, "\n"))
	}
}

func TestTransformsLeaveTheLocalAttemptAlone(t *testing.T) {
	local, _ := lmStudio(t, nil)
	r := router(local, nil)
	r.Transforms = transform.New(hush{})
	r.Rules = []Rule{{Route: RouteEscalate, Local: qwen["janus-local"]}}
	plan, _, _ := r.Explain(context.Background(), subagentHeader(), parse(t, longRequest))

	if plan.Backend.Name() != "local" || strings.Contains(string(plan.Body), "(hushed)") {
		t.Errorf("the local attempt was rewritten: %s", plan.Body)
	}
	if e := fallbackOf(t, plan); !strings.Contains(string(e.Body), "(hushed)") || strings.Join(e.Stages, ",") != "hush" {
		t.Errorf("the upstream fallback was not rewritten: %+v", e)
	}
}

// fallbackOf works out a plan's fallback, as the proxy does when the first
// attempt fails.
func fallbackOf(t *testing.T, plan proxy.Plan) proxy.Plan {
	t.Helper()
	if plan.Fallback == nil {
		t.Fatal("the plan has no fallback")
	}
	fallback, ok := plan.Fallback(context.Background())
	if !ok {
		t.Fatal("the fallback could not be planned")
	}
	return fallback
}

// counted counts how often it is asked to rewrite anything.
type counted struct{ calls int }

func (c *counted) Name() string { return "counted" }
func (c *counted) Rewrite(at transform.Position, b anthropic.Block) (anthropic.Block, bool) {
	c.calls++
	return b, false
}

func TestTransformsRunOnlyWhenTheFallbackIsUsed(t *testing.T) {
	local, _ := lmStudio(t, nil)
	r := router(local, nil)
	c := &counted{}
	r.Transforms = transform.New(c)
	r.Rules = []Rule{{Route: RouteEscalate, Local: qwen["janus-local"]}}
	plan, _, _ := r.Explain(context.Background(), subagentHeader(), parse(t, longRequest))
	if c.calls != 0 {
		t.Fatalf("the transforms ran %d times while planning a local attempt", c.calls)
	}
	fallbackOf(t, plan)
	if c.calls == 0 {
		t.Error("the transforms did not run on the fallback once it was used")
	}
}

func TestRulesKeepWithinTheLocalModelsLimit(t *testing.T) {
	local, _ := lmStudio(t, nil)
	r := router(local, nil)
	one := qwen["janus-local"]
	one.MaxConcurrent = 1
	r.Rules = []Rule{{Name: "subagents", Class: Subagent, Route: RouteEscalate, Local: one}}
	plan := func() (proxy.Plan, []string) {
		p, _, trace := r.Explain(context.Background(), subagentHeader(), parse(t, opusRequest))
		return p, trace
	}

	first, _ := plan()
	if first.Backend.Name() != "local" || first.Release == nil {
		t.Fatalf("first request: backend %s, release set %v; want local, holding a place", first.Backend.Name(), first.Release != nil)
	}
	second, trace := plan()
	if second.Backend.Name() != "upstream" || !strings.Contains(second.Reason, "qwen/qwen3.8-27b already has 1 requests in flight, its limit; sent upstream") {
		t.Errorf("second request while the first is in flight: %s, %q", second.Backend.Name(), second.Reason)
	}
	if !strings.Contains(strings.Join(trace, "\n"), "its limit") {
		t.Errorf("trace does not say why: %q", trace)
	}

	first.Release()
	first.Release() // a second call must not free a second place
	third, _ := plan()
	if third.Backend.Name() != "local" {
		t.Fatalf("after the first finished: %s, want local", third.Backend.Name())
	}
	if fourth, _ := plan(); fourth.Backend.Name() != "upstream" {
		t.Errorf("releasing twice freed two places: fourth request went %s", fourth.Backend.Name())
	}
	third.Release()
}

func TestAPlanThatCannotGoLocalGivesItsPlaceBack(t *testing.T) {
	local, _ := lmStudio(t, nil)
	r := router(local, nil)
	tiny := qwen["janus-local"]
	tiny.MaxConcurrent, tiny.ContextTokens = 1, 10 // nothing fits
	r.Rules = []Rule{{Route: RouteEscalate, Local: tiny}}
	for range 3 {
		p, _, _ := r.Explain(context.Background(), subagentHeader(), parse(t, opusRequest))
		if !strings.Contains(p.Reason, "more than the 10-token context") {
			t.Fatalf("reason = %q, want the request too large, not the model full", p.Reason)
		}
	}
}

func TestNoLimitMeansNoLimit(t *testing.T) {
	local, _ := lmStudio(t, nil)
	r := router(local, nil)
	r.Rules = []Rule{{Route: RouteEscalate, Local: qwen["janus-local"]}}
	for i := range 5 {
		if p, _, _ := r.Explain(context.Background(), subagentHeader(), parse(t, opusRequest)); p.Backend.Name() != "local" {
			t.Fatalf("request %d went %s with no limit set", i, p.Backend.Name())
		}
	}
}

// A stand-in has nowhere else to go but Claude's error, so when the local
// model is at its limit it waits for a place rather than failing (#172):
// until a place is freed, the model's timeout passes, or the client goes.
func TestAStandInWaitsForABusyLocalModel(t *testing.T) {
	local, _ := lmStudio(t, nil)
	r := router(local, nil)
	one := qwen["janus-local"]
	one.MaxConcurrent = 1
	r.Rules = []Rule{{Route: RouteUpstream, IfDown: &one}}
	a, _, _ := r.Explain(context.Background(), mainHeader(), parse(t, opusRequest))
	b, _, _ := r.Explain(context.Background(), mainHeader(), parse(t, opusRequest))
	standIn, ok := a.Fallback(context.Background())
	if !ok || standIn.Release == nil {
		t.Fatalf("the first stand-in was refused, or holds no place")
	}

	type result struct {
		plan proxy.Plan
		ok   bool
	}
	second := make(chan result, 1)
	go func() {
		plan, ok := b.Fallback(context.Background())
		second <- result{plan, ok}
	}()
	select {
	case <-second:
		t.Fatal("a second stand-in went ahead while the first holds the only place")
	case <-time.After(100 * time.Millisecond):
	}
	standIn.Release()
	select {
	case got := <-second:
		if !got.ok {
			t.Fatalf("the waiting stand-in was refused after the place was freed: %s", got.plan.Reason)
		}
		got.plan.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting stand-in did not go ahead when the place was freed")
	}

	// It waits no longer than the model would be given to answer.
	one.Timeout = 150 * time.Millisecond
	c, _, _ := r.Explain(context.Background(), mainHeader(), parse(t, opusRequest))
	d, _, _ := r.Explain(context.Background(), mainHeader(), parse(t, opusRequest))
	holding, _ := c.Fallback(context.Background())
	defer holding.Release()
	refused, ok := d.Fallback(context.Background())
	if ok || !strings.Contains(refused.Reason, "had 1 requests in flight, its limit, for ") {
		t.Errorf("past the timeout: ok %v, reason %q", ok, refused.Reason)
	}

	// Nor once the client has gone.
	one.Timeout = time.Minute
	e, _, _ := r.Explain(context.Background(), mainHeader(), parse(t, opusRequest))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	begun := time.Now()
	if _, ok := e.Fallback(ctx); ok || time.Since(begun) > 5*time.Second {
		t.Errorf("after the client went: ok %v after %s", ok, time.Since(begun))
	}
}

// A model LM Studio has not loaded is loaded by Janus, at the context it
// chose, before the request is sent (#170); the plan records that. One
// already loaded, at a context too small for the request, is left alone:
// reloading it would throw away whatever else is using it.
func TestJanusLoadsAnUnloadedModelButNeverReloads(t *testing.T) {
	var loads []string
	listing := `{"data":[{"id":"qwen/qwen3.8-27b","state":"not-loaded","max_context_length":262144}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/models":
			io.WriteString(w, listing)
		case "/api/v1/models/load":
			body, _ := io.ReadAll(r.Body)
			loads = append(loads, string(body))
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			answerLoad(w, r)
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	r := router(backend.NewLocal(u, nil), nil)
	standIn := qwen["janus-local"]
	r.Rules = []Rule{{Route: RouteUpstream, IfDown: &standIn}}

	plan, _, _ := r.Explain(context.Background(), mainHeader(), parse(t, opusRequest))
	local, ok := plan.Fallback(context.Background())
	if !ok {
		t.Fatalf("no stand-in: %s", local.Reason)
	}
	local.Release()
	if len(loads) != 1 || !strings.Contains(loads[0], `"context_length":65536`) || !strings.Contains(loads[0], `"model":"qwen/qwen3.8-27b"`) {
		t.Errorf("loads = %q", loads)
	}
	if !slices.Contains(local.Stages, backend.StageLoad) {
		t.Errorf("stages = %v, want %s", local.Stages, backend.StageLoad)
	}

	// Loaded, but too small for this request: refused, and not reloaded.
	listing = `{"data":[{"id":"qwen/qwen3.8-27b","state":"loaded","loaded_context_length":4096,"max_context_length":262144}]}`
	r = router(backend.NewLocal(u, nil), nil)
	r.Rules = []Rule{{Route: RouteUpstream, IfDown: &standIn}}
	medium := `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"` + strings.Repeat("word ", 8000) + `"}]}`
	plan, _, _ = r.Explain(context.Background(), mainHeader(), parse(t, medium))
	if refused, ok := plan.Fallback(context.Background()); ok || !strings.Contains(refused.Reason, "more than the 3072 tokens local model") {
		t.Errorf("too small: ok %v, reason %q", ok, refused.Reason)
	}
	if len(loads) != 1 {
		t.Errorf("a loaded model was reloaded: %q", loads)
	}
}

// When a rule meant a request for the local model and sent it upstream
// without trying, the plan names why, so the ledger and janus stats can
// say what to do about it (#202).
func TestARulePassingOverTheLocalModelSaysWhy(t *testing.T) {
	big := `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"` + strings.Repeat("word ", 40000) + `"}]}`
	rule := func(local LocalModel) []Rule {
		return []Rule{{Name: "subagents", Class: Subagent, Route: RouteEscalate, Local: local}}
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	down, _ := url.Parse(gone.URL)
	gone.Close() // nothing listens there now
	lms, _ := lmStudio(t, nil)
	cases := map[string]struct {
		router *Router
		body   string
		cause  string
	}{
		"LM Studio not running": {&Router{Upstream: router(lms, nil).Upstream, Local: backend.NewLocal(down, nil), Rules: rule(qwen["janus-local"])}, opusRequest, proxy.CauseUnreachable},
		"model missing":         {&Router{Upstream: router(lms, nil).Upstream, Local: lms, Rules: rule(LocalModel{ID: "no-such-model", Timeout: time.Minute})}, opusRequest, proxy.CauseModelMissing},
		"too large":             {&Router{Upstream: router(lms, nil).Upstream, Local: lms, Rules: rule(qwen["janus-local"])}, big, proxy.CauseTooLarge},
	}
	for name, c := range cases {
		plan, _, _ := c.router.Explain(context.Background(), subagentHeader(), parse(t, c.body))
		if plan.Backend.Name() != "upstream" || plan.LocalCause != c.cause {
			t.Errorf("%s: went %s with cause %q, want upstream with %q (%s)", name, plan.Backend.Name(), plan.LocalCause, c.cause, plan.Reason)
		}
	}

	// Busy: the model's one place is taken.
	one := qwen["janus-local"]
	one.MaxConcurrent = 1
	r := &Router{Upstream: router(lms, nil).Upstream, Local: lms, Rules: rule(one)}
	first, _, _ := r.Explain(context.Background(), subagentHeader(), parse(t, opusRequest))
	defer first.Release()
	if second, _, _ := r.Explain(context.Background(), subagentHeader(), parse(t, opusRequest)); second.LocalCause != proxy.CauseBusy {
		t.Errorf("busy: cause %q, want %q", second.LocalCause, proxy.CauseBusy)
	}

	// Skipped: the breaker is open after repeated failures.
	for range 3 {
		r.health.record(false)
	}
	if skipped, _, _ := r.Explain(context.Background(), subagentHeader(), parse(t, opusRequest)); skipped.LocalCause != proxy.CauseSkipped {
		t.Errorf("skipped: cause %q, want %q", skipped.LocalCause, proxy.CauseSkipped)
	}
}
