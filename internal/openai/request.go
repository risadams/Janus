package openai

import (
	"bytes"
	"crypto/sha1" // #nosec G505 -- a name suffix, as the Codex CLI makes it; nothing rests on it being hard to forge
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/risadams/janus/internal/anthropic"
)

// ErrUntranslatable is wrapped by TranslateRequest's error for a request
// OpenAI cannot be given: one with a Files API id, an Anthropic server
// tool, or a block janus does not know. Such a request skips OpenAI
// (docs/research/openai.md, What has no equivalent).
var ErrUntranslatable = errors.New("openai: the request cannot be translated")

// MaxOutputTokens is the most any candidate model writes in one response,
// reasoning included (docs/research/openai.md, Top level).
const MaxOutputTokens = 128000

// RequestOptions are what the translation needs besides the request.
type RequestOptions struct {
	// Model is the OpenAI model to ask (#374).
	Model string
	// Effort is reasoning.effort, already clamped to what Model accepts
	// (#374). Empty leaves it to the model.
	Effort string
	// SessionID is Claude Code's X-Claude-Code-Session-Id, hashed into
	// prompt_cache_key. Empty sends no key.
	SessionID string
}

// Translated is a Messages request as a Responses request.
type Translated struct {
	// Body is the Responses request.
	Body []byte
	// Tools turns the tool names in OpenAI's answer back into Claude Code's.
	Tools ToolNames
	// Dropped names what the request asked for that OpenAI has no way to
	// honour, such as stop_sequences, for the ledger.
	Dropped []string
}

// TranslateRequest turns Claude Code's Messages request into a Responses
// request, as docs/research/openai.md decided.
//
// The same history always becomes the same bytes: the output is built
// from structs in a fixed order, never from maps, and every rule depends
// only on the block it applies to. A later turn, which repeats the earlier
// turns and adds to them, therefore translates to the same prefix, and
// OpenAI's prompt cache holds as Claude's does. TestLaterTurnsKeepThePrefix
// is the guard.
//
// Every request is sent with store false, truncation disabled, and no
// previous_response_id, so OpenAI keeps nothing between requests and
// never drops the start of a conversation.
func TranslateRequest(req *anthropic.Request, opts RequestOptions) (*Translated, error) {
	t := &translator{byName: map[string]anthropic.Tool{}}
	if err := t.collectTools(req); err != nil {
		return nil, err
	}
	b := body{
		Model:      opts.Model,
		Stream:     req.Stream(),
		Truncation: "disabled",
		Include:    []string{"reasoning.encrypted_content"},
	}
	if err := t.system(req.System()); err != nil {
		return nil, err
	}
	for i, m := range req.Messages() {
		if err := t.message(m); err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
	}
	b.Input = t.items
	if b.Input == nil {
		b.Input = []*item{}
	}
	b.Tools = t.tools
	if err := t.topLevel(req, &b, opts); err != nil {
		return nil, err
	}
	if t.breakpoints {
		b.PromptCacheOptions = &cacheOptions{Mode: "explicit"}
	}
	if opts.SessionID != "" {
		sum := sha256.Sum256([]byte(opts.SessionID))
		b.PromptCacheKey = hex.EncodeToString(sum[:])[:32]
	}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	// Text goes as it is: escaping <, >, and & would only make it longer.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(b); err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	return &Translated{Body: bytes.TrimSuffix(out.Bytes(), []byte("\n")), Tools: t.names, Dropped: t.dropped}, nil
}

