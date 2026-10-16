package route

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// TestRealCapturesAreClassified classifies every captured request, from the
// method, path, and headers its capture recorded. No request Claude Code
// sent may come out unknown. Opt-in, like the other tests on real traffic:
//
//	JANUS_CAPTURES=~/logs/captures go test ./internal/route -run RealCapturesAreClassified -v
func TestRealCapturesAreClassified(t *testing.T) {
	dir := os.Getenv("JANUS_CAPTURES")
	if dir == "" {
		t.Skip("JANUS_CAPTURES is not set")
	}
	pages, _ := filepath.Glob(filepath.Join(dir, "*", "*.md"))
	counts := map[Class]int{}
	untested := map[string]bool{}
	for _, page := range pages {
		if strings.Contains(page, ".shadow.") {
			continue
		}
		data, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		method, path, header := capturedRequest(string(data))
		if method == "" {
			continue
		}
		if v := ClaudeCodeVersion(header); v != "" && newerThanTested(v) && !untested[v] {
			untested[v] = true
			t.Errorf("%s is from Claude Code %s, newer than TestedClaudeCode.Newest (%s); once this test passes on it, raise Newest", filepath.Base(page), v, TestedClaudeCode.Newest)
		}
		class := Classify(method, path, header)
		if method == http.MethodPost && path == "/v1/messages" {
			body, err := os.ReadFile(strings.TrimSuffix(page, ".md") + ".request.txt")
			if err != nil {
				continue
			}
			req, err := anthropic.ParseRequest(body)
			if err != nil {
				t.Errorf("%s: %v", filepath.Base(page), err)
				continue
			}
			class = ClassifyRequest(header, req)
		}
		counts[class]++
		if class == Unknown && header.Get("X-Claude-Code-Session-Id") != "" {
			t.Errorf("%s: %s %s from Claude Code was classified unknown", filepath.Base(page), method, path)
		}
	}
	t.Logf("%d captures: %v", len(pages), counts)
	if counts[Main] == 0 {
		t.Error("no main-loop turn among the captures, so the test proves little")
	}
}

// capturedRequest reads the request line and headers from a capture's page.
func capturedRequest(page string) (method, path string, header http.Header) {
	header = http.Header{}
	first, rest, _ := strings.Cut(page, "\n")
	if fields := strings.Fields(strings.TrimPrefix(first, "# ")); len(fields) == 2 {
		method, path = fields[0], fields[1]
	}
	_, block, _ := strings.Cut(rest, "<headers>")
	block, _, _ = strings.Cut(block, "</headers>")
	for _, line := range strings.Split(block, "\n") {
		if k, v, ok := strings.Cut(line, ": "); ok {
			header.Add(k, v)
		}
	}
	return method, path, header
}

func TestClassifyRequestFindsStatusSummaries(t *testing.T) {
	main := http.Header{"X-Claude-Code-Session-Id": {"s1"}}
	sub := http.Header{"X-Claude-Code-Session-Id": {"s1"}, "X-Claude-Code-Agent-Id": {"a1"}}
	ask := `Describe your most recent action in 3-5 words using present tense (-ing). Name the file or function, not the branch. Do not use tools.`
	// As Claude Code 2.1.296 sends it for a subagent: the conversation so
	// far, the instruction as the last text of the last user message, and
	// sometimes a system message after it.
	withSystem := `{"model":"m","max_tokens":128000,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"find the env vars"},{"type":"text","text":"` + ask + `"}]},` +
		`{"role":"system","content":[{"type":"text","text":"tools changed"}]}]}`
	later := `{"model":"m","max_tokens":128000,"messages":[` +
		`{"role":"user","content":"find the env vars"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Grep","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"config.go:311"},{"type":"text","text":"` + ask + `"}]}]}`

	cases := []struct {
		name   string
		header http.Header
		body   string
		want   Class
	}{
		{"subagent, system message after", sub, withSystem, Status},
		{"subagent, after a tool result", sub, later, Status},
		{"main loop", main, later, Status},
		{"quoted in a tool result", sub, `{"model":"m","max_tokens":1000,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"` + ask + `"}]}]}`, Subagent},
		{"quoted mid-sentence", main, `{"model":"m","max_tokens":1000,"messages":[{"role":"user","content":"Claude Code asks: ` + ask + `"}]}`, Main},
		{"in an earlier user message", sub, `{"model":"m","max_tokens":1000,"messages":[{"role":"user","content":"` + ask + `"},{"role":"assistant","content":"Reading config.go"},{"role":"user","content":"thanks"}]}`, Subagent},
		{"not Claude Code", http.Header{}, later, Unknown},
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

func TestClassifyRequestFindsSafetyChecks(t *testing.T) {
	main := http.Header{"X-Claude-Code-Session-Id": {"s1"}}
	sub := http.Header{"X-Claude-Code-Session-Id": {"s1"}, "X-Claude-Code-Agent-Id": {"a1"}}
	role := "You are a security monitor for autonomous AI coding agents.\\n\\n## Context"
	// As Claude Code 2.1.296 sends it: a billing block, then the
	// classifier's role, no tools, a short answer cut at </severity>.
	check := `{"model":"claude-sonnet-5","max_tokens":64,"stop_sequences":["</severity>"],"system":[` +
		`{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.296.b99;"},{"type":"text","text":"` + role + `"}],` +
		`"messages":[{"role":"user","content":"<transcript>...</transcript> Respond with <severity>N</severity> ONLY."}]}`
	quoted := `{"model":"m","max_tokens":1000,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"` + role + `"}]}]}`
	cases := []struct {
		name   string
		header http.Header
		body   string
		want   Class
	}{
		{"from the main loop", main, check, Safety},
		{"from a subagent", sub, check, Safety},
		{"quoted in a tool result", main, quoted, Main},
		{"another system prompt", main, `{"model":"m","max_tokens":64,"system":"You are a helpful assistant","messages":[{"role":"user","content":"hi"}]}`, Main},
		{"not Claude Code", http.Header{}, check, Unknown},
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
