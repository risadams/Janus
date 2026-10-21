package openai

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/openai/openaitest"
)

// A conversation that moves Claude → OpenAI → Claude keeps everything it
// said, and each side is sent only what it accepts (#375).

// conversation is a Claude Code session's history, as Claude Code keeps it:
// each turn exactly as it was received.
type conversation struct {
	messages []string
}

func (c *conversation) add(m ...string) { c.messages = append(c.messages, m...) }

func (c *conversation) request() *anthropic.Request {
	body := `{"model":"claude-sonnet-5-5","max_tokens":32000,"stream":true,` +
		`"system":[{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],` +
		`"tools":[{"name":"Bash","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}},` +
		`{"name":"mcp__claude-in-chrome__read_page","input_schema":{"type":"object"}}],` +
		`"thinking":{"type":"adaptive","display":"omitted"},` +
		`"messages":[` + strings.Join(c.messages, ",") + `]}`
	req, err := anthropic.ParseRequest([]byte(body))
	if err != nil {
		panic(err)
	}
	return req
}

// toClaude is the request as janus sends it upstream: repaired, as every
// upstream plan is.
func (c *conversation) toClaude(t *testing.T) *anthropic.Request {
	t.Helper()
	req := c.request()
	backend.RepairForUpstream(req)
	acceptedByClaude(t, req)
	return req
}

var claudeToolID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// acceptedByClaude checks what the Messages API is known to refuse: a
// thinking block without a signature, a tool id outside its pattern, a
// tool result with no call before it, and an empty message.
func acceptedByClaude(t *testing.T, req *anthropic.Request) {
	t.Helper()
	calls := map[string]bool{}
	for i, m := range req.Messages() {
		if len(m.Blocks()) == 0 {
			t.Errorf("messages[%d] is empty", i)
		}
		for _, b := range m.Blocks() {
			switch b.Type {
			case "thinking":
				var th anthropic.ThinkingBlock
				_ = b.Decode(&th)
				if th.Signature == "" {
					t.Errorf("messages[%d] sends Claude a thinking block with no signature", i)
				}
			case "tool_use":
				var u anthropic.ToolUseBlock
				_ = b.Decode(&u)
				if !claudeToolID.MatchString(u.ID) {
					t.Errorf("messages[%d] has tool id %q, which Claude refuses", i, u.ID)
				}
				calls[u.ID] = true
			case "tool_result":
				var r anthropic.ToolResultBlock
				_ = b.Decode(&r)
				if !calls[r.ToolUseID] {
					t.Errorf("messages[%d] answers tool call %q, which was never made", i, r.ToolUseID)
				}
			}
		}
	}
}

