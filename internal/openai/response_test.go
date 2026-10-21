package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/openai/openaitest"
)

// ask sends a request to the fake and returns its answer.
func ask(t *testing.T, srv *openaitest.Server, stream bool) *http.Response {
	t.Helper()
	body := `{"model":"gpt-6-luna","input":[],"stream":false,"store":false}`
	if stream {
		body = `{"model":"gpt-6-luna","input":[],"stream":true,"store":false}`
	}
	req, err := NewRequest(context.Background(), loadTestKey(t), srv.URL, "/v1/responses", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// streamed translates the fake's streamed answer to reply.
func streamed(t *testing.T, reply openaitest.Reply, opts ResponseOptions, omitEventLines bool) (string, Result, error) {
	t.Helper()
	srv := openaitest.New(t, reply)
	srv.OmitEventLines = omitEventLines
	resp := ask(t, srv, true)
	var out bytes.Buffer
	res, err := TranslateStream(&out, resp.Body, opts)
	return out.String(), res, err
}

// observe reads a translated stream as janus reads Claude's.
func observe(t *testing.T, stream string) anthropic.Summary {
	t.Helper()
	o := anthropic.NewObserver("text/event-stream")
	_, _ = o.Write([]byte(stream))
	return o.Summary()
}

var mcpTool = "mcp__claude-in-chrome__computer"

func toolNames() ToolNames {
	n := ToolNames{}
	_ = n.add(mcpTool)
	_ = n.add("Bash")
	return n
}

func TestAStreamedAnswerReadsAsAnAnthropicStream(t *testing.T) {
	reply := openaitest.Reply{
		Output: []openaitest.Item{
			openaitest.Reasoning{Summary: "Thinking it over.", Encrypted: "gAAAA"},
			openaitest.Text{Text: "I'll take a screenshot."},
			openaitest.Call{CallID: "call_Ab12", Name: encodeToolName(mcpTool), Arguments: `{"action":"screenshot"}`},
		},
		Usage: openaitest.Usage{Input: 12000, Cached: 9000, Write30m: 2000, Output: 300, Reasoning: 120},
	}
	for _, omit := range []bool{false, true} {
		out, res, err := streamed(t, reply, ResponseOptions{Model: "claude-sonnet-5-5", Tools: toolNames()}, omit)
		if err != nil {
			t.Fatal(err)
		}
		s := observe(t, out)
		if !s.Complete || s.Malformed != 0 || s.Error != nil {
			t.Fatalf("event lines omitted %v: not a clean Anthropic stream (complete %v, malformed %d, error %v):\n%s", omit, s.Complete, s.Malformed, s.Error, out)
		}
		if strings.Join(s.Blocks, ",") != "thinking,text,tool_use" || strings.Join(s.Tools, ",") != mcpTool {
			t.Errorf("blocks %v, tools %v", s.Blocks, s.Tools)
		}
		if s.Model != "claude-sonnet-5-5" || s.StopReason != "tool_use" || res.StopReason != "tool_use" {
			t.Errorf("model %q, stop %q", s.Model, s.StopReason)
		}
		// Claude Code sees the model it asked for; the ledger is told the
		// one that answered.
		if res.Model != "gpt-6-luna" {
			t.Errorf("Result.Model = %q, want the model OpenAI reported", res.Model)
		}
		answer, ok := anthropic.Assemble([]byte(out), "text/event-stream")
		if !ok || answer.Blocks[1].Text != "I'll take a screenshot." || answer.Blocks[2].ID != "call_Ab12" || answer.Blocks[2].Input != `{"action":"screenshot"}` {
			t.Errorf("assembled %+v", answer)
		}
		if !strings.HasPrefix(out, "event: message_start\n") || !strings.HasSuffix(out, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n") {
			t.Errorf("the stream does not run from message_start to message_stop:\n%s", out)
		}
	}
}

// OpenAI's reasoning has no signature Claude would accept, so it never
// reaches Claude Code as signed thinking. Its summary shows only when
// Claude Code asked to see thinking.
func TestReasoningIsNeverSignedThinking(t *testing.T) {
	reply := openaitest.Answer(openaitest.Reasoning{Summary: "Weighing two options.", Encrypted: "gAAAAsecret"}, openaitest.Text{Text: "Done."})
	for _, summary := range []bool{false, true} {
		out, _, err := streamed(t, reply, ResponseOptions{Model: "m", Summary: summary}, false)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "gAAAA") {
			t.Error("the encrypted reasoning reached Claude Code")
		}
		if !strings.Contains(out, `"signature":""`) || strings.Contains(out, `"signature":"g`) {
			t.Errorf("the thinking block carries a signature:\n%s", out)
		}
		answer, _ := anthropic.Assemble([]byte(out), "text/event-stream")
		want := ""
		if summary {
			want = "Weighing two options."
		}
		if answer.Blocks[0].Type != "thinking" || answer.Blocks[0].Text != want {
			t.Errorf("summary %v: thinking is %q, want %q", summary, answer.Blocks[0].Text, want)
		}
	}
}

func TestANonStreamedAnswerTranslates(t *testing.T) {
	srv := openaitest.New(t, openaitest.Reply{
		Output: []openaitest.Item{openaitest.Reasoning{Encrypted: "gAAAA"}, openaitest.Text{Text: "Hello."}, openaitest.Call{CallID: "call_1", Name: "Bash", Arguments: `{"command": "ls"}`}},
		Usage:  openaitest.Usage{Input: 100, Cached: 40, Output: 10},
	})
	resp := ask(t, srv, false)
	body, _ := io.ReadAll(resp.Body)
	out, res, err := TranslateResponse(body, ResponseOptions{Model: "claude-haiku-5-5", Tools: toolNames()})
	if err != nil {
		t.Fatal(err)
	}
	answer, ok := anthropic.Assemble(out, "application/json")
	if !ok || answer.StopReason != "tool_use" || len(answer.Blocks) != 3 || answer.Blocks[1].Text != "Hello." || answer.Blocks[2].Input != `{"command":"ls"}` {
		t.Errorf("assembled %+v from\n%s", answer, out)
	}
	if strings.Contains(string(out), "gAAAA") {
		t.Error("the encrypted reasoning reached Claude Code")
	}
	var msg struct {
		Model string         `json:"model"`
		Usage anthropicUsage `json:"usage"`
	}
	_ = json.Unmarshal(out, &msg)
	if res.Model != "gpt-6-luna" {
		t.Errorf("Result.Model = %q, want the model OpenAI reported", res.Model)
	}
	if msg.Model != "claude-haiku-5-5" || msg.Usage.InputTokens != 60 || msg.Usage.CacheReadInputTokens != 40 || res.Usage.Input != 100 {
		t.Errorf("model %q, usage %+v, OpenAI's %+v", msg.Model, msg.Usage, res.Usage)
	}
}

// OpenAI counts cached and written tokens inside input_tokens; Anthropic
// does not. Claude Code judges how full its context is from the sum, so
// the four fields must add up to OpenAI's total.
func TestUsageAddsUpToOpenAIsInput(t *testing.T) {
	_, res, err := streamed(t, openaitest.Reply{
		Output: []openaitest.Item{openaitest.Text{Text: "ok"}},
		Usage:  openaitest.Usage{Input: 50000, Cached: 30000, Write30m: 15000, Output: 800, Reasoning: 500},
	}, ResponseOptions{Model: "m"}, false)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Usage.anthropic()
	if a.InputTokens+a.CacheReadInputTokens+a.CacheCreationInputTokens != 50000 {
		t.Errorf("Anthropic's fields add up to %d, want 50000: %+v", a.InputTokens+a.CacheReadInputTokens+a.CacheCreationInputTokens, a)
	}
	if a.CacheCreation.Ephemeral5m != 15000 || a.OutputTokens != 800 || res.Usage.Reasoning != 500 {
		t.Errorf("%+v, OpenAI's %+v", a, res.Usage)
	}
}

func TestStopReasons(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply openaitest.Reply
		want  string
	}{
		{"text", openaitest.Answer(openaitest.Text{Text: "ok"}), "end_turn"},
		{"out of tokens", openaitest.Reply{Output: []openaitest.Item{openaitest.Text{Text: "cut"}}, Status: "incomplete", Reason: "max_output_tokens"}, "max_tokens"},
		{"filtered", openaitest.Reply{Status: "incomplete", Reason: "content_filter"}, "refusal"},
		{"refusal", openaitest.Answer(openaitest.Refusal{Refusal: "I can't help with that."}), "refusal"},
		{"policy", openaitest.Reply{Status: "failed", Reason: "cyber_policy", Message: "This request was flagged."}, "refusal"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, res, err := streamed(t, c.reply, ResponseOptions{Model: "m"}, false)
			if err != nil {
				t.Fatal(err)
			}
			if s := observe(t, out); res.StopReason != c.want || s.StopReason != c.want || !s.Complete {
				t.Errorf("stop reason %q (stream says %q, complete %v), want %q", res.StopReason, s.StopReason, s.Complete, c.want)
			}
		})
	}
	out, _, _ := streamed(t, openaitest.Reply{Status: "failed", Reason: "cyber_policy", Message: "This request was flagged."}, ResponseOptions{Model: "m"}, false)
	if answer, _ := anthropic.Assemble([]byte(out), "text/event-stream"); len(answer.Blocks) != 1 || !strings.Contains(answer.Blocks[0].Text, "This request was flagged.") {
		t.Errorf("a policy refusal does not say OpenAI's message: %+v", answer)
	}
}

