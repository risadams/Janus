package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/risadams/janus/internal/anthropic"
)

// translate parses a Messages request, translates it, and returns the
// Responses body both raw and decoded.
func translate(t *testing.T, request string, opts RequestOptions) (*Translated, map[string]any) {
	t.Helper()
	req, err := anthropic.ParseRequest([]byte(request))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if opts.Model == "" {
		opts.Model = "gpt-6.1-sol"
	}
	tr, err := TranslateRequest(req, opts)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(tr.Body, &got); err != nil {
		t.Fatalf("the translation is not JSON: %v\n%s", err, tr.Body)
	}
	return tr, got
}

func input(t *testing.T, got map[string]any) []map[string]any {
	t.Helper()
	raw, _ := got["input"].([]any)
	items := make([]map[string]any, len(raw))
	for i, r := range raw {
		items[i] = r.(map[string]any)
	}
	return items
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// claudeCodeRequest is a first turn in the shape Claude Code sends: the
// billing header, two marked system blocks, a tool, and a user message
// carrying the moving mark.
const claudeCodeRequest = `{
  "model": "claude-sonnet-5-5",
  "max_tokens": 128000,
  "stream": true,
  "system": [
    {"type": "text", "text": "x-anthropic-billing-header: cc_version=3.1.0; cc_entrypoint=cli"},
    {"type": "text", "text": "You are Claude Code.", "cache_control": {"type": "ephemeral", "ttl": "1h"}},
    {"type": "text", "text": "Environment: darwin", "cache_control": {"type": "ephemeral", "ttl": "1h"}}
  ],
  "tools": [
    {"name": "Bash", "description": "Run a command.", "input_schema": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]}}
  ],
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "List the files.", "cache_control": {"type": "ephemeral"}}]}
  ],
  "metadata": {"user_id": "user_abc_account_def_session_ghi"},
  "context_management": {"edits": [{"type": "clear_thinking_20251015"}]},
  "thinking": {"type": "adaptive", "display": "omitted"}
}`

func TestEveryRequestIsStatelessAndUntruncated(t *testing.T) {
	tr, got := translate(t, claudeCodeRequest, RequestOptions{})
	if got["store"] != false {
		t.Errorf("store = %v, want false: OpenAI would keep the response for 30 days", got["store"])
	}
	if _, ok := got["previous_response_id"]; ok {
		t.Error("previous_response_id is set; every request carries its whole history")
	}
	if got["truncation"] != "disabled" {
		t.Errorf("truncation = %v, want disabled: auto drops the start of the conversation", got["truncation"])
	}
	if jsonOf(got["include"]) != `["reasoning.encrypted_content"]` {
		t.Errorf("include = %v", got["include"])
	}
	if got["stream"] != true || got["max_output_tokens"] != float64(MaxOutputTokens) || got["model"] != "gpt-6.1-sol" {
		t.Errorf("stream, max_output_tokens, model = %v, %v, %v", got["stream"], got["max_output_tokens"], got["model"])
	}
	for _, leak := range []string{"user_abc", "clear_thinking", "x-anthropic-billing-header", "claude-sonnet"} {
		if bytes.Contains(tr.Body, []byte(leak)) {
			t.Errorf("the translation carries %q, which is Anthropic's alone", leak)
		}
	}
}

func TestSystemBlocksBecomeALeadingDeveloperMessage(t *testing.T) {
	_, got := translate(t, claudeCodeRequest, RequestOptions{})
	first := input(t, got)[0]
	want := `{"content":[{"prompt_cache_breakpoint":{"mode":"explicit"},"text":"You are Claude Code.","type":"input_text"},{"prompt_cache_breakpoint":{"mode":"explicit"},"text":"Environment: darwin","type":"input_text"}],"role":"developer","type":"message"}`
	if jsonOf(first) != want {
		t.Errorf("input[0] =\n%s\nwant\n%s", jsonOf(first), want)
	}
	if _, ok := got["instructions"]; ok {
		t.Error("instructions is set; a breakpoint cannot go there")
	}
}

func TestCacheMarksBecomeBreakpointsWhereClaudeCodePutThem(t *testing.T) {
	tr, got := translate(t, claudeCodeRequest, RequestOptions{})
	if jsonOf(got["prompt_cache_options"]) != `{"mode":"explicit"}` {
		t.Errorf("prompt_cache_options = %v", got["prompt_cache_options"])
	}
	if n := bytes.Count(tr.Body, []byte("prompt_cache_breakpoint")); n != 3 {
		t.Errorf("%d breakpoints, want Claude Code's 3", n)
	}
	last := input(t, got)[1]
	if jsonOf(last["content"]) != `[{"prompt_cache_breakpoint":{"mode":"explicit"},"text":"List the files.","type":"input_text"}]` {
		t.Errorf("the user message is %s", jsonOf(last))
	}
}

// A mark on a block that becomes nothing able to carry a breakpoint moves
// to the nearest earlier part that can.
func TestAMarkOnABlockThatCannotCarryOneMovesBack(t *testing.T) {
	for _, c := range []struct{ name, assistant string }{
		{"thinking", `{"type": "thinking", "thinking": "", "signature": "sig", "cache_control": {"type": "ephemeral"}}`},
		{"assistant text", `{"type": "text", "text": "Done.", "cache_control": {"type": "ephemeral"}}`},
		{"tool call", `{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": {}, "cache_control": {"type": "ephemeral"}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, got := translate(t, `{"model":"m","messages":[
				{"role":"user","content":"First."},
				{"role":"assistant","content":[`+c.assistant+`]}]}`, RequestOptions{})
			items := input(t, got)
			if jsonOf(items[0]) != `{"content":[{"prompt_cache_breakpoint":{"mode":"explicit"},"text":"First.","type":"input_text"}],"role":"user","type":"message"}` {
				t.Errorf("the mark did not move to the user's text: %s", jsonOf(items))
			}
		})
	}
}

// Claude Code's turn N+1 repeats turn N, moves the tail mark to the new
// last message, and adds to the end. Everything before that must translate
// to the same bytes, or OpenAI's cache misses from there on.
func TestLaterTurnsKeepThePrefix(t *testing.T) {
	history := []string{
		`{"role":"user","content":[{"type":"text","text":"List the files."}]}`,
		`{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"EqQB"},{"type":"text","text":"Listing."},{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"command":"ls"}}]}`,
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"a.go\nb.go"},{"type":"tool_addition","tool":{"definition":{"name":"Skill","input_schema":{"type":"object"}}}},{"type":"text","text":"<system-reminder>ok</system-reminder>"}]}`,
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_02","name":"mcp__claude-in-chrome__read_page","input":{"tab":1}}]}`,
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_02","content":[{"type":"text","text":"page"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}]}]}`,
		`{"role":"system","content":"Remember the rules."}`,
		`{"role":"user","content":"Thanks."}`,
	}
	turn := func(n int) []byte {
		messages := append([]string(nil), history[:n]...)
		// The moving mark sits on the last message's last block.
		last := messages[n-1]
		if strings.HasSuffix(last, `}]}`) {
			last = strings.TrimSuffix(last, `}]}`) + `,"cache_control":{"type":"ephemeral"}}]}`
		}
		messages[n-1] = last
		req := `{"model":"m","stream":true,"system":[{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],
			"tools":[{"name":"Bash","input_schema":{"type":"object"}},{"name":"mcp__claude-in-chrome__read_page","input_schema":{"type":"object"}}],
			"messages":[` + strings.Join(messages, ",") + `]}`
		tr, _ := translate(t, req, RequestOptions{SessionID: "3f0c"})
		return tr.Body
	}
	unmarked := regexp.MustCompile(`,"prompt_cache_breakpoint":\{"mode":"explicit"\}`)
	items := func(body []byte) (tools string, input []json.RawMessage) {
		var b struct {
			Tools json.RawMessage   `json:"tools"`
			Input []json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(unmarked.ReplaceAll(body, nil), &b); err != nil {
			t.Fatal(err)
		}
		return string(b.Tools), b.Input
	}
	for n := 1; n < len(history); n++ {
		toolsN, inputN := items(turn(n))
		toolsNext, inputNext := items(turn(n + 1))
		if toolsN != toolsNext {
			t.Errorf("turn %d changed tools", n+1)
		}
		if len(inputNext) < len(inputN) {
			t.Fatalf("turn %d has fewer items than turn %d", n+1, n)
		}
		for i := range inputN {
			if !bytes.Equal(inputN[i], inputNext[i]) {
				t.Errorf("turn %d changed input[%d]:\n%s\nto\n%s", n+1, i, inputN[i], inputNext[i])
			}
		}
	}
	if !bytes.Equal(turn(len(history)), turn(len(history))) {
		t.Error("the same request translated to different bytes")
	}
}

