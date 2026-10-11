// Package mcp is a small Model Context Protocol server: enough of the
// protocol to offer tools to a client over standard input and output.
//
// MCP is JSON-RPC 2.0, one message per line. A server that only offers
// tools has four things to answer: initialize, ping, tools/list, and
// tools/call. That is little enough to write directly, which keeps janus
// free of dependencies (ADR-001).
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Tool is something the client's model can call.
type Tool struct {
	Name        string
	Description string
	// InputSchema is the JSON Schema for the tool's arguments.
	InputSchema json.RawMessage
	// Call runs the tool. The text it returns goes back to the model. If
	// failed is true the text is a description of what went wrong, shown
	// to the model as an error so that it can try something else.
	Call func(ctx context.Context, arguments json.RawMessage) (text string, failed bool)
}

// versions are the protocol revisions this server speaks, newest first.
// For tools-only use they do not differ in anything it does.
var versions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// JSON-RPC error codes.
const (
	parseError     = -32700
	methodNotFound = -32601
	invalidParams  = -32602
)

// maxMessage bounds one incoming message.
const maxMessage = 16 << 20

// Serve reads requests from in and writes responses to out until in ends.
// Cancelling ctx stops tool calls in progress; it does not interrupt the
// read, so a caller that wants to stop on a signal should also close in or
// simply exit. name and version identify the server to the client.
//
// Tool calls run concurrently, since one may take minutes and the client
// should still be able to ping, or call another.
func Serve(ctx context.Context, in io.Reader, out io.Writer, name, version string, tools []Tool) error {
	var (
		write   sync.Mutex
		pending sync.WaitGroup
	)
	send := func(resp response) {
		resp.JSONRPC = "2.0"
		line, err := json.Marshal(resp)
		if err != nil {
			return
		}
		write.Lock()
		defer write.Unlock()
		out.Write(append(line, '\n'))
	}
	// When input ends, calls already asked for are allowed to finish and
	// their answers are written: the client asked, and may still be
	// reading. Cancelling ctx is how a caller stops them early.
	defer pending.Wait()

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64<<10), maxMessage)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			send(response{ID: json.RawMessage("null"), Error: &rpcError{parseError, "that is not a JSON-RPC message"}})
			continue
		}
		if len(req.ID) == 0 {
			continue // a notification: nothing to answer
		}

		switch req.Method {
		case "initialize":
			var params struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			json.Unmarshal(req.Params, &params)
			send(response{ID: req.ID, Result: map[string]any{
				"protocolVersion": agree(params.ProtocolVersion),
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": name, "version": version},
			}})
		case "ping":
			send(response{ID: req.ID, Result: map[string]any{}})
		case "tools/list":
			list := make([]map[string]any, len(tools))
			for i, t := range tools {
				list[i] = map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema}
			}
			send(response{ID: req.ID, Result: map[string]any{"tools": list}})
		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				send(response{ID: req.ID, Error: &rpcError{invalidParams, "tools/call needs a name and arguments"}})
				continue
			}
			tool, found := find(tools, params.Name)
			if !found {
				send(response{ID: req.ID, Error: &rpcError{invalidParams, fmt.Sprintf("there is no tool called %q", params.Name)}})
				continue
			}
			if len(params.Arguments) == 0 {
				params.Arguments = json.RawMessage("{}")
			}
			pending.Add(1)
			go func(id, arguments json.RawMessage) {
				defer pending.Done()
				text, failed := tool.Call(ctx, arguments)
				send(response{ID: id, Result: map[string]any{
					"content": []map[string]string{{"type": "text", "text": text}},
					"isError": failed,
				}})
			}(append(json.RawMessage(nil), req.ID...), append(json.RawMessage(nil), params.Arguments...))
		default:
			send(response{ID: req.ID, Error: &rpcError{methodNotFound, fmt.Sprintf("janus does not implement %q", req.Method)}})
		}
	}
	return scanner.Err()
}

// agree picks the protocol version to answer with: the client's, if this
// server speaks it, and otherwise the newest this server does.
func agree(asked string) string {
	for _, v := range versions {
		if v == asked {
			return v
		}
	}
	return versions[0]
}

func find(tools []Tool, name string) (Tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}