// A stream OpenAI cuts off ends without message_stop, as one Anthropic
// cut off would, and the caller is told to drop the connection.
func TestABrokenStreamEndsAsABrokenAnthropicStream(t *testing.T) {
	out, _, err := streamed(t, openaitest.Reply{Output: []openaitest.Item{openaitest.Text{Text: "partial answer"}}, Break: true}, ResponseOptions{Model: "m"}, false)
	if !errors.Is(err, ErrBrokenStream) {
		t.Fatalf("err = %v, want ErrBrokenStream", err)
	}
	if strings.Contains(out, "message_stop") || strings.Contains(out, "message_delta") {
		t.Errorf("a cut-off stream was ended properly:\n%s", out)
	}
	if s := observe(t, out); s.Complete {
		t.Error("the observer takes the cut-off stream as complete")
	}
}

func TestAnErrorPartWayBecomesAnAnthropicErrorEvent(t *testing.T) {
	out, res, err := streamed(t, openaitest.Reply{
		Output:      []openaitest.Item{openaitest.Text{Text: "started"}},
		StreamError: &openaitest.StreamError{Code: "server_is_overloaded", Message: "The server is overloaded."},
	}, ResponseOptions{Model: "m"}, false)
	if err != nil {
		t.Fatal(err)
	}
	s := observe(t, out)
	if s.Error == nil || s.Error.Type != "overloaded_error" || !strings.HasPrefix(s.Error.Message, "janus: ") {
		t.Fatalf("error = %+v, want an overloaded_error saying janus:\n%s", s.Error, out)
	}
	if res.Err == nil || res.Err.Kind != Overloaded || strings.Contains(out, "message_stop") {
		t.Errorf("result %+v; the stream should end at the error", res)
	}
}