// The Responses request, in the order its fields are written.
type (
	body struct {
		Model              string        `json:"model"`
		Input              []*item       `json:"input"`
		Tools              []*function   `json:"tools,omitempty"`
		ToolChoice         any           `json:"tool_choice,omitempty"`
		ParallelToolCalls  *bool         `json:"parallel_tool_calls,omitempty"`
		Reasoning          *reasoning    `json:"reasoning,omitempty"`
		Text               *textConfig   `json:"text,omitempty"`
		MaxOutputTokens    int           `json:"max_output_tokens,omitempty"`
		Stream             bool          `json:"stream"`
		Store              bool          `json:"store"`
		Truncation         string        `json:"truncation"`
		Include            []string      `json:"include"`
		PromptCacheKey     string        `json:"prompt_cache_key,omitempty"`
		PromptCacheOptions *cacheOptions `json:"prompt_cache_options,omitempty"`
	}

	// item is one entry of input: a message, a function call or its
	// output, or tools added in the middle of the conversation.
	item struct {
		Type      string      `json:"type"`
		Role      string      `json:"role,omitempty"`
		Content   []*part     `json:"content,omitempty"`
		CallID    string      `json:"call_id,omitempty"`
		Name      string      `json:"name,omitempty"`
		Arguments *string     `json:"arguments,omitempty"`
		Output    []*part     `json:"output,omitempty"`
		Tools     []*function `json:"tools,omitempty"`
	}

	part struct {
		Type       string      `json:"type"`
		Text       *string     `json:"text,omitempty"`
		ImageURL   string      `json:"image_url,omitempty"`
		Filename   string      `json:"filename,omitempty"`
		FileData   string      `json:"file_data,omitempty"`
		FileURL    string      `json:"file_url,omitempty"`
		Breakpoint *breakpoint `json:"prompt_cache_breakpoint,omitempty"`
	}

	// breakpoint carries no ttl: the default lifetime is 30 minutes, the
	// only one the reference allows, and both of Claude's lifetimes map to
	// it (docs/research/openai.md, Prompt cache; #399).
	breakpoint struct {
		Mode string `json:"mode"`
	}

	cacheOptions struct {
		Mode string `json:"mode"`
	}

	function struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      bool            `json:"strict"`
	}

	reasoning struct {
		Effort  string `json:"effort,omitempty"`
		Summary string `json:"summary,omitempty"`
	}

	textConfig struct {
		Format jsonSchemaFormat `json:"format"`
	}

	jsonSchemaFormat struct {
		Type   string          `json:"type"`
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
		Strict bool            `json:"strict"`
	}

	namedChoice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
)

// translator builds the input items in order.
type translator struct {
	items []*item
	tools []*function
	// byName holds every tool the request defines, deferred ones and
	// those added by tool_addition blocks included, for tool_reference.
	byName map[string]anthropic.Tool
	names  ToolNames
	// holder is the latest part that can carry a breakpoint. A
	// cache_control on a block that becomes nothing that can carry one, or
	// nothing at all, moves here, the nearest earlier block that is kept.
	holder      *part
	breakpoints bool
	dropped     []string
}

// collectTools encodes the name of every tool the request mentions, and
// translates the tools array.
func (t *translator) collectTools(req *anthropic.Request) error {
	t.names = ToolNames{}
	for _, tool := range req.Tools() {
		if v, ok := tool.Field("type"); ok && string(v) != `"custom"` {
			return fmt.Errorf("%w: tool %q is an Anthropic tool (type %s)", ErrUntranslatable, tool.Name, v)
		}
		if err := t.names.add(tool.Name); err != nil {
			return err
		}
		t.byName[tool.Name] = tool
		if v, ok := tool.Field("defer_loading"); ok && string(v) == "true" {
			continue // added where a tool search loads it
		}
		f, err := t.function(tool)
		if err != nil {
			return err
		}
		t.tools = append(t.tools, f)
	}
	for _, m := range req.Messages() {
		for _, b := range m.Blocks() {
			var name string
			switch b.Type {
			case "tool_use":
				var u anthropic.ToolUseBlock
				_ = b.Decode(&u) // undecodable is refused when the block is translated
				name = u.Name
			case "tool_addition":
				tool, err := additionTool(b)
				if err != nil {
					return err
				}
				t.byName[tool.Name] = tool
				name = tool.Name
			default:
				continue
			}
			if err := t.names.add(name); err != nil {
				return err
			}
		}
	}
	return nil
}