// acceptedByOpenAI checks the same of a translated request: every output
// follows its call, and nothing of Claude's signed thinking goes.
func acceptedByOpenAI(t *testing.T, body []byte) {
	t.Helper()
	var b struct {
		Input []struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	calls := map[string]bool{}
	for i, it := range b.Input {
		switch it.Type {
		case "function_call":
			calls[it.CallID] = true
		case "function_call_output":
			if !calls[it.CallID] {
				t.Errorf("input[%d] answers call %q, which was never made", i, it.CallID)
			}
		case "reasoning":
			t.Errorf("input[%d] is reasoning, which no translation without #410 sends", i)
		}
	}
	if bytes.Contains(body, []byte("EqQBClaudeSig")) || bytes.Contains(body, []byte("I should list")) {
		t.Error("Claude's signed thinking went to OpenAI")
	}
}

// messagesJSON is the request's messages array as it will be sent.
func messagesJSON(req *anthropic.Request) []json.RawMessage {
	var b struct {
		Messages []json.RawMessage `json:"messages"`
	}
	_ = json.Unmarshal(req.Bytes(), &b)
	return b.Messages
}

// received is the assistant message Claude Code keeps from a translated
// stream: each block as the stream built it, signature and all.
func received(t *testing.T, stream string) string {
	t.Helper()
	var blocks []map[string]any
	for _, chunk := range strings.Split(stream, "\n\n") {
		_, data, ok := strings.Cut(chunk, "data: ")
		if !ok {
			continue
		}
		var e struct {
			Type         string         `json:"type"`
			Index        int            `json:"index"`
			ContentBlock map[string]any `json:"content_block"`
			Delta        map[string]any `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &e) != nil {
			continue
		}
		switch e.Type {
		case "content_block_start":
			blocks = append(blocks, e.ContentBlock)
		case "content_block_delta":
			b := blocks[e.Index]
			switch e.Delta["type"] {
			case "text_delta":
				b["text"] = b["text"].(string) + e.Delta["text"].(string)
			case "input_json_delta":
				b["partial"] = asString(b["partial"]) + e.Delta["partial_json"].(string)
			case "signature_delta":
				b["signature"] = e.Delta["signature"]
			}
		}
	}
	for _, b := range blocks {
		if p, ok := b["partial"]; ok {
			var input any
			if err := json.Unmarshal([]byte(p.(string)), &input); err != nil {
				t.Fatalf("tool input %q does not parse", p)
			}
			b["input"] = input
			delete(b, "partial")
		}
	}
	out, _ := json.Marshal(map[string]any{"role": "assistant", "content": blocks})
	return string(out)
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func TestAConversationMovesToOpenAIAndBackKeepingItsHistory(t *testing.T) {
	var c conversation

	// Two turns on Claude, with its signed thinking and a tool call.
	c.add(`{"role":"user","content":"What is in this folder?"}`,
		`{"role":"assistant","content":[{"type":"thinking","thinking":"I should list it.","signature":"EqQBClaudeSig"},{"type":"tool_use","id":"toolu_01ABC","name":"Bash","input":{"command":"ls"}}]}`,
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01ABC","content":"a.go\nb.go"}]}`,
		`{"role":"assistant","content":[{"type":"text","text":"Two Go files."}]}`)
	sentToClaude := messagesJSON(c.toClaude(t))

	// The conversation moves to OpenAI, which answers with reasoning and a
	// call whose id Claude would refuse.
	c.add(`{"role":"user","content":"Open the docs page."}`)
	tr, err := TranslateRequest(c.request(), RequestOptions{Model: "gpt-6.1-sol", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	acceptedByOpenAI(t, tr.Body)
	srv := openaitest.New(t, openaitest.Answer(
		openaitest.Reasoning{Encrypted: "gAAAAopenai"},
		openaitest.Call{CallID: "call_7f.2/x", Name: encodeToolName("mcp__claude-in-chrome__read_page"), Arguments: `{"tab":1}`},
	))
	var stream bytes.Buffer
	if _, err := TranslateStream(&stream, ask(t, srv, true).Body, ResponseOptions{Model: "claude-sonnet-5-5", Tools: tr.Tools}); err != nil {
		t.Fatal(err)
	}
	turn := received(t, stream.String())
	id := ToolID("call_7f.2/x")
	if !strings.Contains(turn, `"id":"`+id+`"`) || !strings.Contains(turn, `"name":"mcp__claude-in-chrome__read_page"`) {
		t.Fatalf("Claude Code received %s", turn)
	}
	c.add(turn, `{"role":"user","content":[{"type":"tool_result","tool_use_id":"`+id+`","content":"Docs"}]}`)

	// The next turn to OpenAI repeats the last one's prefix.
	again, err := TranslateRequest(c.request(), RequestOptions{Model: "gpt-6.1-sol", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	acceptedByOpenAI(t, again.Body)
	if !bytes.Contains(again.Body, []byte(`"call_id":"`+id+`"`)) {
		t.Error("the call went back to OpenAI under another id than its output's")
	}

	// Back to Claude: OpenAI's unsigned thinking is left out, and every
	// turn Claude was sent before the move goes in the same bytes.
	back := messagesJSON(c.toClaude(t))
	for i := range sentToClaude {
		if !bytes.Equal(sentToClaude[i], back[i]) {
			t.Errorf("messages[%d], sent to Claude before the move, changed:\n%s\n%s", i, sentToClaude[i], back[i])
		}
	}
	if len(back) != len(c.messages) {
		t.Errorf("%d messages reached Claude, want all %d", len(back), len(c.messages))
	}

	// And on the turn after, what was sent on the return is sent again as
	// it was, so Claude's cache breaks once per move, not every turn.
	c.add(`{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"EqQBNext"},{"type":"text","text":"It is the docs page."}]}`,
		`{"role":"user","content":"Thanks."}`)
	next := messagesJSON(c.toClaude(t))
	for i := range back {
		if !bytes.Equal(back[i], next[i]) {
			t.Errorf("messages[%d] changed between two turns on Claude:\n%s\n%s", i, back[i], next[i])
		}
	}
}

func TestToolIDsClaudeWouldRefuseAreMadeValidTheSameWayEachTime(t *testing.T) {
	for _, id := range []string{"call_abc123", "fc_68af-01"} {
		if ToolID(id) != id {
			t.Errorf("%q was changed, though Claude accepts it", id)
		}
	}
	for _, id := range []string{"call_7f.2/x", "call:1", strings.Repeat("a", 65), ""} {
		got := ToolID(id)
		if !claudeToolID.MatchString(got) || got != ToolID(id) {
			t.Errorf("%q became %q", id, got)
		}
	}
	if ToolID("call:1") == ToolID("call:2") {
		t.Error("two ids became one")
	}
}
