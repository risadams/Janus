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
			400, "invalid_request_error", `mapped to local model "no-such-model", which LM Studio does not have`},
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
	up := httptest.NewServer(upstream)
	defer up.Close()
	u, _ := url.Parse(up.URL)
	r := &Router{Upstream: backend.NewUpstream(u, nil), Local: local, Aliases: aliases}

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
