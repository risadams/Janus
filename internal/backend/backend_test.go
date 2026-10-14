package backend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/risadams/janus/internal/anthropic"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUpstreamPrepare(t *testing.T) {
	up := NewUpstream(mustURL(t, "https://api.anthropic.com"), nil)
	if up.Name() != "upstream" || up.URL().Host != "api.anthropic.com" {
		t.Errorf("name=%q url=%v", up.Name(), up.URL())
	}

	cases := []struct {
		name     string
		offered  []string
		messages bool
		want     string
	}{
		{"claude code's list", []string{"gzip, deflate, br, zstd"}, true, "gzip"},
		{"weighted", []string{"br;q=1.0, gzip;q=0.8"}, true, "gzip"},
		{"wildcard", []string{"*"}, true, "gzip"},
		{"gzip refused", []string{"br, gzip;q=0"}, true, "identity"},
		{"only unreadable codings", []string{"br, zstd"}, true, "identity"},
		{"nothing offered", nil, true, ""},
		{"not a messages call", []string{"gzip, deflate, br, zstd"}, false, "gzip, deflate, br, zstd"},
	}
	for _, c := range cases {
		req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
		for _, v := range c.offered {
			req.Header.Add("Accept-Encoding", v)
		}
		req.Header.Set("Authorization", "Bearer secret-token")
		up.Prepare(req, c.messages)

		if got := strings.Join(req.Header.Values("Accept-Encoding"), ", "); got != c.want {
			t.Errorf("%s: Accept-Encoding = %q, want %q", c.name, got, c.want)
		}
		if req.Header.Get("Authorization") != "Bearer secret-token" {
			t.Errorf("%s: the upstream request lost its credentials", c.name)
		}
	}
}

func TestLocalPrepareStripsCredentials(t *testing.T) {
	local := NewLocal(mustURL(t, "http://studio.local:1234"), nil)
	req, _ := http.NewRequest("POST", "http://studio.local:1234/v1/messages", nil)
	for k, v := range map[string]string{
		"Authorization": "Bearer secret-token", "X-Api-Key": "sk-secret", "Cookie": "c=1", "Proxy-Authorization": "Basic x",
		"Anthropic-Version": "2023-06-01", "Accept-Encoding": "gzip, br", "X-Claude-Code-Session-Id": "s1",
	} {
		req.Header.Set(k, v)
	}
	local.Prepare(req, true)

	for _, h := range []string{"Authorization", "X-Api-Key", "Cookie", "Proxy-Authorization"} {
		if req.Header.Get(h) != "" {
			t.Errorf("%s was sent to the local server", h)
		}
	}
	if req.Header.Get("Accept-Encoding") != "identity" {
		t.Errorf("Accept-Encoding = %q, want identity", req.Header.Get("Accept-Encoding"))
	}
	if req.Header.Get("Anthropic-Version") == "" || req.Header.Get("X-Claude-Code-Session-Id") == "" {
		t.Error("headers that are not credentials were removed")
	}
}

func lmStudio(t *testing.T, status int, body string, hits *int) *Local {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			*hits++
		}
		if r.URL.Path != "/api/v0/models" {
			t.Errorf("asked for %s, want /api/v0/models", r.URL.Path)
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return NewLocal(mustURL(t, srv.URL+"/"), nil)
}

const modelList = `{"data":[
	{"id":"qwen/qwen3.8-27b","state":"loaded","loaded_context_length":61696,"max_context_length":262144},
	{"id":"gemma-4-e4b","state":"not-loaded","max_context_length":131072}]}`

