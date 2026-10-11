package shadow

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/capture"
	"github.com/risadams/janus/internal/ledger"
	"github.com/risadams/janus/internal/proxy"
)

const modelList = `{"data":[{"id":"qwen/qwen3.8-27b","state":"loaded","loaded_context_length":61696,"max_context_length":262144}]}`

// stream is a local answer that calls the given tools and stops as told.
func stream(stop string, tools ...string) string {
	var b strings.Builder
	b.WriteString("event: message_start\n" + `data: {"type":"message_start","message":{"id":"m","model":"qwen/qwen3.8-27b","usage":{"input_tokens":300,"cache_read_input_tokens":100}}}` + "\n\n")
	for i, tool := range tools {
		fmt.Fprintf(&b, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"tool_use\",\"name\":%q,\"id\":\"t%d\",\"input\":{}}}\n\n", i, tool, i)
	}
	if len(tools) == 0 {
		b.WriteString("event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n")
	}
	fmt.Fprintf(&b, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q},\"usage\":{\"output_tokens\":40}}\n\n", stop)
	b.WriteString("event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n")
	return b.String()
}

type harness struct {
	runner   *Runner
	dir      string
	requests chan *http.Request // what the fake LM Studio was sent
	bodies   chan string
	logs     chan string
}

// start runs a Runner against a fake LM Studio whose Messages endpoint is
// handle.
func start(t *testing.T, handle http.HandlerFunc) *harness {
	t.Helper()
	h := &harness{dir: t.TempDir(), requests: make(chan *http.Request, 32), bodies: make(chan string, 32), logs: make(chan string, 64)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v0/models" {
			io.WriteString(w, modelList)
			return
		}
		body, _ := io.ReadAll(r.Body)
		h.requests <- r
		h.bodies <- string(body)
		handle(w, r)
	}))
	u, _ := url.Parse(srv.URL)
	h.runner = New(backend.NewLocal(u, nil), ledger.Open(filepath.Join(h.dir, "ledger")),
		capture.New(filepath.Join(h.dir, "captures"), 0, 0), func(f string, a ...any) { h.logs <- fmt.Sprintf(f, a...) })
	t.Cleanup(func() { h.runner.Close(); srv.Close() })
	return h
}

func answer(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
	}
}

// real is a finished upstream exchange that asked for a shadow: Claude
// called Read and Bash and stopped for tool use.
func real(id string) (proxy.Exchange, ledger.Record) {
	header := http.Header{}
	header.Set("Authorization", "Bearer secret-token")
	header.Set("Anthropic-Version", "2023-06-01")
	header.Set("X-Claude-Code-Session-Id", "s1")
	header.Set("X-Claude-Code-Agent-Id", "a1")
	ex := proxy.Exchange{
		ID: id, Start: time.Now(), Method: "POST", Path: "/v1/messages", Header: header,
		Status: 200, Duration: 2100 * time.Millisecond, Backend: "upstream", Route: "upstream", Reason: "rule watch",
		RequestBody: []byte(`{"model":"claude-opus-5-5","stream":true,"tools":[{"name":"Read"},{"name":"Bash"}],"messages":[{"role":"user","content":"count the lines"}]}`),
		Summary:     &anthropic.Summary{Model: "claude-opus-5-5", StopReason: "tool_use", Tools: []string{"Read", "Bash"}, Blocks: []string{"tool_use", "tool_use"}, Complete: true},
		Shadow:      &proxy.Shadow{Model: "qwen/qwen3.8-27b", Timeout: 5 * time.Second, Reason: "rule watch"},
	}
	rec := ledger.FromExchange(ex)
	rec.Capture = "2026-10-11/" + id + "_v1-messages"
	return ex, rec
}

// shadowRecord waits for the shadow of id to reach the ledger.
func (h *harness) shadowRecord(t *testing.T, id string) ledger.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		files, _ := filepath.Glob(filepath.Join(h.dir, "ledger", "*.jsonl"))
		for _, f := range files {
			file, err := os.Open(f)
			if err != nil {
				continue
			}
			sc := bufio.NewScanner(file)
			for sc.Scan() {
				var rec ledger.Record
				if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.ShadowOf == id {
					file.Close()
					return rec
				}
			}
			file.Close()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no shadow record for %s appeared in the ledger", id)
	return ledger.Record{}
}

