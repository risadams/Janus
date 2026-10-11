package anthropic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func val(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

// feed writes body to a new Observer in pieces of the given size, so the
// same input is seen whole, in typical reads, and one byte at a time.
func feed(contentType string, body []byte, size int) Summary {
	o := NewObserver(contentType)
	for len(body) > 0 {
		n := min(size, len(body))
		o.Write(body[:n])
		body = body[n:]
	}
	return o.Summary()
}

// testdata/stream.sse is a synthetic stream in the shape the Anthropic API
// sends Claude Code: thinking with a signature, a ping, a tool call whose
// arguments arrive in pieces, and usage reported at both ends.
func TestObserveStream(t *testing.T) {
	body, err := os.ReadFile("testdata/stream.sse")
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{len(body), 4096, 7, 1} {
		s := feed("text/event-stream; charset=utf-8", body, size)

		if s.ID != "msg_01EXAMPLE" || s.Model != "claude-opus-5-5" || s.StopReason != "tool_use" {
			t.Errorf("size %d: id=%q model=%q stop=%q", size, s.ID, s.Model, s.StopReason)
		}
		if got := strings.Join(s.Blocks, ","); got != "thinking,text,tool_use" {
			t.Errorf("size %d: blocks = %s", size, got)
		}
		if !s.Complete || s.Error != nil || s.Malformed != 0 {
			t.Errorf("size %d: complete=%v error=%v malformed=%d", size, s.Complete, s.Error, s.Malformed)
		}
		if got := strings.Join(s.Tools, ","); got != "Read" {
			t.Errorf("size %d: tools = %q, want Read", size, got)
		}
		u := s.Usage
		if u.Input != 2 || u.Output != 249 || val(u.CacheRead) != 8103 || val(u.CacheWrite) != 14299 ||
			val(u.CacheWrite5m) != 0 || val(u.CacheWrite1h) != 14299 || val(u.Thinking) != 124 {
			t.Errorf("size %d: usage = in %d out %d read %d write %d (5m %d, 1h %d) thinking %d", size,
				u.Input, u.Output, val(u.CacheRead), val(u.CacheWrite), val(u.CacheWrite5m), val(u.CacheWrite1h), val(u.Thinking))
		}
	}
}

// LM Studio's stream, as recorded in docs/research/lmstudio.md: no cache
// writes reported, CRLF tolerated, and the whole tool input in one delta.
func TestObserveStreamWithSparseUsage(t *testing.T) {
	body := strings.ReplaceAll(`event: message_start
data: {"type":"message_start","message":{"id":"msg_local","type":"message","role":"assistant","content":[],"model":"qwen/qwen3.8-27b","usage":{"input_tokens":363,"output_tokens":0,"cache_read_input_tokens":256}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"197666655","name":"Bash","input":{}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":30}}

event: message_stop
data: {"type":"message_stop"}

`, "\n", "\r\n")

	s := feed("text/event-stream", []byte(body), 5)
	u := s.Usage
	if u.Input != 363 || u.Output != 30 || val(u.CacheRead) != 256 {
		t.Errorf("usage = in %d out %d read %d; the final report omitted input and cache read, which must keep their earlier values", u.Input, u.Output, val(u.CacheRead))
	}
	if u.CacheWrite != nil || u.CacheWrite1h != nil || u.Thinking != nil {
		t.Error("fields the server never sent are reported as present")
	}
	if !s.Complete || s.StopReason != "tool_use" || s.Model != "qwen/qwen3.8-27b" {
		t.Errorf("summary = %+v", s)
	}
}

func TestObserveCutOffStream(t *testing.T) {
	body, _ := os.ReadFile("testdata/stream.sse")
	cut := body[:strings.Index(string(body), "event: message_delta")+25] // mid-line

	s := feed("text/event-stream", cut, 64)
	if s.Complete {
		t.Error("a stream that never reached message_stop is reported complete")
	}
	if s.StopReason != "" || s.Usage.Output != 4 {
		t.Errorf("stop=%q output=%d, want only what message_start reported", s.StopReason, s.Usage.Output)
	}
	if len(s.Blocks) != 3 {
		t.Errorf("blocks = %v, want the three that started before the cut", s.Blocks)
	}
}

func TestObserveStreamErrorEvent(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"x\",\"usage\":{\"input_tokens\":9}}}\n\n" +
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"

	s := feed("text/event-stream", []byte(body), 16)
	if s.Error == nil || s.Error.Type != "overloaded_error" || s.Error.Message != "Overloaded" {
		t.Errorf("error = %+v", s.Error)
	}
	if s.Complete || s.Usage.Input != 9 {
		t.Errorf("complete=%v input=%d", s.Complete, s.Usage.Input)
	}
}

func TestObserveStreamSkipsWhatItCannotRead(t *testing.T) {
	body := ": keep-alive comment\n\n" +
		"event: content_block_start\ndata: {not json\n\n" +
		"event: something_new\ndata: {\"type\":\"something_new\",\"x\":1}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	s := feed("text/event-stream", []byte(body), 3)
	if s.Malformed != 1 || !s.Complete || len(s.Blocks) != 0 {
		t.Errorf("malformed=%d complete=%v blocks=%v", s.Malformed, s.Complete, s.Blocks)
	}
}

func TestObserveJSONBody(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"Ready."}],
		"model":"qwen/qwen3.8-27b","stop_reason":"end_turn","usage":{"input_tokens":17,"output_tokens":3,"cache_read_input_tokens":0}}`

	for _, size := range []int{len(body), 10} {
		s := feed("application/json; charset=utf-8", []byte(body), size)
		if !s.Complete || s.StopReason != "end_turn" || strings.Join(s.Blocks, ",") != "text" {
			t.Errorf("size %d: summary = %+v", size, s)
		}
		if s.Usage.Input != 17 || s.Usage.Output != 3 || val(s.Usage.CacheRead) != 0 || s.Usage.CacheWrite != nil {
			t.Errorf("size %d: usage = %+v", size, s.Usage)
		}
	}
}

func TestObserveJSONError(t *testing.T) {
	s := feed("application/json", []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`), 512)
	if s.Error == nil || s.Error.Type != "rate_limit_error" || !s.Complete {
		t.Errorf("summary = %+v", s)
	}
}