func additionTool(b anthropic.Block) (anthropic.Tool, error) {
	var added struct {
		Tool struct {
			Definition json.RawMessage `json:"definition"`
		} `json:"tool"`
	}
	if err := b.Decode(&added); err != nil || len(added.Tool.Definition) == 0 {
		return anthropic.Tool{}, fmt.Errorf("%w: a tool_addition block has no tool definition", ErrUntranslatable)
	}
	tool, err := anthropic.NewTool(added.Tool.Definition)
	if err != nil {
		return anthropic.Tool{}, fmt.Errorf("%w: tool_addition: %v", ErrUntranslatable, err)
	}
	return tool, nil
}

// function translates one tool. strict is always false: strict mode would
// make every optional field required, and left unset OpenAI may convert
// the schema itself (docs/research/openai.md, Tool names and schemas).
func (t *translator) function(tool anthropic.Tool) (*function, error) {
	f := &function{Type: "function", Name: encodeToolName(tool.Name)}
	if v, ok := tool.Field("description"); ok {
		if err := json.Unmarshal(v, &f.Description); err != nil {
			return nil, fmt.Errorf("%w: tool %q: description: %v", ErrUntranslatable, tool.Name, err)
		}
	}
	f.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
	if v, ok := tool.Field("input_schema"); ok {
		var compact bytes.Buffer
		if err := json.Compact(&compact, v); err != nil {
			return nil, fmt.Errorf("%w: tool %q: input_schema: %v", ErrUntranslatable, tool.Name, err)
		}
		f.Parameters = compact.Bytes()
	}
	return f, nil
}

// billingHeader starts the system block Claude Code adds for Anthropic's
// billing. It means nothing to OpenAI and is left out.
const billingHeader = "x-anthropic-billing-header:"

// system makes the system blocks a leading developer message, rather than
// instructions, where a breakpoint cannot go.
func (t *translator) system(blocks []anthropic.Block) error {
	var parts []*part
	for i, b := range blocks {
		if i == 0 && b.Type == "text" && strings.HasPrefix(b.Text(), billingHeader) {
			t.mark(b, nil)
			continue
		}
		if b.Type != "text" {
			return fmt.Errorf("%w: system[%d] is a %q block", ErrUntranslatable, i, b.Type)
		}
		p := t.text("input_text", b.Text())
		t.mark(b, p)
		parts = append(parts, p)
	}
	t.say("developer", parts)
	return nil
}

// message translates one Anthropic message into its items.
func (t *translator) message(m anthropic.Message) error {
	switch m.Role {
	case "assistant":
		return t.assistant(m.Blocks())
	case "user", "system":
		return t.user(m)
	default:
		return fmt.Errorf("%w: role %q", ErrUntranslatable, m.Role)
	}
}

// user translates a user message, or a system message in the middle of
// the conversation, which becomes a developer message in the same place.
// Tool results come first, as function outputs, since outputs must follow
// their calls; anything beside them follows as a message, broken where a
// tool_addition puts tools in.
func (t *translator) user(m anthropic.Message) error {
	role := m.Role
	if role == "system" {
		role = "developer"
	}
	blocks := m.Blocks()
	for _, b := range blocks {
		if b.Type == "tool_result" {
			if err := t.toolResult(b); err != nil {
				return err
			}
		}
	}
	var parts []*part
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
		case "tool_addition":
			t.say(role, parts)
			parts = nil
			tool, err := additionTool(b)
			if err != nil {
				return err
			}
			f, err := t.function(tool)
			if err != nil {
				return err
			}
			t.items = append(t.items, &item{Type: "additional_tools", Role: "developer", Tools: []*function{f}})
			t.mark(b, nil)
		default:
			added, err := t.content(b)
			if err != nil {
				return err
			}
			parts = append(parts, added...)
		}
	}
	t.say(role, parts)
	return nil
}