func TestToolNamesOpenAIWouldRefuseAreEncodedAndDecodedFromTheRequest(t *testing.T) {
	long := "mcp__" + strings.Repeat("server", 10) + "__tool"
	tr, got := translate(t, `{"model":"m","tools":[
		{"name":"Read","input_schema":{"type":"object"}},
		{"name":"mcp__claude-in-chrome__computer","input_schema":{"type":"object"}},
		{"name":"mcp__claude_in_chrome__computer","input_schema":{"type":"object"}},
		{"name":"`+long+`","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"hi"}]}`, RequestOptions{})
	var names []string
	for _, tool := range got["tools"].([]any) {
		f := tool.(map[string]any)
		names = append(names, f["name"].(string))
		if f["strict"] != false || f["type"] != "function" {
			t.Errorf("tool %v: strict and type are %v, %v", f["name"], f["strict"], f["type"])
		}
	}
	valid := regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
	for _, n := range names {
		if !valid.MatchString(n) {
			t.Errorf("%q is not a name OpenAI is documented to accept", n)
		}
	}
	if names[0] != "Read" || names[2] != "mcp__claude_in_chrome__computer" {
		t.Errorf("a name that was fine changed: %v", names)
	}
	if names[1] == names[2] {
		t.Error("two tools that clean up alike got the same name")
	}
	for i, want := range []string{"Read", "mcp__claude-in-chrome__computer", "mcp__claude_in_chrome__computer", long} {
		if back, ok := tr.Tools.Claude(names[i]); !ok || back != want {
			t.Errorf("%q decodes to %q, %v; want %q", names[i], back, ok, want)
		}
	}
}

