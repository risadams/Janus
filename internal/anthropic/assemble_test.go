package anthropic

import (
	"os"
	"testing"
)

func TestAssembleStream(t *testing.T) {
	body, err := os.ReadFile("testdata/stream.sse")
	if err != nil {
		t.Fatal(err)
	}
	a, ok := Assemble(body, "text/event-stream; charset=utf-8")
	if !ok || a.StopReason != "tool_use" || len(a.Blocks) != 3 {
		t.Fatalf("answer = %+v, ok = %v", a, ok)
	}
	want := []AnswerBlock{
		{Type: "thinking"},
		{Type: "text", Text: "Reading it now."},
		{Type: "tool_use", Name: "Read", ID: "toolu_01EXAMPLE", Input: `{"file_path":"/work/notes.txt"}`},
	}
	for i, w := range want {
		if a.Blocks[i] != w {
			t.Errorf("block %d = %+v, want %+v", i, a.Blocks[i], w)
		}
	}
}

func TestAssembleJSON(t *testing.T) {
	body := `{"type":"message","stop_reason":"tool_use","content":[
		{"type":"thinking","thinking":"hmm"},
		{"type":"text","text":"On it."},
		{"type":"tool_use","id":"364900003","name":"Bash","input":{"command":"ls"}}]}`
	a, ok := Assemble([]byte(body), "application/json")
	if !ok || a.StopReason != "tool_use" || len(a.Blocks) != 3 {
		t.Fatalf("answer = %+v, ok = %v", a, ok)
	}
	if a.Blocks[0].Text != "hmm" || a.Blocks[1].Text != "On it." || a.Blocks[2].Name != "Bash" || a.Blocks[2].Input != `{"command":"ls"}` {
		t.Errorf("blocks = %+v", a.Blocks)
	}
}

func TestAssembleKeepsBrokenToolInputAsItCame(t *testing.T) {
	stream := "data: {\"type\":\"message_start\",\"message\":{}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"name\":\"Read\",\"id\":\"1\",\"input\":{}}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"file_path\\\": \"}}\n\n"
	a, ok := Assemble([]byte(stream), "text/event-stream")
	if !ok || len(a.Blocks) != 1 || a.Blocks[0].Input != `{"file_path": ` {
		t.Errorf("answer = %+v; a cut-off tool input must be kept as it is, for the validator to judge", a)
	}
}

func TestAssembleRejectsWhatIsNotAnAnswer(t *testing.T) {
	for name, c := range map[string]struct{ body, contentType string }{
		"an API error":           {`{"type":"error","error":{"type":"overloaded_error"}}`, "application/json"},
		"html":                   {"<html>502</html>", "text/html"},
		"empty":                  {"", "application/json"},
		"a stream with no start": {"data: {\"type\":\"ping\"}\n\n", "text/event-stream"},
	} {
		if a, ok := Assemble([]byte(c.body), c.contentType); ok {
			t.Errorf("%s: accepted as %+v", name, a)
		}
	}
	// An absurd block index is ignored, not allocated for.
	huge := "data: {\"type\":\"message_start\"}\n\ndata: {\"type\":\"content_block_start\",\"index\":999999999,\"content_block\":{\"type\":\"text\"}}\n\n"
	if a, _ := Assemble([]byte(huge), "text/event-stream"); len(a.Blocks) != 0 {
		t.Errorf("%d blocks allocated for an out-of-range index", len(a.Blocks))
	}
}