func TestLocalModels(t *testing.T) {
	hits := 0
	local := lmStudio(t, 200, modelList, &hits)

	models, err := local.Models(context.Background())
	if err != nil || len(models) != 2 {
		t.Fatalf("Models() = %+v, %v", models, err)
	}
	if want := (Model{ID: "qwen/qwen3.8-27b", Loaded: true, LoadedContext: 61696, MaxContext: 262144}); models[0] != want {
		t.Errorf("models[0] = %+v, want %+v", models[0], want)
	}
	if models[1].Loaded || models[1].LoadedContext != 0 {
		t.Errorf("models[1] = %+v, want not loaded", models[1])
	}

	m, found, err := local.Model(context.Background(), "gemma-4-e4b")
	if err != nil || !found || m.MaxContext != 131072 {
		t.Errorf("Model(gemma) = %+v, %v, %v", m, found, err)
	}
	if _, found, _ := local.Model(context.Background(), "missing"); found {
		t.Error("Model reports a model LM Studio does not have")
	}
	if hits != 1 {
		t.Errorf("LM Studio was asked %d times for three lookups in a row, want 1", hits)
	}
	if err := local.Health(context.Background()); err != nil {
		t.Errorf("Health() = %v", err)
	}
}

func TestLocalModelsFailures(t *testing.T) {
	// What LM Studio says for a path it does not know: HTTP 200 and an error.
	notLMS := lmStudio(t, 200, `{"error":"Unexpected endpoint or method. (GET /api/v0/models)"}`, nil)
	if _, err := notLMS.Models(context.Background()); !errors.Is(err, ErrNotLMStudio) {
		t.Errorf("err = %v, want ErrNotLMStudio", err)
	}

	dead := httptest.NewServer(http.NotFoundHandler())
	u := mustURL(t, dead.URL)
	dead.Close()
	local := NewLocal(u, nil)
	if err := local.Health(context.Background()); err == nil || errors.Is(err, ErrNotLMStudio) {
		t.Errorf("Health of a dead server = %v, want a connection error", err)
	}
	// A failure is not cached: the next call tries again.
	if _, err := local.Models(context.Background()); err == nil {
		t.Error("a second call after a failure succeeded without a server")
	}
}

func loadRequest(t *testing.T) *anthropic.Request {
	t.Helper()
	raw, err := os.ReadFile("../anthropic/testdata/request.json")
	if err != nil {
		t.Fatal(err)
	}
	req, err := anthropic.ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// The synthetic request has what a real Claude Code request has: two
// tool_addition blocks in a system message, another system message later
// on, and a deferred placeholder tool.
func TestNormalize(t *testing.T) {
	req := loadRequest(t)
	before := req.Messages()

	stages, err := Normalize(req, "qwen/qwen3.8-27b")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stages, ","); got != "set_model,lift_tool_additions,fold_system_messages,drop_deferred_tools" {
		t.Errorf("stages = %s", got)
	}

	out, err := anthropic.ParseRequest(req.Bytes())
	if err != nil {
		t.Fatalf("normalised request does not parse: %v", err)
	}
	if out.Model() != "qwen/qwen3.8-27b" {
		t.Errorf("model = %q", out.Model())
	}
	var names []string
	for _, tool := range out.Tools() {
		names = append(names, tool.Name)
		if _, deferred := tool.Field("defer_loading"); deferred {
			t.Errorf("tool %s still carries defer_loading", tool.Name)
		}
	}
	if got := strings.Join(names, ","); got != "Read,Bash,mcp__docs__search,mcp__docs__read" {
		t.Errorf("tools = %s", got)
	}
	if strings.Contains(string(req.Bytes()), "tool_addition") {
		t.Error("tool_addition blocks remain")
	}

	// The two system messages are folded into the user messages before
	// them, as reminders; every other message is byte for byte what it was.
	after := out.Messages()
	if len(after) != len(before)-2 {
		t.Fatalf("%d messages after normalising, want %d", len(after), len(before)-2)
	}
	for i, m := range after {
		if m.Role == "system" {
			t.Errorf("messages[%d] is still a system message", i)
		}
	}
	reminder := func(text string) string { return "<system-reminder>\n" + text + "\n</system-reminder>" }
	first := after[0].Blocks()
	if len(first) != 3 || first[2].Text() != reminder("Deferred tools are now available.") {
		t.Errorf("messages[0] = %d blocks, last %q", len(first), first[len(first)-1].Text())
	}
	third := after[2].Blocks()
	if after[2].Role != "user" || len(third) != 2 || third[0].Type != "tool_result" || third[1].Text() != reminder("The user changed the working directory.") {
		t.Errorf("messages[2] = role %q, %d blocks", after[2].Role, len(third))
	}
	for i, j := range map[int]int{1: 2, 3: 5, 4: 6} {
		if string(after[i].Raw()) != string(before[j].Raw()) {
			t.Errorf("messages[%d] changed though it was not a system message's neighbour", j)
		}
	}
	// Fields LM Studio tolerates are left in.
	for _, field := range []string{"context_management", "output_config", "thinking", "metadata", "future_top_level_field"} {
		if _, ok := out.Field(field); !ok {
			t.Errorf("%s was removed", field)
		}
	}
}