func TestToolCallsKeepTheirIdsAndResultsComeFirst(t *testing.T) {
	_, got := translate(t, `{"model":"m","messages":[
		{"role":"user","content":"Run it."},
		{"role":"assistant","content":[{"type":"text","text":"Running two."},
			{"type":"tool_use","id":"toolu_01A","name":"Bash","input":{"command": "ls"}},
			{"type":"tool_use","id":"toolu_01B","name":"Bash","input":{}}]},
		{"role":"user","content":[{"type":"text","text":"Beside."},
			{"type":"tool_result","tool_use_id":"toolu_01A","content":"ok"},
			{"type":"tool_result","tool_use_id":"toolu_01B","content":"no such file","is_error":true}]}]}`, RequestOptions{})
	var shapes []string
	for _, it := range input(t, got) {
		shapes = append(shapes, jsonOf(it))
	}
	want := []string{
		`{"content":[{"text":"Run it.","type":"input_text"}],"role":"user","type":"message"}`,
		`{"content":[{"text":"Running two.","type":"output_text"}],"role":"assistant","type":"message"}`,
		`{"arguments":"{\"command\":\"ls\"}","call_id":"toolu_01A","name":"Bash","type":"function_call"}`,
		`{"arguments":"{}","call_id":"toolu_01B","name":"Bash","type":"function_call"}`,
		`{"call_id":"toolu_01A","output":[{"text":"ok","type":"input_text"}],"type":"function_call_output"}`,
		`{"call_id":"toolu_01B","output":[{"text":"Error: no such file","type":"input_text"}],"type":"function_call_output"}`,
		`{"content":[{"text":"Beside.","type":"input_text"}],"role":"user","type":"message"}`,
	}
	if strings.Join(shapes, "\n") != strings.Join(want, "\n") {
		t.Errorf("input =\n%s\nwant\n%s", strings.Join(shapes, "\n"), strings.Join(want, "\n"))
	}
}