// Each OpenAI error reaches Claude Code in Anthropic's shape, saying what
// to do, and a refused key is never a 401 or 403, which Claude Code would
// take as its own login failing.
func TestOpenAIErrorsAreExplainedInAnthropicsShape(t *testing.T) {
	for _, c := range []struct {
		name       string
		reply      openaitest.Reply
		kind       Kind
		status     int
		typ        string
		say        string
		retryAfter string
	}{
		{"bad key", openaitest.Fail(401, "invalid_request_error", "invalid_api_key", "Incorrect API key provided: janus-te****5d85."), KeyRefused, 500, "api_error", "the environment variable OPENAI_API_KEY", ""},
		{"no access", openaitest.Fail(403, "invalid_request_error", "unsupported_country_region_territory", "Country not supported."), KeyRefused, 500, "api_error", "may not serve your region", ""},
		{"per minute", openaitest.RateLimited("rate_limit_exceeded", "7"), RateLimited, 429, "rate_limit_error", "per-minute", "7"},
		{"quota", openaitest.RateLimited("insufficient_quota", ""), QuotaExhausted, 429, "rate_limit_error", "Retrying will not help", ""},
		{"too long", openaitest.Fail(400, "invalid_request_error", "context_length_exceeded", "Your input exceeds the context window."), TooLong, 400, "invalid_request_error", "prompt is too long", ""},
		{"bad request", openaitest.Fail(400, "invalid_request_error", "invalid_value", "Invalid value for 'input'."), BadRequest, 400, "invalid_request_error", "Invalid value for 'input'.", ""},
		{"server", openaitest.Fail(500, "server_error", "server_error", "The server had an error."), ServerError, 500, "api_error", "Try again", ""},
		{"overloaded", openaitest.Fail(503, "server_error", "server_is_overloaded", "Overloaded."), Overloaded, 529, "overloaded_error", "overloaded", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := openaitest.New(t, c.reply)
			resp := ask(t, srv, true)
			body, _ := io.ReadAll(resp.Body)
			f := TranslateError(resp.StatusCode, resp.Header, body, "env:OPENAI_API_KEY")
			if f.Kind != c.kind || f.Status != c.status || f.Type != c.typ || f.RetryAfter != c.retryAfter {
				t.Errorf("got %s %d %s retry %q, want %s %d %s retry %q", f.Kind, f.Status, f.Type, f.RetryAfter, c.kind, c.status, c.typ, c.retryAfter)
			}
			if !strings.HasPrefix(f.Message, "janus: ") || !strings.Contains(f.Message, c.say) {
				t.Errorf("message %q, want it to begin janus: and say %q", f.Message, c.say)
			}
			if strings.Contains(f.Message, "janus-te") {
				t.Error("the message quotes part of the key")
			}
			var e struct {
				Type  string             `json:"type"`
				Error anthropic.APIError `json:"error"`
			}
			if json.Unmarshal(f.Body(), &e) != nil || e.Type != "error" || e.Error.Type != c.typ || e.Error.Message != f.Message {
				t.Errorf("body %s is not Anthropic's error shape", f.Body())
			}
		})
	}
}
