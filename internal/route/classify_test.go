package route

import (
	"net/http"
	"testing"

	"github.com/risadams/janus/internal/anthropic"
)

func TestClassify(t *testing.T) {
	h := func(pairs ...string) http.Header {
		header := http.Header{}
		for i := 0; i < len(pairs); i += 2 {
			header.Set(pairs[i], pairs[i+1])
		}
		return header
	}
	session := []string{"X-Claude-Code-Session-Id", "s1"}
	agent := append([]string{"X-Claude-Code-Agent-Id", "a1"}, session...)

	cases := []struct {
		method, path string
		header       http.Header
		want         Class
	}{
		{"POST", "/v1/messages", h(session...), Main},
		{"POST", "/v1/messages", h(agent...), Subagent},
		{"POST", "/v1/messages", h(), Unknown}, // not Claude Code
		{"POST", "/v1/messages/count_tokens", h(session...), TokenCount},
		{"POST", "/v1/messages/count_tokens", h(), TokenCount},
		{"GET", "/v1/messages/count_tokens", h(session...), Unknown},
		{"GET", "/v1/messages", h(session...), Unknown},
		{"HEAD", "/api/hello", h(), Connectivity},
		{"GET", "/", h(), Connectivity},
		{"POST", "/", h(), Unknown},
		{"GET", "/v1/models", h(session...), Unknown},
	}
	for _, c := range cases {
		if got := Classify(c.method, c.path, c.header); got != c.want {
			t.Errorf("Classify(%s %s, %v) = %s, want %s", c.method, c.path, c.header, got, c.want)
		}
	}
}

func TestClassifyRequestFindsTheQuotaCheck(t *testing.T) {
	main := http.Header{"X-Claude-Code-Session-Id": {"s1"}}
	sub := http.Header{"X-Claude-Code-Session-Id": {"s1"}, "X-Claude-Code-Agent-Id": {"a1"}}
	// As Claude Code sends it at start-up, metadata aside.
	quota := `{"model":"claude-opus-5-5","max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`

	cases := []struct {
		name   string
		header http.Header
		body   string
		want   Class
	}{
		{"the quota check", main, quota, Quota},
		{"as a block", main, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text","text":"quota"}]}]}`, Quota},
		{"more output", main, `{"model":"m","max_tokens":2,"messages":[{"role":"user","content":"quota"}]}`, Main},
		{"other text", main, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hello"}]}`, Main},
		{"with tools", main, `{"model":"m","max_tokens":1,"tools":[{"name":"t","input_schema":{}}],"messages":[{"role":"user","content":"quota"}]}`, Main},
		{"a later turn", main, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"quota"},{"role":"assistant","content":"x"},{"role":"user","content":"quota"}]}`, Main},
		{"from a subagent", sub, quota, Subagent},
		{"not Claude Code", http.Header{}, quota, Unknown},
	}
	for _, c := range cases {
		req, err := anthropic.ParseRequest([]byte(c.body))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := ClassifyRequest(c.header, req); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestClassifyRequestFindsCompaction(t *testing.T) {
	main := http.Header{"X-Claude-Code-Session-Id": {"s1"}}
	sub := http.Header{"X-Claude-Code-Session-Id": {"s1"}, "X-Claude-Code-Agent-Id": {"a1"}}
	ask := `CRITICAL: Respond with TEXT ONLY.\n\nYour task is to create a detailed summary of the conversation so far, paying close attention to the user's explicit requests.`
	// As Claude Code 2.1.296 sends it: the main loop's conversation, with
	// the instruction added after the tool result that ended the last turn.
	compact := `{"model":"m","max_tokens":128000,"messages":[` +
		`{"role":"user","content":"fix the test"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"},{"type":"text","text":"` + ask + `"}]}]}`

	cases := []struct {
		name   string
		header http.Header
		body   string
		want   Class
	}{
		{"compaction", main, compact, Compaction},
		{"as the whole message", main, `{"model":"m","max_tokens":1000,"messages":[{"role":"user","content":"` + ask + `"}]}`, Compaction},
		{"quoted earlier, then a real turn", main, `{"model":"m","max_tokens":1000,"messages":[{"role":"user","content":"` + ask + `"},{"role":"assistant","content":"x"},{"role":"user","content":"thanks"}]}`, Main},
		{"in a tool result", main, `{"model":"m","max_tokens":1000,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"` + ask + `"}]}]}`, Main},
		{"from a subagent", sub, compact, Subagent},
		{"not Claude Code", http.Header{}, compact, Unknown},
	}
	for _, c := range cases {
		req, err := anthropic.ParseRequest([]byte(c.body))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := ClassifyRequest(c.header, req); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}