// An empty content array with tokens spent is the "successful but empty"
// local response from docs/research/lmstudio.md. The summary must make it
// visible: no blocks, output above zero.
func TestObserveEmptySuccess(t *testing.T) {
	s := feed("application/json", []byte(`{"type":"message","content":[],"stop_reason":"end_turn","usage":{"input_tokens":65,"output_tokens":14}}`), 512)
	if len(s.Blocks) != 0 || s.Usage.Output != 14 || s.StopReason != "end_turn" || s.Error != nil {
		t.Errorf("summary = %+v", s)
	}
}

func TestObserveNonJSONBody(t *testing.T) {
	s := feed("text/html", []byte("<html>502 Bad Gateway</html>"), 512)
	if s.Complete || s.Malformed != 1 {
		t.Errorf("summary = %+v, want incomplete with one malformed body", s)
	}
	if s := NewObserver("application/json").Summary(); s.Complete || s.Malformed != 0 {
		t.Errorf("empty body summary = %+v", s)
	}
}

func TestObserverStopsBufferingPastTheLimit(t *testing.T) {
	o := NewObserver("application/json")
	chunk := make([]byte, 1<<20)
	for i := 0; i < 20; i++ {
		if n, err := o.Write(chunk); n != len(chunk) || err != nil {
			t.Fatalf("Write = %d, %v; the observer must always accept everything", n, err)
		}
	}
	if len(o.buf) != 0 {
		t.Errorf("observer is holding %d bytes after passing its limit", len(o.buf))
	}
	if s := o.Summary(); s.Complete {
		t.Error("an oversized body is reported complete")
	}
}

// See TestRealCaptures in request_test.go for what JANUS_CAPTURES is.
func TestRealCapturedResponses(t *testing.T) {
	dir := os.Getenv("JANUS_CAPTURES")
	if dir == "" {
		t.Skip("JANUS_CAPTURES is not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*v1-messages.response.txt"))
	if len(files) == 0 {
		t.Fatalf("no *v1-messages.response.txt files under %s/*", dir)
	}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		whole := feed("text/event-stream", body, len(body))
		bytewise := feed("text/event-stream", body, 1)

		name := filepath.Base(f)
		if !whole.Complete || whole.Malformed != 0 || whole.Error != nil {
			t.Errorf("%s: complete=%v malformed=%d error=%v", name, whole.Complete, whole.Malformed, whole.Error)
		}
		if whole.Model == "" || whole.StopReason == "" || len(whole.Blocks) == 0 || whole.Usage.Output == 0 {
			t.Errorf("%s: model=%q stop=%q blocks=%v output=%d", name, whole.Model, whole.StopReason, whole.Blocks, whole.Usage.Output)
		}
		if whole.Usage.CacheRead == nil || whole.Usage.CacheWrite == nil || whole.Usage.CacheWrite1h == nil {
			t.Errorf("%s: cache usage missing: %+v", name, whole.Usage)
		}
		if whole.StopReason != bytewise.StopReason || whole.Usage.Output != bytewise.Usage.Output ||
			strings.Join(whole.Blocks, ",") != strings.Join(bytewise.Blocks, ",") {
			t.Errorf("%s: reading byte by byte gave a different summary", name)
		}
	}
	t.Logf("checked %d real responses", len(files))
}

func TestObserveToolNamesInJSONBody(t *testing.T) {
	s := feed("application/json", []byte(`{"type":"message","content":[{"type":"text","text":"x"},{"type":"tool_use","name":"Bash","id":"1","input":{}},{"type":"tool_use","name":"Read","id":"2","input":{}}],"usage":{"input_tokens":1,"output_tokens":1}}`), 512)
	if got := strings.Join(s.Tools, ","); got != "Bash,Read" {
		t.Errorf("tools = %q, want Bash,Read", got)
	}
}
