package route

import "net/http"

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