func TestShadowThatAgrees(t *testing.T) {
	h := start(t, answer(stream("tool_use", "Bash", "Read"))) // same tools, other order
	ex, rec := real("r1")
	h.runner.Submit(ex, rec)
	got := h.shadowRecord(t, "r1")

	if got.ID != "r1-shadow" || got.Route != "shadow" || got.Backend != "local" || got.Reason != "rule watch" || got.Outcome != ledger.OutcomeOK {
		t.Errorf("record = %+v", got)
	}
	if got.Class != "subagent" || got.Session != "s1" || got.Agent != "a1" || got.Request == nil || got.Request.Model != "claude-opus-5-5" {
		t.Errorf("the shadow should carry the real request's identity: class %q session %q agent %q request %+v", got.Class, got.Session, got.Agent, got.Request)
	}
	if got.Response == nil || got.Response.Model != "qwen/qwen3.8-27b" || strings.Join(got.Response.Tools, ",") != "Bash,Read" {
		t.Errorf("response = %+v", got.Response)
	}
	// LM Studio's input figure includes its cache reads; the record
	// separates them, as for any local answer.
	if got.Usage == nil || got.Usage.Input != 200 || *got.Usage.CacheRead != 100 || got.Usage.Output != 40 {
		t.Errorf("usage = %+v", got.Usage)
	}
	c := got.Compare
	if c == nil || !c.StopMatch || !c.ToolsMatch || c.UpstreamStop != "tool_use" || c.UpstreamMs != 2100 {
		t.Errorf("compare = %+v, want both indicators to match", c)
	}

	// What LM Studio was sent: the local model, and no credentials.
	sent, body := <-h.requests, <-h.bodies
	if !strings.Contains(body, `"model":"qwen/qwen3.8-27b"`) || sent.Header.Get("Authorization") != "" {
		t.Errorf("LM Studio got body %s, auth %q", body, sent.Header.Get("Authorization"))
	}
	if sent.Header.Get("Anthropic-Version") != "2023-06-01" {
		t.Error("anthropic-version was not passed on")
	}

	// The capture sits beside the real one.
	base := filepath.Join(h.dir, "captures", rec.Capture)
	for _, ext := range []string{".shadow.md", ".shadow.request.txt", ".shadow.response.txt"} {
		data, err := os.ReadFile(base + ext)
		if err != nil || len(data) == 0 {
			t.Errorf("%s: %v (%d bytes)", ext, err, len(data))
		}
		if strings.Contains(string(data), "secret-token") {
			t.Errorf("%s contains the user's credential", ext)
		}
	}
}

func TestShadowThatDiffers(t *testing.T) {
	cases := map[string]struct {
		local              string
		wantStop, wantTool bool
	}{
		"answers in text instead of calling tools": {stream("end_turn"), false, false},
		"calls a different tool":                   {stream("tool_use", "Read", "Edit"), true, false},
		"calls one tool twice":                     {stream("tool_use", "Read", "Read"), true, false},
		"calls fewer tools":                        {stream("tool_use", "Read"), true, false},
	}
	for name, c := range cases {
		h := start(t, answer(c.local))
		ex, rec := real("r1")
		h.runner.Submit(ex, rec)
		got := h.shadowRecord(t, "r1").Compare
		if got == nil || got.StopMatch != c.wantStop || got.ToolsMatch != c.wantTool {
			t.Errorf("%s: compare = %+v, want stop %v tools %v", name, got, c.wantStop, c.wantTool)
		}
	}
}

