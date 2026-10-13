package guard

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/risadams/janus/internal/anthropic"
)

// Policy marks some content as local-only: it must not go to Anthropic,
// whatever it contains. Where the detectors look for things shaped like
// secrets, a policy goes by where content came from and what it says it
// is.
//
// Content is local-only if it is the result of a tool call that named a
// protected path, or if it carries a protected marker. Separately, a line
// of some other tool result that quotes a protected file, as a search
// across the tree does, is local-only by itself.
type Policy struct {
	paths   []pathRule
	markers []string
}

type pathRule struct {
	glob string
	re   *regexp.Regexp
}

// NewPolicy builds a policy from path globs and marker strings.
//
// A glob is matched against the end of a path, so "*.pem" protects any
// file with that extension wherever it is, ".env*" any file whose name
// starts that way, and "secrets/**" everything under any directory called
// secrets. "*" stays within one path component; "**" crosses them.
func NewPolicy(paths, markers []string) (*Policy, error) {
	p := &Policy{}
	for _, glob := range paths {
		re, err := compileGlob(glob)
		if err != nil {
			return nil, err
		}
		p.paths = append(p.paths, pathRule{glob, re})
	}
	for _, m := range markers {
		if strings.TrimSpace(m) == "" {
			return nil, fmt.Errorf("a local-only marker is empty")
		}
		p.markers = append(p.markers, m)
	}
	return p, nil
}