// Tools added mid-conversation stay where they were added, and tools a
// search loads come in straight after its result: lifting either into
// tools would change the start of every later request.
func TestToolsAddedLaterStayInPlace(t *testing.T) {
	_, got := translate(t, `{"model":"m",
		"tools":[{"name":"Bash","input_schema":{"type":"object"}},
			{"name":"WebFetch","description":"Fetch.","input_schema":{"type":"object"},"defer_loading":true}],
		"messages":[
		{"role":"user","content":[{"type":"text","text":"Before."},
			{"type":"tool_addition","tool":{"definition":{"name":"Skill","input_schema":{"type":"object"}}}},
			{"type":"text","text":"After."}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"ToolSearch","input":{"query":"fetch"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[
			{"type":"tool_reference","tool_name":"WebFetch"},{"type":"tool_reference","tool_name":"Gone"}]}]}]}`, RequestOptions{})
	if jsonOf(got["tools"]) != `[{"name":"Bash","parameters":{"type":"object"},"strict":false,"type":"function"}]` {
		t.Errorf("tools = %s; the deferred tool should be left out", jsonOf(got["tools"]))
	}
	var shapes []string
	for _, it := range input(t, got) {
		shapes = append(shapes, jsonOf(it))
	}
	want := []string{
		`{"content":[{"text":"Before.","type":"input_text"}],"role":"user","type":"message"}`,
		`{"role":"developer","tools":[{"name":"Skill","parameters":{"type":"object"},"strict":false,"type":"function"}],"type":"additional_tools"}`,
		`{"content":[{"text":"After.","type":"input_text"}],"role":"user","type":"message"}`,
		`{"arguments":"{\"query\":\"fetch\"}","call_id":"toolu_1","name":"ToolSearch","type":"function_call"}`,
		`{"call_id":"toolu_1","output":[{"text":"Tool WebFetch is loaded and can be called.","type":"input_text"},{"text":"Tool Gone was found, but it cannot be called here.","type":"input_text"}],"type":"function_call_output"}`,
		`{"role":"developer","tools":[{"description":"Fetch.","name":"WebFetch","parameters":{"type":"object"},"strict":false,"type":"function"}],"type":"additional_tools"}`,
	}
	if strings.Join(shapes, "\n") != strings.Join(want, "\n") {
		t.Errorf("input =\n%s\nwant\n%s", strings.Join(shapes, "\n"), strings.Join(want, "\n"))
	}
}

func TestImagesAndDocumentsTranslate(t *testing.T) {
	_, got := translate(t, `{"model":"m","messages":[{"role":"user","content":[
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}},
		{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}},
		{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBER"},"title":"Spec"},
		{"type":"document","source":{"type":"text","media_type":"text/plain","data":"plain words"}}]}]}`, RequestOptions{})
	want := `[{"image_url":"data:image/png;base64,iVBOR","type":"input_image"},{"image_url":"https://example.com/a.png","type":"input_image"},{"file_data":"data:application/pdf;base64,JVBER","filename":"document.pdf","type":"input_file"},{"text":"plain words","type":"input_text"}]`
	if c := jsonOf(input(t, got)[0]["content"]); c != want {
		t.Errorf("content =\n%s\nwant\n%s", c, want)
	}
}