// assistant translates Claude's turn: text becomes an assistant message,
// each tool call a function_call, in order. Claude's thinking is signed
// for Anthropic and left out, the same way on every turn.
func (t *translator) assistant(blocks []anthropic.Block) error {
	var parts []*part
	for i, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, t.text("output_text", b.Text()))
			t.mark(b, nil) // output_text cannot carry a breakpoint
		case "thinking", "redacted_thinking":
			t.mark(b, nil)
		case "tool_use":
			t.say("assistant", parts)
			parts = nil
			var u anthropic.ToolUseBlock
			if err := b.Decode(&u); err != nil {
				return fmt.Errorf("%w: content[%d]: tool_use: %v", ErrUntranslatable, i, err)
			}
			args := "{}"
			if len(u.Input) > 0 {
				var compact bytes.Buffer
				if err := json.Compact(&compact, u.Input); err != nil {
					return fmt.Errorf("%w: content[%d]: tool_use input: %v", ErrUntranslatable, i, err)
				}
				args = compact.String()
			}
			t.items = append(t.items, &item{Type: "function_call", CallID: u.ID, Name: encodeToolName(u.Name), Arguments: &args})
			t.mark(b, nil)
		default:
			return fmt.Errorf("%w: content[%d] is a %q block", ErrUntranslatable, i, b.Type)
		}
	}
	t.say("assistant", parts)
	return nil
}

// toolResult makes a function_call_output. Its output is always an array
// of parts, never a string, so that it can carry a breakpoint without its
// shape depending on where Claude Code's moving mark is that turn.
func (t *translator) toolResult(b anthropic.Block) error {
	var r anthropic.ToolResultBlock
	if err := b.Decode(&r); err != nil {
		return fmt.Errorf("%w: tool_result: %v", ErrUntranslatable, err)
	}
	var content []anthropic.Block
	if len(r.Content) > 0 && string(r.Content) != "null" {
		var err error
		if content, err = anthropic.ParseContent(r.Content); err != nil {
			return fmt.Errorf("%w: tool_result content: %v", ErrUntranslatable, err)
		}
	}
	var output []*part
	var referenced []*function
	for _, c := range content {
		if c.Type == "tool_reference" {
			var ref struct {
				ToolName string `json:"tool_name"`
			}
			_ = c.Decode(&ref) // a reference that does not decode names no tool
			tool, ok := t.byName[ref.ToolName]
			text := fmt.Sprintf("Tool %s is loaded and can be called.", ref.ToolName)
			if !ok {
				text = fmt.Sprintf("Tool %s was found, but it cannot be called here.", ref.ToolName)
			} else {
				f, err := t.function(tool)
				if err != nil {
					return err
				}
				referenced = append(referenced, f)
			}
			output = append(output, t.text("input_text", text))
			continue
		}
		added, err := t.content(c)
		if err != nil {
			return err
		}
		output = append(output, added...)
	}
	if r.IsError {
		// OpenAI has no error flag on a function's output.
		if len(output) > 0 && output[0].Type == "input_text" {
			text := "Error: " + *output[0].Text
			output[0].Text = &text
		} else {
			output = append([]*part{t.text("input_text", "Error:")}, output...)
		}
	}
	if len(output) == 0 {
		output = []*part{t.text("input_text", "")}
	}
	t.holder = output[len(output)-1]
	t.mark(b, nil)
	t.items = append(t.items, &item{Type: "function_call_output", CallID: r.ToolUseID, Output: output})
	if len(referenced) > 0 {
		t.items = append(t.items, &item{Type: "additional_tools", Role: "developer", Tools: referenced})
	}
	return nil
}

