package anthropic

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdata/request.json is a synthetic request in the shape Claude Code
// sends: mid-conversation system messages, tool_addition blocks, deferred
// tools, cache_control with a ttl, and fields janus has no model for.
func loadRequest(t *testing.T) ([]byte, *Request) {
	t.Helper()
	raw, err := os.ReadFile("testdata/request.json")
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw, r
}

func TestUnmodifiedRequestIsTheOriginalBytes(t *testing.T) {
	raw, r := loadRequest(t)
	if r.Modified() {
		t.Error("a freshly parsed request reports itself modified")
	}
	if !bytes.Equal(r.Bytes(), raw) {
		t.Error("Bytes() of an unmodified request differs from the input")
	}

	// Formatting janus would never produce itself must survive too.
	odd := []byte("{ \"stream\" : false,\n\t\"model\":\"m\" , \"messages\":[ ] }")
	r2, err := ParseRequest(odd)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(r2.Bytes(), odd) {
		t.Errorf("Bytes() = %q, want the input unchanged", r2.Bytes())
	}
}

func TestAccessors(t *testing.T) {
	_, r := loadRequest(t)

	if r.Model() != "claude-opus-5-5" || !r.Stream() || r.MaxTokens() != 128000 {
		t.Errorf("model=%q stream=%v max_tokens=%d", r.Model(), r.Stream(), r.MaxTokens())
	}
	if got := len(r.System()); got != 3 {
		t.Fatalf("system blocks = %d, want 3", got)
	}
	if !strings.Contains(r.System()[0].Text(), "cc_entrypoint=test") {
		t.Errorf("system[0] = %q", r.System()[0].Text())
	}

	var names []string
	for _, tool := range r.Tools() {
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != "Read,Bash,DeferredToolPlaceholder" {
		t.Errorf("tools = %v", names)
	}
	if _, deferred := r.Tools()[2].Field("defer_loading"); !deferred {
		t.Error("defer_loading is not visible on the placeholder tool")
	}

	var roles []string
	for _, m := range r.Messages() {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "user,system,assistant,user,system,assistant,user" {
		t.Errorf("roles = %v", roles)
	}
	if got := r.CacheBreakpoints(); got != 4 {
		t.Errorf("CacheBreakpoints() = %d, want 4 (two system, one tool_addition, one text)", got)
	}
	if v, ok := r.Field("output_config"); !ok || string(v) != `{"effort":"medium"}` {
		t.Errorf("Field(output_config) = %s, %v", v, ok)
	}
	if _, ok := r.Field("no_such_field"); ok {
		t.Error("Field reports a field that is not there")
	}
}

func TestBlockViews(t *testing.T) {
	_, r := loadRequest(t)
	assistant := r.Messages()[2].Blocks()

	var thinking ThinkingBlock
	if assistant[0].Type != "thinking" || assistant[0].Decode(&thinking) != nil || thinking.Signature != "c2lnbmF0dXJlLWJ5dGVz" {
		t.Errorf("thinking block = %+v (type %q)", thinking, assistant[0].Type)
	}

	var use ToolUseBlock
	if assistant[2].Decode(&use); use.ID != "toolu_01EXAMPLE" || use.Name != "Read" || !strings.Contains(string(use.Input), "/work/notes.txt") {
		t.Errorf("tool_use block = %+v", use)
	}

	var result ToolResultBlock
	r.Messages()[3].Blocks()[0].Decode(&result)
	inner, err := ParseContent(result.Content)
	if err != nil || result.ToolUseID != "toolu_01EXAMPLE" || len(inner) != 1 || inner[0].Text() != "line one\nline two" {
		t.Errorf("tool_result = %+v, inner = %v, err = %v", result, inner, err)
	}

	var cc CacheControl
	v, _ := r.System()[1].Field("cache_control")
	if json.Unmarshal(v, &cc); cc.Type != "ephemeral" || cc.TTL != "1h" {
		t.Errorf("cache_control = %+v", cc)
	}

	// A block type janus has no view for is still a block.
	added := r.Messages()[1].Blocks()[1]
	if added.Type != "tool_addition" || added.Text() != "" {
		t.Errorf("tool_addition block: type=%q text=%q", added.Type, added.Text())
	}
}

func TestStringContentIsOneTextBlock(t *testing.T) {
	_, r := loadRequest(t)
	for _, i := range []int{4, 6} {
		blocks := r.Messages()[i].Blocks()
		if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text() == "" {
			t.Errorf("messages[%d] blocks = %+v, want one text block", i, blocks)
		}
	}

	r2, err := ParseRequest([]byte(`{"system":"Be brief.","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if s := r2.System(); len(s) != 1 || s[0].Text() != "Be brief." {
		t.Errorf("string system = %+v", s)
	}
}

func TestSetModelChangesOnlyTheModel(t *testing.T) {
	raw, r := loadRequest(t)
	r.SetModel("qwen/qwen3.8-27b")

	want := bytes.Replace(raw, []byte(`"model":"claude-opus-5-5"`), []byte(`"model":"qwen/qwen3.8-27b"`), 1)
	if !bytes.Equal(r.Bytes(), want) {
		t.Errorf("after SetModel the body differs from the original by more than the model:\n got %s\nwant %s", r.Bytes(), want)
	}
	if !r.Modified() || r.Model() != "qwen/qwen3.8-27b" {
		t.Errorf("Modified()=%v Model()=%q", r.Modified(), r.Model())
	}
}

// The edit LM Studio needs (docs/research/lmstudio.md): lift tool_addition
// definitions into tools and drop the blocks, leaving everything else as it
// was.
func TestEditingOneMessageLeavesTheRestByteIdentical(t *testing.T) {
	_, r := loadRequest(t)
	before := r.Messages()

	var kept []Block
	tools := r.Tools()[:2] // drop the deferred placeholder
	for _, b := range before[1].Blocks() {
		if b.Type != "tool_addition" {
			kept = append(kept, b)
			continue
		}
		var added struct {
			Tool struct {
				Definition json.RawMessage `json:"definition"`
			} `json:"tool"`
		}
		if err := b.Decode(&added); err != nil {
			t.Fatal(err)
		}
		tool, err := NewTool(added.Tool.Definition)
		if err != nil {
			t.Fatal(err)
		}
		tools = append(tools, tool)
	}
	after := append([]Message{}, before...)
	after[1] = before[1].WithBlocks(kept)
	r.SetMessages(after)
	r.SetTools(tools)

	out := r.Bytes()
	again, err := ParseRequest(out)
	if err != nil {
		t.Fatalf("edited request does not parse: %v\n%s", err, out)
	}

	if strings.Contains(string(out), "tool_addition") {
		t.Error("tool_addition blocks are still present")
	}
	var names []string
	for _, tool := range again.Tools() {
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != "Read,Bash,mcp__docs__search,mcp__docs__read" {
		t.Errorf("tools after edit = %v", names)
	}
	for i, m := range before {
		if i == 1 {
			continue
		}
		if !bytes.Contains(out, m.Raw()) {
			t.Errorf("messages[%d] was not edited but its bytes changed", i)
		}
	}
	if got := again.Messages()[1]; got.Role != "system" || len(got.Blocks()) != 1 {
		t.Errorf("edited message = role %q with %d blocks, want system with 1", got.Role, len(got.Blocks()))
	}
	for _, untouched := range []string{
		`"future_top_level_field":[1,2.50,1e3,null]`,
		`"future_message_field":{"kept":true}`,
		`"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`,
		`<system-reminder>`,
		`& tell me`,
		`日本語`,
	} {
		if !bytes.Contains(out, []byte(untouched)) {
			t.Errorf("edited body lost or re-spelled %s", untouched)
		}
	}
}

func TestWithBlocksKeepsOtherMessageFields(t *testing.T) {
	_, r := loadRequest(t)
	m := r.Messages()[5].WithBlocks([]Block{NewTextBlock("a < b")})

	want := `{"role":"assistant","content":[{"type":"text","text":"a < b"}],"future_message_field":{"kept":true}}`
	if string(m.Raw()) != want {
		t.Errorf("Raw() = %s\nwant    %s", m.Raw(), want)
	}
}

func TestParseRequestErrors(t *testing.T) {
	for name, body := range map[string]string{
		"not json":           `{"model":`,
		"not an object":      `["model"]`,
		"trailing data":      `{"model":"m"} {"x":1}`,
		"messages not array": `{"messages":"hi"}`,
		"message not object": `{"messages":["hi"]}`,
		"block not object":   `{"messages":[{"role":"user","content":[1]}]}`,
		"model not string":   `{"model":7}`,
	} {
		if _, err := ParseRequest([]byte(body)); err == nil {
			t.Errorf("%s: ParseRequest(%s) succeeded, want an error", name, body)
		}
	}
}

// Set JANUS_CAPTURES to a directory of blush captures to check the model
// against real traffic. Captures hold account identifiers and prompts, so
// they are never committed; this test is how they still get exercised.
func TestRealCaptures(t *testing.T) {
	dir := os.Getenv("JANUS_CAPTURES")
	if dir == "" {
		t.Skip("JANUS_CAPTURES is not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*v1-messages.request.txt"))
	more, _ := filepath.Glob(filepath.Join(dir, "*v1-messages.request.txt"))
	files = append(files, more...)
	if len(files) == 0 {
		t.Fatalf("no *v1-messages.request.txt files under %s", dir)
	}

	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		r, err := ParseRequest(raw)
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
			continue
		}
		if !bytes.Equal(r.Bytes(), raw) {
			t.Errorf("%s: unmodified Bytes() differs from the capture", filepath.Base(f))
		}
		if r.Model() == "" || len(r.Messages()) == 0 {
			t.Errorf("%s: model=%q messages=%d", filepath.Base(f), r.Model(), len(r.Messages()))
		}

		// Force every setter with the values already there. The result
		// must be the same body: re-encoding may not disturb anything.
		r.SetModel(r.Model())
		r.SetTools(r.Tools())
		r.SetMessages(r.Messages())
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(r.Bytes(), compact.Bytes()) {
			t.Errorf("%s: re-encoding with unchanged values altered the body", filepath.Base(f))
		}
	}
	t.Logf("checked %d real requests", len(files))
}
