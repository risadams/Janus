package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"testing"

	"github.com/risadams/janus/internal/anthropic"
)

// Seeds for FuzzTranslateRequest: hand-built, in the shapes Claude Code
// sends. Real captures are never used; they hold prompts and credentials.
var translateSeeds = []string{
	claudeCodeRequest,
	`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
	`{"model":"m","tools":[{"name":"mcp__a-b__c","input_schema":{"type":"object"},"defer_loading":true}],"tool_choice":{"type":"tool","name":"mcp__a-b__c"},` +
		`"messages":[{"role":"user","content":[{"type":"tool_addition","tool":{"definition":{"name":"Skill"}}},{"type":"text","text":"go","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"c2ln"},{"type":"tool_use","id":"t1","name":"mcp__a-b__c","input":{"q":1}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":[{"type":"tool_reference","tool_name":"mcp__a-b__c"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA"}}]}]},` +
		`{"role":"system","content":"note"}]}`,
	`{"model":"m","messages":[{"role":"user","content":[{"type":"document","source":{"type":"content","content":[{"type":"text","text":"x"}]}}]}],"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`,
}

// Whatever Claude Code's request holds, the translation either refuses it
// with ErrUntranslatable or gives JSON that is the same every time, with
// store false and no previous_response_id, and only tool names OpenAI is
// documented to accept, each of which decodes back.
func FuzzTranslateRequest(f *testing.F) {
	for _, s := range translateSeeds {
		f.Add([]byte(s))
	}
	valid := regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
	f.Fuzz(func(t *testing.T, data []byte) {
		req, err := anthropic.ParseRequest(data)
		if err != nil {
			return
		}
		tr, err := TranslateRequest(req, RequestOptions{Model: "gpt-6-luna", SessionID: "s"})
		if err != nil {
			if !errors.Is(err, ErrUntranslatable) {
				t.Fatalf("refused without ErrUntranslatable: %v", err)
			}
			return
		}
		again, err := TranslateRequest(req, RequestOptions{Model: "gpt-6-luna", SessionID: "s"})
		if err != nil || !bytes.Equal(tr.Body, again.Body) {
			t.Fatalf("the same request translated differently")
		}
		var b struct {
			Store    *bool           `json:"store"`
			Previous json.RawMessage `json:"previous_response_id"`
			Tools    []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(tr.Body, &b); err != nil {
			t.Fatalf("the translation is not JSON: %v", err)
		}
		if b.Store == nil || *b.Store || b.Previous != nil {
			t.Fatalf("store or previous_response_id is wrong: %s", tr.Body)
		}
		for _, tool := range b.Tools {
			if !valid.MatchString(tool.Name) {
				t.Fatalf("tool name %q is not one OpenAI is documented to accept", tool.Name)
			}
			if _, ok := tr.Tools.Claude(tool.Name); !ok {
				t.Fatalf("tool name %q does not decode", tool.Name)
			}
		}
	})
}

// streamSeeds are Responses streams built by hand from OpenAI's reference.
var streamSeeds = []string{
	"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"reasoning\"}}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":0,\"delta\":\"hm\",\"obfuscation\":\"x\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"gA\"}}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"Bash\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":1,\"delta\":\"{\\\"a\\\":1}\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":1}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n",
	"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\"}}\n\ndata: {\"type\":\"response.refusal.delta\",\"output_index\":0,\"delta\":\"no\"}\n\n" +
		"data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n",
	"data: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow\"}\n\n",
	"data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"bio_policy\",\"message\":\"no\"}}}\n\n",
	"data: {\"type\":\"response.output_text.delta\",\"output_index\":3,\"delta\":\"orphan\"}\n\ndata: [DONE]\n\n",
}

// Whatever OpenAI's stream holds, the translation writes only events
// janus's own reader takes as a clean Anthropic stream, never panics, and
// ends the stream properly only when the response ended: a stream cut
// short stays cut short.
func FuzzTranslateStream(f *testing.F) {
	for _, s := range streamSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var out bytes.Buffer
		res, err := TranslateStream(&out, bytes.NewReader(data), ResponseOptions{Model: "m", Tools: ToolNames{"Bash": "Bash"}, Summary: true})
		o := anthropic.NewObserver("text/event-stream")
		_, _ = o.Write(out.Bytes())
		s := o.Summary()
		if s.Malformed != 0 {
			t.Fatalf("wrote %d events janus cannot read:\n%s", s.Malformed, out.Bytes())
		}
		ended := bytes.Contains(out.Bytes(), []byte("event: message_stop\n"))
		switch {
		case errors.Is(err, ErrBrokenStream):
			if ended {
				t.Fatalf("a broken stream was ended properly")
			}
		case err != nil:
			t.Fatalf("unexpected error: %v", err)
		case res.Err != nil:
			if ended || s.Error == nil {
				t.Fatalf("a failed response did not end with an error event")
			}
		default:
			if !ended || !s.Complete {
				t.Fatalf("a finished response did not end with message_stop")
			}
		}
	})
}
