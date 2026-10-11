package validate

import (
	"strings"
	"testing"

	"github.com/risadams/janus/internal/anthropic"
)

const request = `{"model":"claude-opus-5-5","tools":[
	{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"limit":{"type":"integer"},"mode":{"type":"string","enum":["text","binary"]}},"required":["file_path"],"additionalProperties":false}},
	{"name":"Bash","input_schema":{"type":"object","properties":{"command":{"type":"string"},"env":{"type":"object","properties":{"PATH":{"type":"string"}}},"args":{"type":"array","items":{"type":"string"}}},"required":["command"]}},
	{"name":"NoSchema"}],
	"messages":[{"role":"user","content":"hi"}]}`

func req(t *testing.T) *anthropic.Request {
	t.Helper()
	r, err := anthropic.ParseRequest([]byte(request))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var done = anthropic.Summary{Complete: true}

func text(s string) anthropic.Answer {
	return anthropic.Answer{StopReason: "end_turn", Blocks: []anthropic.AnswerBlock{{Type: "text", Text: s}}}
}

func call(name, input string) anthropic.Answer {
	return anthropic.Answer{StopReason: "tool_use", Blocks: []anthropic.AnswerBlock{{Type: "tool_use", Name: name, ID: "1", Input: input}}}
}

func TestGoodAnswersPass(t *testing.T) {
	cases := map[string]anthropic.Answer{
		"plain text":                     text("a.txt has 2 lines."),
		"a tool call":                    call("Read", `{"file_path":"/tmp/a.txt"}`),
		"optional arguments":             call("Read", `{"file_path":"/tmp/a.txt","limit":100,"mode":"text"}`),
		"nested object and array":        call("Bash", `{"command":"ls","env":{"PATH":"/bin"},"args":["-l","-a"]}`),
		"extra argument on an open tool": call("Bash", `{"command":"ls","timeout":5}`),
		"a tool with no schema":          call("NoSchema", `{"anything":true}`),
		"thinking, text, and a call": {StopReason: "tool_use", Blocks: []anthropic.AnswerBlock{
			{Type: "thinking", Text: "let me look"}, {Type: "text", Text: "Reading."}, {Type: "tool_use", Name: "Read", Input: `{"file_path":"x"}`}}},
		"a long answer that mentions not being able to": text(strings.Repeat("The function reads the file and counts newlines. ", 12) + "I cannot see a bug in it."),
		"an answer that says sorry later":               text("The count is 2. Sorry, I miscounted earlier."),
	}
	for name, answer := range cases {
		if failed, why := Check(req(t), done, answer, true, nil); failed != "" {
			t.Errorf("%s: failed %s: %s", name, failed, why)
		}
	}
}

func TestBadAnswersFail(t *testing.T) {
	cases := map[string]struct {
		summary anthropic.Summary
		answer  anthropic.Answer
		want    string
		whyHas  string
	}{
		"nothing at all":                     {done, anthropic.Answer{StopReason: "end_turn"}, Empty, "no text and no tool call"},
		"only thinking":                      {done, anthropic.Answer{StopReason: "end_turn", Blocks: []anthropic.AnswerBlock{{Type: "thinking", Text: "hmm"}}}, Empty, ""},
		"only whitespace":                    {done, text("  \n\t "), Empty, ""},
		"out of tokens":                      {done, anthropic.Answer{StopReason: "max_tokens", Blocks: []anthropic.AnswerBlock{{Type: "text", Text: "The answer is"}}}, Truncated, "ran out of tokens"},
		"a tool that was not offered":        {done, call("Write", `{"file_path":"x"}`), Tools, `calls "Write"`},
		"arguments that are not JSON":        {done, call("Read", `{"file_path": `), Schema, "not valid JSON"},
		"arguments that are an array":        {done, call("Read", `["/tmp/a.txt"]`), Schema, "should be object, not array"},
		"a required argument missing":        {done, call("Read", `{"limit":5}`), Schema, "file_path is missing"},
		"an argument of the wrong type":      {done, call("Read", `{"file_path":42}`), Schema, "file_path should be string, not integer"},
		"a fraction for an integer":          {done, call("Read", `{"file_path":"x","limit":1.5}`), Schema, "limit should be integer, not number"},
		"a value outside the enum":           {done, call("Read", `{"file_path":"x","mode":"hex"}`), Schema, "mode is not one of the allowed values"},
		"an argument the tool does not take": {done, call("Read", `{"file_path":"x","encoding":"utf8"}`), Schema, "encoding is not a parameter"},
		"wrong type inside an object":        {done, call("Bash", `{"command":"ls","env":{"PATH":1}}`), Schema, "env.PATH should be string"},
		"wrong type inside an array":         {done, call("Bash", `{"command":"ls","args":["-l",7]}`), Schema, "args[1] should be string"},
		"no-schema tool given a string":      {done, call("NoSchema", `"just text"`), Schema, "not a JSON object"},
		"a refusal":                          {done, text("I cannot help with that request."), Refusal, "refusal"},
		"an apology":                         {done, text("I’m sorry, but I don't have access to your files."), Refusal, ""},
		"as an AI":                           {done, text("As an AI language model, I do not have a filesystem."), Refusal, ""},
		"an API error":                       {anthropic.Summary{Complete: true, Error: &anthropic.APIError{Type: "invalid_request_error"}}, anthropic.Answer{}, "error", "invalid_request_error"},
		"cut off":                            {anthropic.Summary{}, text("The answer"), "incomplete", ""},
		"unreadable events":                  {anthropic.Summary{Complete: true, Malformed: 1}, text("ok"), "malformed", ""},
	}
	for name, c := range cases {
		failed, why := Check(req(t), c.summary, c.answer, true, nil)
		if failed != c.want || !strings.Contains(why, c.whyHas) || why == "" {
			t.Errorf("%s: failed %q (%s), want %q with %q", name, failed, why, c.want, c.whyHas)
		}
	}
	if failed, _ := Check(req(t), done, text("fine"), false, nil); failed != "malformed" {
		t.Errorf("an answer that could not be assembled: failed %q, want malformed", failed)
	}
}

// One broken call among good ones is enough.
func TestEveryToolCallIsChecked(t *testing.T) {
	answer := anthropic.Answer{StopReason: "tool_use", Blocks: []anthropic.AnswerBlock{
		{Type: "tool_use", Name: "Read", Input: `{"file_path":"a"}`},
		{Type: "tool_use", Name: "Bash", Input: `{}`},
	}}
	if failed, why := Check(req(t), done, answer, true, nil); failed != Schema || !strings.Contains(why, "Bash") {
		t.Errorf("failed %q (%s), want the second call caught", failed, why)
	}
}

func TestSkippingChecks(t *testing.T) {
	cases := map[string]struct {
		answer anthropic.Answer
		skip   string
	}{
		"empty":     {anthropic.Answer{StopReason: "end_turn"}, Empty},
		"truncated": {anthropic.Answer{StopReason: "max_tokens", Blocks: []anthropic.AnswerBlock{{Type: "text", Text: "x"}}}, Truncated},
		"tools":     {call("Write", `{}`), Tools},
		"schema":    {call("Read", `{"limit":"five"}`), Schema},
		"refusal":   {text("I cannot do that."), Refusal},
	}
	for name, c := range cases {
		if failed, _ := Check(req(t), done, c.answer, true, nil); failed != c.skip {
			t.Errorf("%s: with nothing skipped, failed %q, want %q", name, failed, c.skip)
		}
		if failed, why := Check(req(t), done, c.answer, true, []string{c.skip}); failed != "" {
			t.Errorf("%s: skipped, but still failed %q (%s)", name, failed, why)
		}
	}
	// What can never be skipped.
	if failed, _ := Check(req(t), anthropic.Summary{}, text("x"), true, All); failed != "incomplete" {
		t.Errorf("a cut-off answer passed with every check skipped: %q", failed)
	}
}

// The schema check must never reject what a full validator would accept.
// Keywords it does not implement are ignored.
func TestSchemaCheckIsLenientAboutWhatItDoesNotKnow(t *testing.T) {
	r, _ := anthropic.ParseRequest([]byte(`{"model":"m","tools":[{"name":"T","input_schema":{
		"type":"object",
		"properties":{
			"n":{"type":"integer","minimum":10},
			"s":{"type":"string","pattern":"^[a-z]+$","maxLength":3},
			"u":{"anyOf":[{"type":"string"},{"type":"number"}]},
			"nullable":{"type":["string","null"]},
			"num":{"type":"number"},
			"any":true
		},
		"oneOf":[{"required":["n"]},{"required":["s"]}]}}],"messages":[]}`))
	answer := call("T", `{"n":1,"s":"TOO-LONG","u":[1,2],"nullable":null,"num":3,"any":{"x":1}}`)
	if failed, why := Check(r, done, answer, true, nil); failed != "" {
		t.Errorf("failed %s: %s", failed, why)
	}
	if failed, _ := Check(r, done, call("T", `{"nullable":5}`), true, nil); failed != Schema {
		t.Errorf("a type list should still be enforced: failed %q", failed)
	}
}

func TestKnown(t *testing.T) {
	for _, name := range All {
		if !Known(name) {
			t.Errorf("Known(%q) = false", name)
		}
	}
	if Known("vibes") || Known("") {
		t.Error("Known accepts a name that is not a check")
	}
}