func TestShadowFailuresAreRecorded(t *testing.T) {
	cases := map[string]struct {
		handle      http.HandlerFunc
		wantOutcome string
	}{
		"local error": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"no"}}`)
		}, ledger.OutcomeAPIError},
		"cut off": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, stream("tool_use", "Read")[:120])
		}, ledger.OutcomeIncomplete},
		"connection dropped": {func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) }, ledger.OutcomeNoResponse},
	}
	for name, c := range cases {
		h := start(t, c.handle)
		ex, rec := real("r1")
		h.runner.Submit(ex, rec)
		got := h.shadowRecord(t, "r1")
		if got.Outcome != c.wantOutcome || got.Compare != nil {
			t.Errorf("%s: outcome %q, compare %+v; want %q and no comparison", name, got.Outcome, got.Compare, c.wantOutcome)
		}
	}
}

func TestShadowTimeout(t *testing.T) {
	h := start(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ex, rec := real("r1")
	ex.Shadow.Timeout = 100 * time.Millisecond
	h.runner.Submit(ex, rec)
	if got := h.shadowRecord(t, "r1"); got.Outcome != ledger.OutcomeNoResponse || !strings.Contains(got.Reason, "deadline") {
		t.Errorf("outcome %q, reason %q", got.Outcome, got.Reason)
	}
}

func TestShadowSkips(t *testing.T) {
	h := start(t, answer(stream("end_turn")))

	big, bigRec := real("big")
	big.RequestBody = []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"` + strings.Repeat("word ", 40000) + `"}]}`)
	missing, missingRec := real("missing")
	missing.Shadow.Model = "no-such-model"
	garbled, garbledRec := real("garbled")
	garbled.RequestBody = []byte("not json")

	h.runner.Submit(big, bigRec)
	h.runner.Submit(missing, missingRec)
	h.runner.Submit(garbled, garbledRec)

	for id, want := range map[string]string{
		"big":     "more than the model's 61696-token context",
		"missing": `LM Studio does not have local model "no-such-model"`,
		"garbled": "could not be read back",
	} {
		got := h.shadowRecord(t, id)
		if got.Outcome != ledger.OutcomeSkipped || !strings.Contains(got.Reason, want) || !strings.HasPrefix(got.Reason, "rule watch: ") {
			t.Errorf("%s: outcome %q, reason %q; want skipped with %q", id, got.Outcome, got.Reason, want)
		}
	}
	select {
	case body := <-h.bodies:
		t.Errorf("a skipped shadow was sent to LM Studio anyway: %.80s", body)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestOnlyCompleteUpstreamAnswersAreShadowed(t *testing.T) {
	h := start(t, answer(stream("end_turn")))

	notAsked, rec := real("a")
	notAsked.Shadow = nil
	failed, _ := real("b")
	failed.Summary = &anthropic.Summary{Complete: true, Error: &anthropic.APIError{Type: "rate_limit_error"}}
	cutOff, _ := real("c")
	cutOff.Summary.Complete = false
	unread, _ := real("d")
	unread.Summary = nil

	for _, ex := range []proxy.Exchange{notAsked, failed, cutOff, unread} {
		h.runner.Submit(ex, rec)
	}
	select {
	case body := <-h.bodies:
		t.Errorf("a shadow ran with no complete real answer to compare with: %.80s", body)
	case <-time.After(200 * time.Millisecond):
	}
}

// Submit must return at once whatever the local model is doing, and a
// burst beyond the queue is dropped and recorded, not left to pile up.
func TestSubmitNeverBlocksAndOverflowIsRecorded(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	h := start(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		answer(stream("end_turn"))(w, r)
	})
	defer once.Do(func() { close(release) })

	const burst = queueSize + 6
	started := time.Now()
	for i := 0; i < burst; i++ {
		ex, rec := real(fmt.Sprintf("r%02d", i))
		h.runner.Submit(ex, rec)
	}
	if took := time.Since(started); took > time.Second {
		t.Fatalf("submitting %d shadows took %s with LM Studio stuck; Submit must not wait", burst, took)
	}

	// One is in progress and queueSize are waiting, so the rest overflowed.
	// Those are in the ledger already, as skipped.
	dropped := 0
	deadline := time.Now().Add(3 * time.Second)
	for dropped < burst-queueSize-1 && time.Now().Before(deadline) {
		dropped = 0
		files, _ := filepath.Glob(filepath.Join(h.dir, "ledger", "*.jsonl"))
		for _, f := range files {
			data, _ := os.ReadFile(f)
			dropped += strings.Count(string(data), "the shadow queue is full")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if dropped != burst-queueSize-1 {
		t.Errorf("%d shadows recorded as dropped, want %d", dropped, burst-queueSize-1)
	}
	once.Do(func() { close(release) })
}

func TestCloseAbandonsWorkInProgress(t *testing.T) {
	h := start(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ex, rec := real("r1")
	ex.Shadow.Timeout = 0
	h.runner.Submit(ex, rec)
	<-h.requests // the shadow is now in flight

	done := make(chan struct{})
	go func() { h.runner.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close waited for a shadow that will never finish")
	}
}

func TestSameTools(t *testing.T) {
	cases := []struct {
		a, b []string
		want bool
	}{
		{nil, nil, true},
		{[]string{}, nil, true},
		{[]string{"Read", "Bash"}, []string{"Bash", "Read"}, true},
		{[]string{"Read", "Read"}, []string{"Read"}, false},
		{[]string{"Read", "Read", "Bash"}, []string{"Read", "Bash", "Bash"}, false},
		{[]string{"Read"}, nil, false},
	}
	for _, c := range cases {
		if got := sameTools(c.a, c.b); got != c.want {
			t.Errorf("sameTools(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
