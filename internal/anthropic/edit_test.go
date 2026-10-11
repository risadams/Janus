package anthropic

import "testing"

func TestSetResponseModel(t *testing.T) {
	cases := map[string]struct {
		in, want string
		ok       bool
	}{
		"whole message": {
			`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"model: x"}],"model":"qwen/qwen3.8-27b","stop_reason":"end_turn","usage":{"input_tokens":17}}`,
			`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"model: x"}],"model":"janus-local","stop_reason":"end_turn","usage":{"input_tokens":17}}`,
			true,
		},
		"message_start event": {
			`{"type":"message_start","message":{"id":"msg_1","type":"message","content":[],"model":"qwen/qwen3.8-27b","usage":{"input_tokens":363,"output_tokens":0}}}`,
			`{"type":"message_start","message":{"id":"msg_1","type":"message","content":[],"model":"janus-local","usage":{"input_tokens":363,"output_tokens":0}}}`,
			true,
		},
		"another event":           {`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"\"model\":\"x\""}}`, "", false},
		"an error":                {`{"type":"error","error":{"type":"overloaded_error","message":"model busy"}}`, "", false},
		"a message with no model": {`{"type":"message","content":[]}`, "", false},
		"not json":                {`data: nope`, "", false},
	}
	for name, c := range cases {
		got, ok := SetResponseModel([]byte(c.in), "janus-local")
		want := c.want
		if !c.ok {
			want = c.in
		}
		if ok != c.ok || string(got) != want {
			t.Errorf("%s: ok=%v\n got %s\nwant %s", name, ok, got, want)
		}
	}
}
