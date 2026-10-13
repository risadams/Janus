package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Whatever a client sends, the server answers in well-formed JSON-RPC, one
// message per line, and never crashes.
func FuzzServe(f *testing.F) {
	f.Add(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n" +
		`{"jsonrpc":"2.0","id":"x","method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}` + "\n")
	f.Add("not json\n{}\n[]\n" + `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nope"}}` + "\n")
	f.Add(`{"jsonrpc":"2.0","id":null,"method":"ping"}` + "\n" + `{"jsonrpc":"1.0","id":4,"method":"ping"}`)
	echo := Tool{Name: "echo", Description: "echoes", InputSchema: json.RawMessage(`{"type":"object"}`),
		Call: func(ctx context.Context, args json.RawMessage) (string, bool) { return string(args), false }}

	f.Fuzz(func(t *testing.T, in string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var out bytes.Buffer
		if err := Serve(ctx, strings.NewReader(in), &out, "janus", "test", []Tool{echo}); err != nil && ctx.Err() != nil {
			t.Fatalf("Serve did not finish: %v", err)
		}
		sc := bufio.NewScanner(&out)
		sc.Buffer(make([]byte, 0, 1<<20), maxMessage*2)
		for sc.Scan() {
			var msg struct {
				JSONRPC string `json:"jsonrpc"`
			}
			if err := json.Unmarshal(sc.Bytes(), &msg); err != nil || msg.JSONRPC != "2.0" {
				t.Fatalf("an output line is not a JSON-RPC 2.0 message: %q", sc.Text())
			}
		}
	})
}