// content translates a block of user content, or of a tool's result.
func (t *translator) content(b anthropic.Block) ([]*part, error) {
	var parts []*part
	switch b.Type {
	case "text":
		parts = []*part{t.text("input_text", b.Text())}
	case "image":
		p, err := image(b)
		if err != nil {
			return nil, err
		}
		parts = []*part{p}
	case "document":
		var err error
		if parts, err = t.document(b); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: a %q block", ErrUntranslatable, b.Type)
	}
	for _, p := range parts {
		t.holder = p
	}
	t.mark(b, nil)
	return parts, nil
}

type source struct {
	Type      string          `json:"type"`
	MediaType string          `json:"media_type"`
	Data      string          `json:"data"`
	URL       string          `json:"url"`
	Content   json.RawMessage `json:"content"`
}

func blockSource(b anthropic.Block) (source, error) {
	var s struct {
		Source source `json:"source"`
	}
	if err := b.Decode(&s); err != nil {
		return source{}, fmt.Errorf("%w: %s source: %v", ErrUntranslatable, b.Type, err)
	}
	if s.Source.Type == "file" {
		return source{}, fmt.Errorf("%w: a %s from Anthropic's Files API", ErrUntranslatable, b.Type)
	}
	return s.Source, nil
}

func image(b anthropic.Block) (*part, error) {
	s, err := blockSource(b)
	if err != nil {
		return nil, err
	}
	switch s.Type {
	case "base64":
		return &part{Type: "input_image", ImageURL: "data:" + s.MediaType + ";base64," + s.Data}, nil
	case "url":
		return &part{Type: "input_image", ImageURL: s.URL}, nil
	default:
		return nil, fmt.Errorf("%w: an image with a %q source", ErrUntranslatable, s.Type)
	}
}

// document translates a document block. Its title, context, and citations
// have no equivalent and are left out.
func (t *translator) document(b anthropic.Block) ([]*part, error) {
	s, err := blockSource(b)
	if err != nil {
		return nil, err
	}
	switch {
	case s.Type == "base64" && s.MediaType == "application/pdf":
		return []*part{{Type: "input_file", Filename: "document.pdf", FileData: "data:application/pdf;base64," + s.Data}}, nil
	case s.Type == "url":
		return []*part{{Type: "input_file", FileURL: s.URL}}, nil
	case s.Type == "text":
		return []*part{t.text("input_text", s.Data)}, nil
	case s.Type == "content":
		blocks, err := anthropic.ParseContent(s.Content)
		if err != nil {
			return nil, fmt.Errorf("%w: document content: %v", ErrUntranslatable, err)
		}
		var parts []*part
		for _, c := range blocks {
			added, err := t.content(c)
			if err != nil {
				return nil, err
			}
			parts = append(parts, added...)
		}
		return parts, nil
	default:
		return nil, fmt.Errorf("%w: a document with a %q source of %q", ErrUntranslatable, s.Type, s.MediaType)
	}
}

func (t *translator) text(kind, text string) *part {
	return &part{Type: kind, Text: &text}
}

// say adds a message of parts, if there are any. A part that can carry a
// breakpoint becomes the holder.
func (t *translator) say(role string, parts []*part) {
	if len(parts) == 0 {
		return
	}
	for _, p := range parts {
		if p.Type != "output_text" {
			t.holder = p
		}
	}
	t.items = append(t.items, &item{Type: "message", Role: role, Content: parts})
}

// mark carries a block's cache_control over as a breakpoint: onto p, the
// part the block became, or, if it became none that can carry one, onto
// the nearest earlier part that can.
func (t *translator) mark(b anthropic.Block, p *part) {
	if !b.HasCacheControl() {
		return
	}
	if p == nil {
		p = t.holder
	}
	if p == nil {
		return // nothing earlier to cache
	}
	p.Breakpoint = &breakpoint{Mode: "explicit"}
	t.breakpoints = true
}

