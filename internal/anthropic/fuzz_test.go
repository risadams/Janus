package anthropic

import (
	"bytes"
	"reflect"
	"testing"
)

// Seeds for the request fuzzers: hand-built, in the shapes Claude Code
// sends. Real captures are never used; they hold prompts and credentials.
var requestSeeds = []string{
	`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
	`{"model":"claude-opus-5-5","max_tokens":32000,"stream":true,"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
		`"tools":[{"name":"Bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":[{"type":"text","text":"run it"}]},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"c2ln"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a.go\nb.go","cache_control":{"type":"ephemeral"}}]}],"metadata":{"user_id":"u"}}`,
	`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"x"}}}]},{"role":"system","content":"note"}]}`,
	`{"messages":[],"unknown_field":[1,2,{"deep":null}]}`,
}

// A request Janus parses but does not change must go out byte for byte as
// it came in, whatever it holds; and re-encoding it must give a request
// that parses to the same thing.
func FuzzParseRequest(f *testing.F) {
	for _, s := range requestSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := ParseRequest(data)
		if err != nil {
			return
		}
		if !bytes.Equal(r.Bytes(), data) {
			t.Fatalf("an unmodified request did not encode to its own bytes")
		}
		model, n, tools := r.Model(), len(r.Messages()), len(r.Tools())
		r.SetMessages(r.Messages())
		r.SetTools(r.Tools())
		again, err := ParseRequest(r.Bytes())
		if err != nil {
			t.Fatalf("a re-encoded request does not parse: %v\n%s", err, r.Bytes())
		}
		if again.Model() != model || len(again.Messages()) != n || len(again.Tools()) != tools {
			t.Fatalf("re-encoding changed the request: model %q→%q, messages %d→%d, tools %d→%d",
				model, again.Model(), n, len(again.Messages()), tools, len(again.Tools()))
		}
	})
}

const streamSeed = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"m1","model":"claude-opus-5-5","usage":{"input_tokens":2,"cache_read_input_tokens":900}}}` + "\n\n" +
	"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}` + "\n\n" +
	": keep-alive\n\n" +
	"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}` + "\n\n" +
	"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}` + "\n\n" +
	"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"

// The Observer reads a response as it is relayed, in whatever pieces the
// network delivers. How a body is cut must not change what it learns.
func FuzzObserverIgnoresHowTheBodyIsCut(f *testing.F) {
	f.Add([]byte(streamSeed), uint16(37), true)
	f.Add([]byte(`{"type":"message","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}`), uint16(5), false)
	f.Add([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`), uint16(9), false)
	f.Fuzz(func(t *testing.T, body []byte, cut uint16, stream bool) {
		contentType := "application/json"
		if stream {
			contentType = "text/event-stream"
		}
		whole := NewObserver(contentType)
		whole.Write(body)
		at := int(cut) % (len(body) + 1)
		pieces := NewObserver(contentType)
		pieces.Write(body[:at])
		pieces.Write(body[at:])
		if a, b := whole.Summary(), pieces.Summary(); !reflect.DeepEqual(a, b) {
			t.Fatalf("cut at %d of %d changed the summary:\nwhole  %+v\npieces %+v", at, len(body), a, b)
		}
		Assemble(body, contentType) // must not panic
	})
}
