package digest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/risadams/janus/internal/mcp"
)

// toolOutputLimit bounds one answer of the tool, so that fetching an
// original cannot put more in Claude's context than it asked for by
// accident. A larger original is read a range at a time.
const toolOutputLimit = 60_000

// Tool is the tool_output tool for janus mcp: it returns the original of a
// digested tool result, whole or in part.
func Tool(store *Store) mcp.Tool {
	return mcp.Tool{
		Name: "tool_output",
		Description: "Fetch the original output of a tool result that janus replaced with a digest. " +
			"A digest begins \"[janus: a local model digested this ... output\" and gives an id. " +
			"Ask for the lines you need: a range, or the lines matching a pattern. " +
			"With neither you get the whole output, up to about 60 kB; past that, ask for a range.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string", "description": "The id the digest gives."},
				"from_line": {"type": "integer", "description": "First line to return, counting from 1."},
				"to_line": {"type": "integer", "description": "Last line to return."},
				"pattern": {"type": "string", "description": "A regular expression (Go syntax); only the lines matching it are returned, with their line numbers."}
			},
			"required": ["id"]
		}`),
		Call: func(ctx context.Context, arguments json.RawMessage) (string, bool) {
			var args struct {
				ID      string `json:"id"`
				From    int    `json:"from_line"`
				To      int    `json:"to_line"`
				Pattern string `json:"pattern"`
			}
			if err := json.Unmarshal(arguments, &args); err != nil {
				return "the arguments are not valid: " + err.Error(), true
			}
			text, err := store.Get(args.ID)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					return err.Error() + ". Run the original tool again if you still need its output.", true
				}
				return err.Error(), true
			}
			return excerpt(text, args.From, args.To, args.Pattern)
		},
	}
}

// excerpt picks lines out of an original, numbered as in the original.
func excerpt(text string, from, to int, pattern string) (string, bool) {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if from <= 0 {
		from = 1
	}
	if to <= 0 || to > len(lines) {
		to = len(lines)
	}
	if from > to {
		return fmt.Sprintf("from_line %d is past the end; the output has %d lines", from, len(lines)), true
	}
	var re *regexp.Regexp
	if pattern != "" {
		var err error
		if re, err = regexp.Compile(pattern); err != nil {
			return "the pattern is not a valid regular expression: " + err.Error(), true
		}
	}

	var b strings.Builder
	shown, matched := 0, 0
	for n := from; n <= to; n++ {
		line := lines[n-1]
		if re != nil && !re.MatchString(line) {
			continue
		}
		matched++
		entry := fmt.Sprintf("%6d\t%s\n", n, line)
		if b.Len()+len(entry) > toolOutputLimit {
			fmt.Fprintf(&b, "[cut short at line %d to keep this answer under %d kB; ask for from_line %d onward, or a pattern]\n", n-1, toolOutputLimit/1000, n)
			return b.String(), false
		}
		b.WriteString(entry)
		shown++
	}
	if re != nil && matched == 0 {
		return fmt.Sprintf("no line between %d and %d matches %q; the output has %d lines", from, to, pattern, len(lines)), false
	}
	return b.String(), false
}