func TestNormalizePlainRequestOnlySetsTheModel(t *testing.T) {
	raw := `{"model":"janus-local","max_tokens":64,"tools":[{"name":"Read"}],"messages":[{"role":"user","content":"hi"}]}`
	req, _ := anthropic.ParseRequest([]byte(raw))

	stages, err := Normalize(req, "qwen/qwen3.8-27b")
	if err != nil || strings.Join(stages, ",") != "set_model" {
		t.Errorf("stages = %v, err = %v", stages, err)
	}
	want := strings.Replace(raw, `"janus-local"`, `"qwen/qwen3.8-27b"`, 1)
	if string(req.Bytes()) != want {
		t.Errorf("body = %s\nwant   %s", req.Bytes(), want)
	}
}

func TestNormalizeEdgeCases(t *testing.T) {
	addition := `{"type":"tool_addition","tool":{"type":"custom","definition":{"name":"Read","description":"again"}}}`

	// A message holding nothing but tool additions must not become empty,
	// and a definition for a tool already present is not added twice.
	req, _ := anthropic.ParseRequest([]byte(`{"model":"m","tools":[{"name":"Read"}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":[` + addition + `]}]}`))
	if _, err := Normalize(req, "local"); err != nil {
		t.Fatal(err)
	}
	out, _ := anthropic.ParseRequest(req.Bytes())
	if len(out.Tools()) != 1 {
		t.Errorf("%d tools, want the duplicate definition ignored", len(out.Tools()))
	}
	// The emptied system message says what was there, and is then folded
	// into the user message before it like any other.
	if m := out.Messages(); len(m) != 1 || len(m[0].Blocks()) != 2 || !strings.Contains(m[0].Blocks()[1].Text(), "tool definitions were added here") {
		t.Errorf("messages = %+v, want the placeholder folded into the user message", m)
	}

	// A tool_addition janus cannot read is an error, not a silent drop:
	// dropping it would send the model a request missing a tool.
	bad, _ := anthropic.ParseRequest([]byte(`{"model":"m","messages":[{"role":"system","content":[{"type":"tool_addition"}]}]}`))
	if _, err := Normalize(bad, "local"); err == nil {
		t.Error("a tool_addition with no definition was accepted")
	}
}

