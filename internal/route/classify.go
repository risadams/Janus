package route

import (
	"net/http"
	"strings"

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
	// Compaction is Claude Code asking the model to summarise the
	// conversation so far, to replace it: the main loop's request with an
	// instruction added at the end, and no other turn following from it.
	Compaction Class = "compaction"
	// Status is Claude Code asking for a few words on what a subagent is
	// doing, for the line it shows while the subagent runs. It carries the
	// subagent's whole conversation for an answer of a handful of tokens,
	// so it is cheap from Claude's cache and slow from a local model.
	Status Class = "status"
	// TokenCount is a request to count a prompt's tokens without running
	// it. It is free, and only Claude can answer it.
	TokenCount Class = "token_count"
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
	case method == http.MethodPost && path == "/v1/messages/count_tokens":
		return TokenCount
	case (method == http.MethodGet || method == http.MethodHead) && (path == "/" || path == "/api/hello"):
		return Connectivity
	}
	return Unknown
}

// ClassifyRequest is Classify for a Messages request whose body has been
// read, which separates the quota check, compaction, and status summaries
// from real turns: they carry the same headers as the turns they belong
// to.
func ClassifyRequest(header http.Header, req *anthropic.Request) Class {
	class := Classify(http.MethodPost, "/v1/messages", header)
	switch {
	case class == Subagent || class == Main:
		if statusSummary(req) {
			return Status
		}
	}
	switch {
	case class != Main:
	case quotaCheck(req):
		return Quota
	case compaction(req):
		return Compaction
	}
	return class
}

// compactionAsk is the sentence Claude Code's compaction instruction is
// built around (2.1.296). It is prompt text, so a new release could word
// it differently; the class would then fall back to main.
const compactionAsk = "Your task is to create a detailed summary of the conversation so far"

// compaction reports whether req ends with Claude Code's instruction to
// summarise the conversation. It is the last block of the last message,
// after whatever tool results that message carries.
func compaction(req *anthropic.Request) bool {
	messages := req.Messages()
	if len(messages) == 0 {
		return false
	}
	m := messages[len(messages)-1]
	blocks := m.Blocks()
	return m.Role == "user" && len(blocks) > 0 && strings.Contains(blocks[len(blocks)-1].Text(), compactionAsk)
}

// statusAsk begins Claude Code's request for a status line (2.1.296). It is
// prompt text, so a new release could word it differently; the class would
// then fall back to the turn's own.
const statusAsk = "Describe your most recent action in "

// statusSummary reports whether req asks for a status line: the last user
// message ends in a text block that begins with Claude Code's instruction.
// System messages may follow that user message. The instruction quoted
// anywhere else, such as in a tool result, does not count.
func statusSummary(req *anthropic.Request) bool {
	messages := req.Messages()
	for i := len(messages) - 1; i >= 0; i-- {
		switch messages[i].Role {
		case "system":
			continue
		case "user":
			var last string
			for _, b := range messages[i].Blocks() {
				if b.Type == "text" {
					last = b.Text()
				}
			}
			return strings.HasPrefix(strings.TrimSpace(last), statusAsk)
		}
		return false
	}
	return false
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
