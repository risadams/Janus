package mcp

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// session runs the server on in-memory pipes and lets a test send it lines
// and read its replies.
type session struct {
	t       *testing.T
	toSrv   *io.PipeWriter
	fromSrv *json.Decoder
	done    chan error
}

func start(t *testing.T, tools ...Tool) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &session{t: t, toSrv: inW, fromSrv: json.NewDecoder(outR), done: make(chan error, 1)}
	go func() {
		s.done <- Serve(context.Background(), inR, outW, "janus", "test", tools)
		outW.Close()
	}()
	t.Cleanup(func() { inW.Close() })
	return s
}

func (s *session) send(line string) {
	s.t.Helper()
	if _, err := io.WriteString(s.toSrv, line+"\n"); err != nil {
		s.t.Fatal(err)
	}
}

type reply struct {
	JSONRPC string
	ID      json.RawMessage
	Result  map[string]any
	Error   *struct {
		Code    int
		Message string
	}
}

func (s *session) read() reply {
	s.t.Helper()
	got := make(chan reply, 1)
	go func() {
		var r reply
		if err := s.fromSrv.Decode(&r); err == nil {
			got <- r
		}
	}()
	select {
	case r := <-got:
		if r.JSONRPC != "2.0" {
			s.t.Errorf("reply is not JSON-RPC 2.0: %+v", r)
		}
		return r
	case <-time.After(3 * time.Second):
		s.t.Fatal("no reply from the server")
		return reply{}
	}
}

var echo = Tool{
	Name: "echo", Description: "Says it back", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
	Call: func(ctx context.Context, args json.RawMessage) (string, bool) {
		var a struct{ Text string }
		json.Unmarshal(args, &a)
		if a.Text == "" {
			return "nothing to echo", true
		}
		return "you said " + a.Text, false
	},
}

func TestInitializeAgreesAVersion(t *testing.T) {
	s := start(t, echo)
	for asked, want := range map[string]string{"2025-06-18": "2025-06-18", "2024-11-05": "2024-11-05", "2099-01-01": "2025-06-18", "": "2025-06-18"} {
		s.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + asked + `","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
		r := s.read()
		if r.Result["protocolVersion"] != want {
			t.Errorf("asked for %q, got %v, want %q", asked, r.Result["protocolVersion"], want)
		}
		info, _ := r.Result["serverInfo"].(map[string]any)
		caps, _ := r.Result["capabilities"].(map[string]any)
		if info["name"] != "janus" || info["version"] != "test" || caps["tools"] == nil {
			t.Errorf("initialize result = %+v", r.Result)
		}
	}
}

func TestListAndCallTools(t *testing.T) {
	s := start(t, echo)

	s.send(`{"jsonrpc":"2.0","id":"list-1","method":"tools/list"}`)
	r := s.read()
	tools, _ := r.Result["tools"].([]any)
	if string(r.ID) != `"list-1"` || len(tools) != 1 {
		t.Fatalf("tools/list: id %s, result %+v", r.ID, r.Result)
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "echo" || tool["description"] != "Says it back" || tool["inputSchema"].(map[string]any)["type"] != "object" {
		t.Errorf("tool = %+v", tool)
	}

	s.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`)
	r = s.read()
	content := r.Result["content"].([]any)[0].(map[string]any)
	if string(r.ID) != "2" || content["type"] != "text" || content["text"] != "you said hi" || r.Result["isError"] != false {
		t.Errorf("tools/call = %+v", r.Result)
	}

	// A tool that fails says so in a result the model can read, not as a
	// protocol error.
	s.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo"}}`)
	r = s.read()
	if r.Error != nil || r.Result["isError"] != true || r.Result["content"].([]any)[0].(map[string]any)["text"] != "nothing to echo" {
		t.Errorf("failed call = %+v, error %+v", r.Result, r.Error)
	}
}

func TestProtocolErrors(t *testing.T) {
	s := start(t, echo)
	cases := []struct {
		line string
		code int
		has  string
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}`, invalidParams, `no tool called "nope"`},
		{`{"jsonrpc":"2.0","id":2,"method":"resources/list"}`, methodNotFound, `does not implement "resources/list"`},
		{`this is not json`, parseError, "not a JSON-RPC message"},
		{`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":"wrong"}`, invalidParams, "needs a name"},
	}
	for _, c := range cases {
		s.send(c.line)
		r := s.read()
		if r.Error == nil || r.Error.Code != c.code || !strings.Contains(r.Error.Message, c.has) {
			t.Errorf("%s: error = %+v, want %d containing %q", c.line, r.Error, c.code, c.has)
		}
	}
}

func TestNotificationsGetNoReplyAndPingDoes(t *testing.T) {
	s := start(t, echo)
	s.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	s.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":9}}`)
	s.send(``)
	s.send(`{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	// The only reply is to the ping: had a notification been answered, it
	// would have arrived first.
	if r := s.read(); string(r.ID) != "7" || r.Error != nil {
		t.Errorf("reply = %+v", r)
	}
}

// A slow tool call must not hold up anything else, and replies carry the
// id of the call they answer.
func TestCallsRunConcurrently(t *testing.T) {
	release := make(chan struct{})
	slow := Tool{Name: "slow", InputSchema: json.RawMessage(`{}`), Call: func(ctx context.Context, _ json.RawMessage) (string, bool) {
		<-release
		return "finally", false
	}}
	s := start(t, slow, echo)

	s.send(`{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{"name":"slow","arguments":{}}}`)
	s.send(`{"jsonrpc":"2.0","id":"b","method":"tools/call","params":{"name":"echo","arguments":{"text":"quick"}}}`)
	if r := s.read(); string(r.ID) != `"b"` {
		t.Fatalf("the quick call was held up behind the slow one: first reply was to %s", r.ID)
	}
	close(release)
	if r := s.read(); string(r.ID) != `"a"` || r.Result["content"].([]any)[0].(map[string]any)["text"] != "finally" {
		t.Errorf("slow reply = %+v", r)
	}
}

// When input ends, a call that is still running is allowed to finish and
// its answer is written before Serve returns.
func TestCallsInProgressFinishAfterInputEnds(t *testing.T) {
	release := make(chan struct{})
	slow := Tool{Name: "slow", InputSchema: json.RawMessage(`{}`), Call: func(ctx context.Context, _ json.RawMessage) (string, bool) {
		<-release
		if ctx.Err() != nil {
			return "cancelled", true
		}
		return "done", false
	}}
	var out strings.Builder
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow","arguments":{}}}`+"\n"), &out, "janus", "test", []Tool{slow})
	}()

	select {
	case <-done:
		t.Fatal("Serve returned while a call it had accepted was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil || !strings.Contains(out.String(), `"text":"done"`) {
			t.Errorf("Serve = %v, output %q", err, out.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return once the call finished")
	}
}

// Cancelling the context is how a caller stops calls in progress.
func TestCancellingTheContextStopsRunningTools(t *testing.T) {
	started := make(chan struct{})
	stuck := Tool{Name: "stuck", InputSchema: json.RawMessage(`{}`), Call: func(ctx context.Context, _ json.RawMessage) (string, bool) {
		close(started)
		<-ctx.Done()
		return "stopped", true
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stuck","arguments":{}}}`+"\n"), io.Discard, "janus", "test", []Tool{stuck})
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a tool that was told to stop kept Serve from returning")
	}
}
