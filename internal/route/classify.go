package route

import (
	"net/http"

	"github.com/risadams/janus/internal/anthropic"
)

// Class names what kind of request this is. The set comes from
// docs/research/traffic.md and will grow as more of Claude Code's traffic
// is catalogued.
type Class string

const (
	// Main is a turn of Claude Code's main conversation.
	Main Class = "main"
	// Subagent is a turn of a subagent's own conversation.
	Subagent Class = "subagent"
	// Connectivity is the client checking that the API is reachable.
	Connectivity Class = "connectivity"
	// Quota is Claude Code asking for one token at start-up to read its
	// rate limits from the response headers. It must reach Claude, and its
	// answer is often a 429 that is not a failure.
	Quota Class = "quota"
	// Unknown is everything else. Unknown requests are always passed
	// through untouched.
	Unknown Class = "unknown"
)

// Classify names a request from its method, path, and headers alone.
//
// Claude Code marks its requests with a session header, and a subagent's
// with an agent header as well. Headers are used in preference to anything
// in the body because they are cheap to read and less likely to change
// than prompt text.
func Classify(method, path string, header http.Header) Class {
	switch {
	case method == http.MethodPost && path == "/v1/messages":
		switch {
		case header.Get("X-Claude-Code-Agent-Id") != "":
			return Subagent
		case header.Get("X-Claude-Code-Session-Id") != "":
			return Main
		}
	case (method == http.MethodGet || method == http.MethodHead) && (path == "/" || path == "/api/hello"):
		return Connectivity
	}
	return Unknown
}

// ClassifyRequest is Classify for a Messages request whose body has been
// read, which separates the quota check from real turns: it carries the
// same headers as the main loop.
func ClassifyRequest(header http.Header, req *anthropic.Request) Class {
	class := Classify(http.MethodPost, "/v1/messages", header)
	if class == Main && quotaCheck(req) {
		return Quota
	}
	return class
}

// quotaCheck reports whether req is Claude Code's start-up quota check:
// one user message reading "quota", one token of output, no tools.
func quotaCheck(req *anthropic.Request) bool {
	if req.MaxTokens() != 1 || len(req.Tools()) > 0 || len(req.Messages()) != 1 {
		return false
	}
	m := req.Messages()[0]
	return m.Role == "user" && len(m.Blocks()) == 1 && m.Blocks()[0].Text() == "quota"
}
