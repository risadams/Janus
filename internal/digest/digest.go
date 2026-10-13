// Package digest is the transform that replaces an oversized tool result,
// on its way upstream, with a digest the local model writes. The original
// is kept, and Claude can fetch it, whole or in part, through the
// tool_output tool of janus mcp.
//
// Large tool results are few, but each is sent again on every later turn
// of its conversation: in the 2026-10-12 captures, the results over 2 KB
// were a fifth of the results and seven tenths of what tool results cost.
package digest

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/transform"
)

// Asker is the local model: worker.Model.
type Asker interface {
	Ask(ctx context.Context, system, user string, maxTokens int) (string, anthropic.Usage, error)
}

// Digest is the transform.
type Digest struct {
	// Model writes the digests. Its timeout is how long a turn may be
	// held up for one: past it the result goes as it is, and stays so.
	Model Asker
	// Store keeps the originals.
	Store *Store
	// MinBytes is the smallest result digested.
	MinBytes int
	// Tools, if set, limits digests to the results of these tools.
	Tools []string
	// Protected reports whether the local-only policy covers a block. A
	// digest could put protected content in words the policy no longer
	// recognises, so such a block is never digested.
	Protected func(req *anthropic.Request, message, block int, toolUseID string) bool
	// Logf, if set, is told why a result that qualified was not digested.
	Logf func(format string, args ...any)
}

// Name implements transform.Transform.
func (d *Digest) Name() string { return "digest" }

// retrieval finds the tool that fetches originals, by the name Claude Code
// gives a tool of an MCP server: mcp__<server>__tool_output. It may be in
// the tools list, added in a message, or only named as a deferred tool
// that Claude can load.
var retrieval = regexp.MustCompile(`mcp__[A-Za-z0-9_-]+?__tool_output`)

// retrievalTool finds that tool's name in a request. A digest's own note
// names it too, after "call ", and an earlier digest in the history is no
// sign that the tool is still there, so those mentions do not count.
func retrievalTool(body []byte) string {
	for _, at := range retrieval.FindAllIndex(body, -1) {
		if !strings.HasSuffix(string(body[max(0, at[0]-5):at[0]]), "call ") {
			return string(body[at[0]:at[1]])
		}
	}
	return ""
}

// Rewrite implements transform.Transform.
func (d *Digest) Rewrite(at transform.Position, b anthropic.Block) (anthropic.Block, bool) {
	if b.Type != "tool_result" {
		return b, false
	}
	var result anthropic.ToolResultBlock
	if b.Decode(&result) != nil || result.IsError {
		// An error is what the next turn is about; it goes as it is.
		return b, false
	}
	text, ok := plainText(result.Content)
	if !ok || len(text) < d.MinBytes {
		return b, false
	}
	tool, input := toolCall(at.Request, at.Message, result.ToolUseID)
	if len(d.Tools) > 0 && !slices.Contains(d.Tools, tool) {
		return b, false
	}
	if tool == "Bash" && printsFile(input) {
		// Claude printed a file to read it, most likely to edit it, and
		// needs its exact text.
		return b, false
	}
	if d.Protected != nil && d.Protected(at.Request, at.Message, at.Block, result.ToolUseID) {
		return b, false
	}
	// A digest promises the original, so there must be a way to fetch it.
	fetch := retrievalTool(at.Request.Bytes())
	if fetch == "" {
		d.logf("digest: a %d-byte %s result was not digested: the request offers no mcp__<server>__tool_output tool to fetch the original with; add janus mcp to Claude Code", len(text), tool)
		return b, false
	}

	words := targetWords(len(text))
	summary, _, err := d.Model.Ask(context.Background(), system(words), prompt(tool, text), words*3)
	if err != nil {
		d.logf("digest: a %d-byte %s result was sent as it is: %v", len(text), tool, err)
		return b, false
	}
	id, err := d.Store.Put(text)
	if err != nil {
		d.logf("digest: a %d-byte %s result was sent as it is: %v", len(text), tool, err)
		return b, false
	}

	lines := strings.Count(text, "\n") + 1
	note := fmt.Sprintf("[janus: a local model digested this %s output (%s bytes, %s lines) to save tokens. "+
		"For the full text, or a line range, or the lines matching a pattern, call %s with id %q.]",
		orTool(tool), commas(len(text)), commas(lines), fetch, id)
	content, _ := json.Marshal(note + "\n\n" + summary)
	return b.With("content", content), true
}

func (d *Digest) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

// targetWords scales the digest to the result: about a twentieth of its
// bytes, within limits that keep a digest both useful and short.
func targetWords(bytes int) int {
	return min(max(bytes/20, 80), 400)
}

func system(words int) string {
	return fmt.Sprintf(`You condense tool output for another AI assistant, which will act on your summary without seeing the output itself.

Keep, exactly as written: every error and warning, failing test names, file paths, line numbers, identifiers, version numbers, counts, exit codes, and the final status or result. Keep anything the assistant would need to decide its next step.

Drop repetition, progress output, and routine lines that report nothing went wrong. End with one line saying what kinds of lines you dropped.

Never add anything that is not in the output. Do not comment on it or suggest what to do. Use at most %d words.`, words)
}

func prompt(tool, text string) string {
	return fmt.Sprintf("The output of the %s tool:\n\n<output>\n%s\n</output>", orTool(tool), text)
}

// plainText is a tool result's content as text: a string, or a list of
// text blocks. Anything with an image or another kind of block is left
// alone.
func plainText(content json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s, true
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil || len(blocks) == 0 {
		return "", false
	}
	var parts []string
	for _, b := range blocks {
		if b.Type != "text" {
			return "", false
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n"), true
}

// filePrinter matches a program that prints a file given by a path-like
// argument (one with a dot or a slash): cat, sed -n, head, tail, nl, less,
// more, or bat, as the whole command or one step of it. The same program
// reading another command's output, as in "go test | head -40", has no
// such argument and does not match.
var filePrinter = regexp.MustCompile(`(?:^|[;&|(]|\bthen|\bdo)\s*(?:cat|sed\s+(?:-\w+\s+)*-n\w*|sed\s+-\w*n\w*|head|tail|nl|less|more|bat)\b(?:\s+(?:-[^\s;&|<>]+|'[^']*'|"[^"]*"|[^\s;&|<>./]+))*\s+[^\s;&|<>]*[./][^\s;&|<>]*`)

// printsFile reports whether a Bash call's command prints a file.
func printsFile(input json.RawMessage) bool {
	var call struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(input, &call) != nil {
		return false
	}
	return filePrinter.MatchString(call.Command)
}

// toolCall finds the call a result answers, in the messages before it:
// the tool's name and its input.
func toolCall(req *anthropic.Request, before int, id string) (string, json.RawMessage) {
	messages := req.Messages()
	for i := min(before, len(messages)) - 1; i >= 0; i-- {
		for _, b := range messages[i].Blocks() {
			if b.Type != "tool_use" {
				continue
			}
			var use anthropic.ToolUseBlock
			if b.Decode(&use) == nil && use.ID == id {
				return use.Name, use.Input
			}
		}
	}
	return "", nil
}

func orTool(name string) string {
	if name == "" {
		return "tool"
	}
	return name
}

func commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