// topLevel translates the request's other fields.
func (t *translator) topLevel(req *anthropic.Request, b *body, opts RequestOptions) error {
	if n := req.MaxTokens(); n > 0 {
		b.MaxOutputTokens = min(n, MaxOutputTokens)
	}
	if v, ok := req.Field("tool_choice"); ok {
		var c struct {
			Type                   string `json:"type"`
			Name                   string `json:"name"`
			DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
		}
		if err := json.Unmarshal(v, &c); err != nil {
			return fmt.Errorf("%w: tool_choice: %v", ErrUntranslatable, err)
		}
		switch c.Type {
		case "auto":
			b.ToolChoice = "auto"
		case "any":
			b.ToolChoice = "required"
		case "none":
			b.ToolChoice = "none"
		case "tool":
			b.ToolChoice = namedChoice{Type: "function", Name: encodeToolName(c.Name)}
		default:
			return fmt.Errorf("%w: tool_choice %q", ErrUntranslatable, c.Type)
		}
		if c.DisableParallelToolUse {
			off := false
			b.ParallelToolCalls = &off
		}
	}
	var r reasoning
	r.Effort = opts.Effort
	if v, ok := req.Field("thinking"); ok {
		var th struct {
			Display string `json:"display"`
		}
		if json.Unmarshal(v, &th) == nil && th.Display == "summarized" {
			r.Summary = "auto"
		}
	}
	if r != (reasoning{}) {
		b.Reasoning = &r
	}
	if v, ok := req.Field("output_config"); ok {
		var oc struct {
			Format *struct {
				Type   string          `json:"type"`
				Schema json.RawMessage `json:"schema"`
			} `json:"format"`
		}
		if err := json.Unmarshal(v, &oc); err != nil {
			return fmt.Errorf("%w: output_config: %v", ErrUntranslatable, err)
		}
		if oc.Format != nil {
			if oc.Format.Type != "json_schema" {
				return fmt.Errorf("%w: output_config.format %q", ErrUntranslatable, oc.Format.Type)
			}
			b.Text = &textConfig{Format: jsonSchemaFormat{Type: "json_schema", Name: "output", Schema: oc.Format.Schema, Strict: false}}
		}
	}
	// Fields with no equivalent that change what the answer would be are
	// named for the ledger. The rest (metadata, which holds account ids,
	// context_management, service_tier, and the like) are Anthropic's own
	// and left out silently.
	for _, name := range []string{"stop_sequences", "temperature", "top_p", "top_k"} {
		if _, ok := req.Field(name); ok {
			t.dropped = append(t.dropped, name)
		}
	}
	return nil
}

// ToolNames maps the name each tool has on OpenAI to its name in Claude
// Code's request. It is built from the request itself, so no table has to
// outlive a turn or a restart.
type ToolNames map[string]string

// Claude returns Claude Code's name for a tool OpenAI called.
func (n ToolNames) Claude(openaiName string) (string, bool) {
	name, ok := n[openaiName]
	return name, ok
}

func (n ToolNames) add(name string) error {
	encoded := encodeToolName(name)
	if prior, ok := n[encoded]; ok && prior != name {
		return fmt.Errorf("%w: tools %q and %q have the same name on OpenAI", ErrUntranslatable, prior, name)
	}
	n[encoded] = name
	return nil
}

var (
	plainToolName = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
	notNameChar   = regexp.MustCompile(`[^A-Za-z0-9_]`)
)

// encodeToolName gives a tool a name OpenAI accepts. A name of letters,
// digits, and underscores, up to 64 long, goes as it is. Any other has each
// other character made "_", is cut to 51 characters, and gets "_" and 12
// hex characters of its SHA-1, as the Codex CLI does, so two names that
// clean up the same stay apart (docs/research/openai.md, Tool names and
// schemas).
func encodeToolName(name string) string {
	if plainToolName.MatchString(name) {
		return name
	}
	clean := notNameChar.ReplaceAllString(name, "_")
	if len(clean) > 51 {
		clean = clean[:51]
	}
	sum := sha1.Sum([]byte(name)) // #nosec G401 -- a name suffix, not a security boundary
	return clean + "_" + hex.EncodeToString(sum[:])[:12]
}