func compileGlob(glob string) (*regexp.Regexp, error) {
	g := strings.TrimPrefix(strings.TrimSpace(glob), "./")
	if g == "" || strings.Trim(g, "*/") == "" {
		return nil, fmt.Errorf("local-only path %q would match everything; name a file or directory", glob)
	}
	var b strings.Builder
	b.WriteString(`(?:^|/)`)
	for i := 0; i < len(g); i++ {
		switch c := g[i]; {
		case c == '*' && i+1 < len(g) && g[i+1] == '*':
			b.WriteString(`.*`)
			i++
		case c == '*':
			b.WriteString(`[^/]*`)
		case c == '?':
			b.WriteString(`[^/]`)
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	// A pattern naming a directory protects what is in it.
	if !strings.HasSuffix(g, "*") {
		b.WriteString(`(?:/.*)?`)
	}
	b.WriteString(`$`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("local-only path %q is not a usable pattern: %v", glob, err)
	}
	return re, nil
}

// matchPath reports the glob that protects path, if one does.
func (p *Policy) matchPath(path string) (glob string, ok bool) {
	path = strings.TrimPrefix(path, "./")
	for _, rule := range p.paths {
		if rule.re.MatchString(path) {
			return rule.glob, true
		}
	}
	return "", false
}

// Match is what a policy found in one request.
type Match struct {
	// Reasons says why, in words fit for a log or an error message. It
	// names paths and markers, never content.
	Reasons []string

	// Quoted says which protected files other tool results quote lines
	// of. Those lines are withheld from anything sent upstream, but they
	// do not make the request as a whole local-only: a search that happens
	// to touch a protected file is not a conversation about it.
	Quoted []string

	results map[string]string // tool_use id -> reason its result is protected
	texts   map[textRef]string
	lines   map[textRef]bool // tool results with lines quoting a protected file
}

type textRef struct{ message, block int }

// Protected reports whether the request holds local-only content.
func (m Match) Protected() bool { return len(m.results) > 0 || len(m.texts) > 0 }

// Covers reports whether one block of the request holds content the policy
// protects: the result of a call that named a protected path, a text with
// a marker, or a tool result quoting a protected file.
func (m Match) Covers(message, block int, toolUseID string) bool {
	ref := textRef{message, block}
	return (toolUseID != "" && m.results[toolUseID] != "") || m.texts[ref] != "" || m.lines[ref]
}

// Any reports whether there is anything at all to keep from the upstream.
func (m Match) Any() bool { return m.Protected() || len(m.lines) > 0 }

// Check finds the local-only content in a request.
func (p *Policy) Check(req *anthropic.Request) Match {
	m := Match{results: map[string]string{}, texts: map[textRef]string{}, lines: map[textRef]bool{}}
	if p == nil || (len(p.paths) == 0 && len(p.markers) == 0) {
		return m
	}
	seen := map[string]bool{}
	add := func(reason string) {
		if !seen[reason] {
			seen[reason] = true
			m.Reasons = append(m.Reasons, reason)
		}
	}

	messages := req.Messages()
	// First, the tool calls that named a protected path. Whatever such a
	// call returned is protected, whether or not the path is mentioned
	// again in the result.
	for _, msg := range messages {
		if msg.Role != "assistant" {
			continue
		}
		for _, b := range msg.Blocks() {
			if b.Type != "tool_use" {
				continue
			}
			var use anthropic.ToolUseBlock
			if b.Decode(&use) != nil || use.ID == "" {
				continue
			}
			for _, candidate := range pathsIn(use.Input) {
				if glob, ok := p.matchPath(candidate); ok {
					reason := fmt.Sprintf("%s read %s, which %q makes local-only", use.Name, candidate, glob)
					m.results[use.ID] = reason
					add(reason)
					break
				}
			}
		}
	}
	// Then, anything carrying a marker.
	quoted := map[string]bool{}
	for i, msg := range messages {
		for j, b := range msg.Blocks() {
			var text string
			var content json.RawMessage
			switch b.Type {
			case "text":
				text = b.Text()
			case "tool_result":
				var result anthropic.ToolResultBlock
				if b.Decode(&result) != nil {
					continue
				}
				if _, already := m.results[result.ToolUseID]; already {
					continue
				}
				content = result.Content
				if inner, err := anthropic.ParseContent(result.Content); err == nil {
					for _, ib := range inner {
						text += ib.Text() + "\n"
					}
				}
			default:
				continue
			}
			for _, marker := range p.markers {
				if strings.Contains(text, marker) {
					reason := fmt.Sprintf("messages[%d] carries the marker %q", i, marker)
					m.texts[textRef{i, j}] = reason
					add(reason)
					break
				}
			}
			if _, whole := m.texts[textRef{i, j}]; whole || content == nil {
				continue
			}
			mapResultText(content, func(text string) string {
				out, found := p.withholdLines(text)
				for _, f := range found {
					m.lines[textRef{i, j}] = true
					if reason := fmt.Sprintf("a tool result quotes a file that %q makes local-only", f[1]); !quoted[reason] {
						quoted[reason] = true
						m.Quoted = append(m.Quoted, reason)
					}
				}
				return out
			})
		}
	}
	sort.Strings(m.Reasons)
	sort.Strings(m.Quoted)
	return m
}

// pathsIn pulls everything that could be a path out of a tool call's
// arguments: each string, and each word of a string, so that the file in
// {"file_path": "secrets/db.yaml"} and the one in {"command": "cat
// secrets/db.yaml | head"} are both found.
func pathsIn(input json.RawMessage) []string {
	var v any
	if json.Unmarshal(input, &v) != nil {
		return nil
	}
	var out []string
	var walk func(v any, depth int)
	walk = func(v any, depth int) {
		if depth > 32 {
			return
		}
		switch x := v.(type) {
		case string:
			out = append(out, x)
			for _, word := range strings.FieldsFunc(x, func(r rune) bool {
				return r == ' ' || r == '\t' || r == '\n' || strings.ContainsRune(`"'`+"`"+`;|&()<>=,`, r)
			}) {
				if word != x {
					out = append(out, word)
				}
			}
		case []any:
			for _, item := range x {
				walk(item, depth+1)
			}
		case map[string]any:
			for _, item := range x {
				walk(item, depth+1)
			}
		}
	}
	walk(v, 0)
	return out
}

// withheldNotice replaces local-only content in what goes to Anthropic. It
// tells the model plainly what happened, so that it does not guess at the
// content or try to read it again.
func withheldNotice(reason string) string {
	return "[Withheld by janus: this content is local-only and was not sent. " + reason + ". " +
		"Do not ask for it again or guess at it; tell the user that this content is kept on their machine.]"
}

// lineNotice replaces one line of a tool result that quoted a local-only
// file.
//
// It names the pattern and not the path, as does the reason given for it:
// what stands where a path would be in a line of search output is not
// always only a path.
func lineNotice(glob string) string {
	return fmt.Sprintf("[janus withheld this line: it quotes a file that %q makes local-only]", glob)
}

// withholdLines replaces the lines of text that quote a protected file, in
// the form search tools print them: the path, a colon, and the line. It
// returns the text and the {path, glob} pairs it found. Lines of context
// around a match, which are printed with a dash in place of the colon, go
// too.
//
// A line that only names a protected file, as a directory listing does, is
// left alone: the name is not the content.
func (p *Policy) withholdLines(text string) (string, [][2]string) {
	if len(p.paths) == 0 || !strings.Contains(text, ":") {
		return text, nil
	}
	lines := strings.Split(text, "\n")
	globs := map[string]string{}
	var found [][2]string
	for _, line := range lines {
		head, rest, ok := strings.Cut(line, ":")
		head = strings.TrimSpace(head)
		if !ok || head == "" || strings.TrimSpace(rest) == "" || strings.ContainsAny(head, " \t") {
			continue
		}
		if _, seen := globs[head]; seen {
			continue
		}
		if glob, ok := p.matchPath(head); ok {
			globs[head] = glob
			found = append(found, [2]string{head, glob})
		}
	}
	if len(found) == 0 {
		return text, nil
	}
	// Longest first, so that a line is attributed the same way every time.
	sort.Slice(found, func(i, j int) bool {
		if len(found[i][0]) != len(found[j][0]) {
			return len(found[i][0]) > len(found[j][0])
		}
		return found[i][0] < found[j][0]
	})
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		for _, f := range found {
			if strings.HasPrefix(trimmed, f[0]+":") || strings.HasPrefix(trimmed, f[0]+"-") {
				lines[i] = lineNotice(f[1])
				break
			}
		}
	}
	return strings.Join(lines, "\n"), found
}

// mapResultText applies fn to each piece of text in a tool result's
// content, which is a string or an array of blocks, and returns the content
// with the changes and whether there were any.
func mapResultText(content json.RawMessage, fn func(string) string) (json.RawMessage, bool) {
	var whole string
	if json.Unmarshal(content, &whole) == nil {
		changed := fn(whole)
		if changed == whole {
			return content, false
		}
		out, _ := json.Marshal(changed)
		return out, true
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(content, &items) != nil {
		return content, false
	}
	edited := false
	for _, item := range items {
		var kind, text string
		if json.Unmarshal(item["type"], &kind) != nil || kind != "text" || json.Unmarshal(item["text"], &text) != nil {
			continue
		}
		if changed := fn(text); changed != text {
			item["text"], _ = json.Marshal(changed)
			edited = true
		}
	}
	if !edited {
		return content, false
	}
	out, _ := json.Marshal(items)
	return out, true
}

// withMember returns the block with one member replaced and the rest, such
// as a cache breakpoint, as they were.
func withMember(b anthropic.Block, key string, value any) (anthropic.Block, error) {
	var members map[string]json.RawMessage
	if err := b.Decode(&members); err != nil {
		return b, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return b, err
	}
	members[key] = raw
	out, err := json.Marshal(members)
	if err != nil {
		return b, err
	}
	return anthropic.NewBlock(out)
}

// Withhold replaces the local-only content in req with a notice, and
// reports how many pieces it replaced. Everything else is left as it was,
// and the same request always comes out the same, so a conversation's
// earlier turns stay byte-stable from one request to the next.
func (p *Policy) Withhold(req *anthropic.Request, m Match) (withheld int) {
	withheld, _ = p.withhold(req, m, true)
	return withheld
}

// Remaining reports how many pieces of local-only content are in req as it
// stands: zero for a request Withhold has been over. It is the check made
// before anything is sent upstream.
func (p *Policy) Remaining(req *anthropic.Request) int {
	pending, failed := p.withhold(req, p.Check(req), false)
	return pending + failed
}

// withhold finds each protected piece that is not already a notice and,
// if replace is set, makes it one. It returns how many it found, and how
// many of the protected pieces it could not deal with.
func (p *Policy) withhold(req *anthropic.Request, m Match, replace bool) (found, failed int) {
	if !m.Any() {
		return 0, 0
	}
	messages := req.Messages()
	for i, msg := range messages {
		blocks := append([]anthropic.Block(nil), msg.Blocks()...)
		changed := false
		for j, b := range blocks {
			var reason string
			switch b.Type {
			case "tool_result":
				var result anthropic.ToolResultBlock
				if b.Decode(&result) != nil {
					// Not understood, so it cannot be shown to be free of
					// protected content.
					failed++
					continue
				}
				reason = m.results[result.ToolUseID]
				if reason == "" {
					reason = m.texts[textRef{i, j}]
				}
				var content any = withheldNotice(reason)
				if reason == "" {
					if !m.lines[textRef{i, j}] {
						continue
					}
					// Only some lines of this one are protected.
					without, changed := mapResultText(result.Content, func(text string) string {
						out, _ := p.withholdLines(text)
						return out
					})
					if !changed {
						failed++
						continue
					}
					content = without
				}
				var already string
				if json.Unmarshal(result.Content, &already) == nil && already == content {
					continue
				}
				replacement, err := withMember(b, "content", content)
				if err != nil {
					failed++
					continue
				}
				blocks[j] = replacement
			case "text":
				reason = m.texts[textRef{i, j}]
				if reason == "" || b.Text() == withheldNotice(reason) {
					continue
				}
				replacement, err := withMember(b, "text", withheldNotice(reason))
				if err != nil {
					failed++
					continue
				}
				blocks[j] = replacement
			default:
				continue
			}
			changed = true
			found++
		}
		if changed && replace {
			messages[i] = msg.WithBlocks(blocks)
		}
	}
	if found > 0 && replace {
		req.SetMessages(messages)
	}
	return found, failed
}
