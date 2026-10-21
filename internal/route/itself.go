package route

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/proxy"
)

// Itself answers what Claude Code sends that would have gone to Anthropic,
// when there is no Anthropic account behind janus (upstream.off, #376):
// it is proxy.Options.Itself. Claude Code calls four endpoints through its
// base URL (docs/research/traffic.md, Request classes seen), and each is
// answered here without anything leaving the machine:
//
//   - GET / and HEAD or GET /api/hello, its connectivity checks: an empty
//     200.
//   - POST /v1/messages/count_tokens: janus's own estimate, as
//     EstimateTokens makes it, which over-counts. Sending the conversation
//     to OpenAI to be counted would send it twice, at a price OpenAI does
//     not publish (#399).
//   - The start-up quota check, a POST /v1/messages the router plans for
//     janus: an answer of no tokens, with no rate-limit headers, since
//     there are no limits of Anthropic's to report.
//
// Every other Messages request goes along a rule's fallback list, and
// anything else is refused with a 404 in Anthropic's shape.
func (r *Router) Itself(req *http.Request, body []byte, plan proxy.Plan) proxy.Reply {
	path := req.URL.Path
	switch {
	case (req.Method == http.MethodGet || req.Method == http.MethodHead) && (path == "/" || path == "/api/hello"):
		return proxy.Reply{Status: http.StatusOK, Route: "janus", Reason: "connectivity check: answered by janus, since upstream is off"}
	case req.Method == http.MethodPost && path == "/v1/messages/count_tokens":
		out, _ := json.Marshal(map[string]int{"input_tokens": EstimateTokens(body)}) // an int marshals
		return proxy.Reply{Status: http.StatusOK, ContentType: "application/json", Body: out, Route: "janus",
			Reason: fmt.Sprintf("token count: estimated by janus at %d bytes a token, since upstream is off", bytesPerToken)}
	case req.Method == http.MethodPost && path == "/v1/messages" && plan.Route == "janus":
		model := "claude"
		if parsed, err := anthropic.ParseRequest(body); err == nil {
			model = parsed.Model()
		}
		out, _ := json.Marshal(map[string]any{ // strings and numbers marshal
			"id": "msg_janus_quota", "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": "max_tokens", "stop_sequence": nil,
			"usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
		})
		return proxy.Reply{Status: http.StatusOK, ContentType: "application/json", Body: out, Route: "janus", Reason: plan.Reason}
	}
	message := fmt.Sprintf("janus: upstream is off, and janus does not answer %s %s itself", req.Method, path)
	out, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": "not_found_error", "message": message}})
	return proxy.Reply{Status: http.StatusNotFound, ContentType: "application/json", Body: out, Route: "none", Reason: "upstream is off; nothing answers " + req.Method + " " + path}
}
