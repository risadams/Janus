package guard

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/risadams/janus/internal/anthropic"
)

// Report is what a scan of one request found. It says how many secrets,
// of what kinds, and whereabouts. It never holds a secret itself, so it
// is safe to log.
type Report struct {
	Findings int
	// Kinds counts findings by kind.
	Kinds map[string]int
	// Where lists the parts of the request that held at least one, for
	// example "messages[4] tool_result".
	Where []string
}

// Summary is the kinds found, most frequent first: "aws_access_key x2,
// assigned_secret".
func (r Report) Summary() string {
	kinds := make([]string, 0, len(r.Kinds))
	for k := range r.Kinds {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool {
		if r.Kinds[kinds[i]] != r.Kinds[kinds[j]] {
			return r.Kinds[kinds[i]] > r.Kinds[kinds[j]]
		}
		return kinds[i] < kinds[j]
	})
	for i, k := range kinds {
		if n := r.Kinds[k]; n > 1 {
			kinds[i] = fmt.Sprintf("%s x%d", k, n)
		}
	}
	return strings.Join(kinds, ", ")
}

// ScanRequest looks for secrets in everything a Messages request carries
// that came from the user's machine: the system prompt, message text, tool
// results, and the arguments of earlier tool calls.
//
// It leaves two things alone. Thinking blocks hold a signature, which is
// random by design and not a secret of the user's. Images are not text.
func (d *Detector) ScanRequest(req *anthropic.Request) Report {
	return d.secretsIn(req).report
}

// textOf calls visit with each part of a request that carries text from
// the user's machine, and where that part is.
func textOf(req *anthropic.Request, visit func(where, text string)) {
	for i, b := range req.System() {
		if b.Type == "text" {
			visit(fmt.Sprintf("system[%d]", i), b.Text())
		}
	}
	for i, m := range req.Messages() {
		var text strings.Builder
		kinds := map[string]bool{}
		for _, b := range m.Blocks() {
			before := text.Len()
			switch b.Type {
			case "text":
				text.WriteString(b.Text())
			case "tool_result":
				var result anthropic.ToolResultBlock
				if b.Decode(&result) == nil {
					if inner, err := anthropic.ParseContent(result.Content); err == nil {
						for _, ib := range inner {
							text.WriteString(ib.Text())
							text.WriteString("\n")
						}
					}
				}
			case "tool_use":
				var use anthropic.ToolUseBlock
				if b.Decode(&use) == nil {
					stringsIn(use.Input, &text)
				}
			}
			if text.Len() > before {
				kinds[b.Type] = true
				text.WriteString("\n")
			}
		}
		if text.Len() == 0 {
			continue
		}
		names := make([]string, 0, len(kinds))
		for k := range kinds {
			names = append(names, k)
		}
		sort.Strings(names)
		visit(fmt.Sprintf("messages[%d] %s (%s)", i, m.Role, strings.Join(names, ", ")), text.String())
	}
}

// stringsIn writes every string inside a JSON value to out, one per line,
// with the key before it for object members. Secrets in tool arguments sit
// in strings, and the key is often what identifies one: {"password": "..."}.
func stringsIn(raw json.RawMessage, out *strings.Builder) {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return
	}
	var walk func(key string, v any, depth int)
	walk = func(key string, v any, depth int) {
		if depth > 32 {
			return
		}
		switch x := v.(type) {
		case string:
			if key != "" {
				out.WriteString(key)
				out.WriteString(": ")
			}
			out.WriteString(x)
			out.WriteString("\n")
		case []any:
			for _, item := range x {
				walk(key, item, depth+1)
			}
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(k, x[k], depth+1)
			}
		}
	}
	walk("", v, 0)
}