// A request OpenAI cannot be given skips it, rather than going with a
// part missing.
func TestUntranslatableRequestsSkipOpenAI(t *testing.T) {
	for name, request := range map[string]string{
		"Files API image": `{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"file","file_id":"file_01"}}]}]}`,
		"server tool":     `{"model":"m","tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"hi"}]}`,
		"server result":   `{"model":"m","messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}]}]}`,
		"unknown block":   `{"model":"m","messages":[{"role":"user","content":[{"type":"hologram"}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			req, err := anthropic.ParseRequest([]byte(request))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := TranslateRequest(req, RequestOptions{Model: "m"}); !errors.Is(err, ErrUntranslatable) {
				t.Errorf("err = %v, want ErrUntranslatable", err)
			}
		})
	}
}

func TestToolChoiceAndParallelCalls(t *testing.T) {
	for choice, want := range map[string]string{
		`{"type":"auto"}`:                      `"auto"`,
		`{"type":"any"}`:                       `"required"`,
		`{"type":"none"}`:                      `"none"`,
		`{"type":"tool","name":"mcp__a-b__c"}`: `{"name":"` + encodeToolName("mcp__a-b__c") + `","type":"function"}`,
	} {
		_, got := translate(t, `{"model":"m","tool_choice":`+choice+`,"messages":[]}`, RequestOptions{})
		if jsonOf(got["tool_choice"]) != want {
			t.Errorf("tool_choice %s became %s, want %s", choice, jsonOf(got["tool_choice"]), want)
		}
		if _, ok := got["parallel_tool_calls"]; ok {
			t.Errorf("parallel_tool_calls set for %s", choice)
		}
	}
	_, got := translate(t, `{"model":"m","tool_choice":{"type":"auto","disable_parallel_tool_use":true},"messages":[]}`, RequestOptions{})
	if got["parallel_tool_calls"] != false {
		t.Errorf("parallel_tool_calls = %v, want false", got["parallel_tool_calls"])
	}
}

func TestWhatOpenAICannotHonourIsNamed(t *testing.T) {
	tr, got := translate(t, `{"model":"m","stop_sequences":["END"],"temperature":0.2,"max_tokens":200000,"messages":[]}`, RequestOptions{})
	if strings.Join(tr.Dropped, ",") != "stop_sequences,temperature" {
		t.Errorf("Dropped = %v", tr.Dropped)
	}
	if got["max_output_tokens"] != float64(MaxOutputTokens) {
		t.Errorf("max_output_tokens = %v, want the cap", got["max_output_tokens"])
	}
}

func TestCacheKeyIsAHashOfTheSessionNotTheSession(t *testing.T) {
	const session = "9a1d5e2c-0b8f-4c47-9d1e-6f3a2b7c8d90"
	tr, got := translate(t, claudeCodeRequest, RequestOptions{SessionID: session})
	key, _ := got["prompt_cache_key"].(string)
	if len(key) != 32 || bytes.Contains(tr.Body, []byte(session)) {
		t.Errorf("prompt_cache_key = %q; want 32 hex characters, not the session id", key)
	}
	_, again := translate(t, claudeCodeRequest, RequestOptions{SessionID: session})
	if again["prompt_cache_key"] != key {
		t.Error("the same session got a different key")
	}
	_, none := translate(t, claudeCodeRequest, RequestOptions{})
	if _, ok := none["prompt_cache_key"]; ok {
		t.Error("a key was sent with no session")
	}
}

func TestSummarizedThinkingAsksForASummaryAndEffortIsPassedOn(t *testing.T) {
	_, got := translate(t, `{"model":"m","thinking":{"type":"adaptive","display":"summarized"},"messages":[]}`, RequestOptions{Effort: "high"})
	if jsonOf(got["reasoning"]) != `{"effort":"high","summary":"auto"}` {
		t.Errorf("reasoning = %s", jsonOf(got["reasoning"]))
	}
	_, got = translate(t, claudeCodeRequest, RequestOptions{})
	if _, ok := got["reasoning"]; ok {
		t.Errorf("reasoning = %s with no effort and thinking omitted", jsonOf(got["reasoning"]))
	}
}