// After a tool search Claude Code sends the result as tool_reference
// blocks, which LM Studio refuses: "request.messages.8.content.0.content.0.type:
// Invalid literal value, expected \"text\"" (#169). They go to the local
// model as text, and the rest of the tool result stays as it was.
func TestNormalizeTurnsToolReferencesIntoText(t *testing.T) {
	raw := `{"model":"claude-opus-5-5","tools":[{"name":"Bash"},{"name":"ToolSearch"}],"messages":[` +
		`{"role":"user","content":"tag a release"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"ToolSearch","input":{"query":"select:Bash,TaskCreate"}},{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"git tag"}}]},` +
		`{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"t1","content":[{"type":"tool_reference","tool_name":"Bash"},{"type":"tool_reference","tool_name":"TaskCreate"}]},` +
		`{"type":"tool_result","tool_use_id":"t2","content":"v0.9.0-rc.7"},` +
		`{"type":"text","text":"go on"}]}]}`
	req, err := anthropic.ParseRequest([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	before := req.Messages()

	stages, err := Normalize(req, "qwen/qwen3.8-27b")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stages, ","); got != "set_model,tool_references_as_text" {
		t.Errorf("stages = %s", got)
	}
	if strings.Contains(string(req.Bytes()), "tool_reference") {
		t.Fatalf("tool_reference remains: %s", req.Bytes())
	}
	out, err := anthropic.ParseRequest(req.Bytes())
	if err != nil {
		t.Fatalf("normalised request does not parse: %v", err)
	}
	last := out.Messages()[2].Blocks()
	var result anthropic.ToolResultBlock
	last[0].Decode(&result)
	content, _ := anthropic.ParseContent(result.Content)
	var texts []string
	for _, c := range content {
		texts = append(texts, c.Type+": "+c.Text())
	}
	want := "text: Tool Bash is loaded and can be called.|text: Tool TaskCreate was found, but it cannot be called here."
	if got := strings.Join(texts, "|"); got != want || result.ToolUseID != "t1" {
		t.Errorf("tool result %s = %s\nwant %s", result.ToolUseID, got, want)
	}
	if string(last[1].Raw()) != `{"type":"tool_result","tool_use_id":"t2","content":"v0.9.0-rc.7"}` || last[2].Text() != "go on" {
		t.Errorf("the other blocks changed: %s %s", last[1].Raw(), last[2].Raw())
	}
	for i := 0; i < 2; i++ {
		if string(out.Messages()[i].Raw()) != string(before[i].Raw()) {
			t.Errorf("messages[%d] changed", i)
		}
	}
}

func rename(t *testing.T, contentType, body string, chunk int) string {
	t.Helper()
	var src io.Reader = strings.NewReader(body)
	if chunk > 0 {
		src = iotest.OneByteReader(src)
	}
	out, err := io.ReadAll(RenameModel(io.NopCloser(src), contentType, "janus-local"))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

const localStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","type":"message","content":[],"model":"qwen/qwen3.8-27b","usage":{"input_tokens":363}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I am qwen/qwen3.8-27b, a \"model\""}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func TestRenameModelInStream(t *testing.T) {
	want := strings.Replace(localStream, `"model":"qwen/qwen3.8-27b"`, `"model":"janus-local"`, 1)
	for _, chunk := range []int{0, 1} {
		got := rename(t, "text/event-stream; charset=utf-8", localStream, chunk)
		if got != want {
			t.Errorf("chunk %d:\n got %q\nwant %q", chunk, got, want)
		}
	}
	// The model's own words about itself are content, and stay.
	if !strings.Contains(want, "I am qwen/qwen3.8-27b") {
		t.Fatal("test is wrong: the text mention should survive")
	}
}

func TestRenameModelInStreamVariants(t *testing.T) {
	crlf := strings.ReplaceAll(localStream, "\n", "\r\n")
	if got, want := rename(t, "text/event-stream", crlf, 1), strings.Replace(crlf, `"model":"qwen/qwen3.8-27b"`, `"model":"janus-local"`, 1); got != want {
		t.Errorf("CRLF stream:\n got %q\nwant %q", got, want)
	}

	// No message_start, or a stream cut off mid-line: everything passes
	// through exactly, including the unfinished tail.
	for name, body := range map[string]string{
		"error only": "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"x\"}}\n\n",
		"cut off":    localStream[:60],
		"empty":      "",
	} {
		if got := rename(t, "text/event-stream", body, 1); got != body {
			t.Errorf("%s: got %q, want it unchanged", name, got)
		}
	}
}

func TestRenameModelInJSON(t *testing.T) {
	body := `{"id":"msg_1","type":"message","content":[{"type":"text","text":"hi"}],"model":"qwen/qwen3.8-27b","stop_reason":"end_turn"}`
	want := strings.Replace(body, "qwen/qwen3.8-27b", "janus-local", 1)
	for _, chunk := range []int{0, 1} {
		if got := rename(t, "application/json", body, chunk); got != want {
			t.Errorf("chunk %d: got %s", chunk, got)
		}
	}

	for name, other := range map[string]string{
		"an API error":                  `{"type":"error","error":{"type":"invalid_request_error","message":"request.max_tokens: Required"}}`,
		"LM Studio's other error shape": `{"error":{"message":"Invalid body","type":"invalid_request","code":"invalid_json"}}`,
		"not json":                      "<html>502</html>",
		"empty":                         "",
	} {
		if got := rename(t, "application/json", other, 0); got != other {
			t.Errorf("%s: got %q, want it unchanged", name, got)
		}
	}
}

func TestRenameModelPassesOnAReadFailure(t *testing.T) {
	boom := errors.New("connection reset")
	for _, contentType := range []string{"text/event-stream", "application/json"} {
		src := io.MultiReader(strings.NewReader(`{"type":"message","model":"x"`), iotest.ErrReader(boom))
		got, err := io.ReadAll(RenameModel(io.NopCloser(src), contentType, "janus-local"))
		if !errors.Is(err, boom) || string(got) != `{"type":"message","model":"x"` {
			t.Errorf("%s: got %q, err %v; want what arrived and then the failure", contentType, got, err)
		}
	}
}

func TestRepairForUpstreamDropsOnlyUnsignedThinking(t *testing.T) {
	// A conversation with one turn from Claude (signed thinking, empty
	// text, as Claude Code receives it) and one from a local model
	// (unsigned thinking, in the two forms it can arrive in).
	claude := `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"c2lnbmVk"},{"type":"tool_use","id":"toolu_01","name":"Read","input":{}}]}`
	raw := `{"model":"claude-opus-5-5","messages":[` +
		`{"role":"user","content":"count the lines"},` +
		claude + `,` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"2"}]},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"The user wants a count","signature":""},{"type":"tool_use","id":"587714761","name":"Bash","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"587714761","content":"1"}]},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"no signature field at all"}]},` +
		`{"role":"user","content":[{"type":"thinking","thinking":"not an assistant turn","signature":""}]}` +
		`]}`
	req, err := anthropic.ParseRequest([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}

	if !RepairForUpstream(req) {
		t.Fatal("nothing was repaired")
	}
	out, err := anthropic.ParseRequest(req.Bytes())
	if err != nil {
		t.Fatalf("repaired request does not parse: %v", err)
	}
	msgs := out.Messages()

	if string(msgs[1].Raw()) != claude {
		t.Errorf("Claude's own turn was changed:\n%s", msgs[1].Raw())
	}
	if blocks := msgs[3].Blocks(); len(blocks) != 1 || blocks[0].Type != "tool_use" {
		t.Errorf("the local turn should keep only its tool call: %s", msgs[3].Raw())
	}
	if blocks := msgs[5].Blocks(); len(blocks) != 1 || blocks[0].Type != "text" {
		t.Errorf("a turn that was only thinking should become a placeholder, not empty: %s", msgs[5].Raw())
	}
	if blocks := msgs[6].Blocks(); len(blocks) != 1 || blocks[0].Type != "thinking" {
		t.Errorf("only assistant turns are repaired: %s", msgs[6].Raw())
	}
	for _, i := range []int{0, 2, 4} {
		if string(msgs[i].Raw()) != string(req.Messages()[i].Raw()) || !strings.Contains(raw, string(msgs[i].Raw())) {
			t.Errorf("messages[%d] changed", i)
		}
	}
}

// A conversation that only ever went to Claude must come through
// untouched, byte for byte.
func TestRepairForUpstreamLeavesClaudeOnlyHistoryAlone(t *testing.T) {
	req := loadRequest(t) // has a signed thinking block with empty text
	before := string(req.Bytes())
	if RepairForUpstream(req) || req.Modified() || string(req.Bytes()) != before {
		t.Error("a request with no unsigned thinking was modified")
	}
}

// A system message with no user message before it becomes one; an empty
// one there is dropped. And folding is stable: a conversation that has
// grown by a turn folds to the same bytes as before, plus the new turn,
// which is what keeps the local model's prompt cache useful.
func TestFoldSystem(t *testing.T) {
	msgs := func(raw string) []anthropic.Message {
		req, err := anthropic.ParseRequest([]byte(`{"model":"m","messages":` + raw + `}`))
		if err != nil {
			t.Fatal(err)
		}
		return req.Messages()
	}

	out, folded := foldSystem(msgs(`[{"role":"system","content":"first"},{"role":"system","content":[]},{"role":"user","content":"hi"}]`))
	if !folded || len(out) != 2 || out[0].Role != "user" || out[0].Blocks()[0].Text() != "<system-reminder>\nfirst\n</system-reminder>" || out[1].Role != "user" {
		t.Errorf("leading system messages: %d messages, roles %q %q", len(out), out[0].Role, out[len(out)-1].Role)
	}

	if _, folded := foldSystem(msgs(`[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]`)); folded {
		t.Error("a conversation with no system message was reported as folded")
	}

	short := `[{"role":"user","content":"read it"},{"role":"system","content":"reminder"},{"role":"assistant","content":"done"}`
	a, _ := foldSystem(msgs(short + `]`))
	b, _ := foldSystem(msgs(short + `,{"role":"user","content":"thanks"},{"role":"system","content":"another"}]`))
	for i := range a {
		if string(a[i].Raw()) != string(b[i].Raw()) {
			t.Errorf("messages[%d] folds differently once the conversation grows", i)
		}
	}
}

// Requests that find the model unloaded together share one load: a second
// load would start a second instance of the model in the same memory
// (#170). The load asks for the context Janus chose, and what LM Studio
// reports back is what the caller gets.
func TestLoadIsSharedByRequestsArrivingTogether(t *testing.T) {
	var loads atomic.Int32
	var asked string
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models/load" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return
		}
		loads.Add(1)
		body, _ := io.ReadAll(r.Body)
		asked = string(body)
		<-release
		io.WriteString(w, `{"type":"llm","instance_id":"qwen/qwen3.8-27b","load_time_seconds":9.1,"status":"loaded","load_config":{"context_length":65536,"flash_attention":true}}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	local := NewLocal(u, nil)

	var wg sync.WaitGroup
	results := make(chan Model, 3)
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := local.Load(context.Background(), "qwen/qwen3.8-27b", 65536)
			if err != nil {
				t.Error(err)
			}
			results <- m
		}()
	}
	// A caller that gives up returns at once, and does not stop the load
	// for the others.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := local.Load(ctx, "qwen/qwen3.8-27b", 65536); err == nil {
		t.Error("a caller that gave up was not told so")
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)

	if n := loads.Load(); n != 1 {
		t.Errorf("%d loads, want 1", n)
	}
	if asked != `{"context_length":65536,"echo_load_config":true,"model":"qwen/qwen3.8-27b"}` {
		t.Errorf("load asked for %s", asked)
	}
	for m := range results {
		if !m.Loaded || m.LoadedContext != 65536 || m.ID != "qwen/qwen3.8-27b" {
			t.Errorf("loaded model = %+v", m)
		}
	}
}

// A load LM Studio refuses is explained with what it said.
func TestLoadFailureIsExplained(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":{"message":"Not enough memory to load the model"}}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	_, err := NewLocal(u, nil).Load(context.Background(), "qwen/qwen3.8-27b", 65536)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") || !strings.Contains(err.Error(), "Not enough memory") {
		t.Errorf("err = %v", err)
	}
}
