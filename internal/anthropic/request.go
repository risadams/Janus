package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Request is a Messages API request body.
//
// It is read-mostly. The fields janus needs are available through typed
// accessors; everything else is carried along untouched. A Request that has
// not been changed encodes to exactly the bytes it was parsed from, and one
// that has been changed re-encodes only the parts that changed.
type Request struct {
	raw      []byte
	members  []member
	modified bool

	model     string
	stream    bool
	maxTokens int
	system    []Block
	tools     []Tool
	messages  []Message
}

// ParseRequest reads a Messages request body. An error means janus does not
// understand the body well enough to act on it; the caller should forward
// the original bytes as they are.
func ParseRequest(data []byte) (*Request, error) {
	members, err := parseObject(data)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	r := &Request{raw: data, members: members}

	for _, m := range members {
		var err error
		switch m.key {
		case "model":
			err = json.Unmarshal(m.val, &r.model)
		case "stream":
			err = json.Unmarshal(m.val, &r.stream)
		case "max_tokens":
			err = json.Unmarshal(m.val, &r.maxTokens)
		case "system":
			r.system, _, err = parseContent(m.val)
		case "tools":
			r.tools, err = parseTools(m.val)
		case "messages":
			r.messages, err = parseMessages(m.val)
		}
		if err != nil {
			return nil, fmt.Errorf("request.%s: %w", m.key, err)
		}
	}
	return r, nil
}

// Bytes is the request body to send. Unmodified, it is the original bytes.
func (r *Request) Bytes() []byte {
	if !r.modified {
		return r.raw
	}
	return encodeObject(r.members)
}

// Modified reports whether any setter has been called.
func (r *Request) Modified() bool { return r.modified }

func (r *Request) Model() string       { return r.model }
func (r *Request) Stream() bool        { return r.stream }
func (r *Request) MaxTokens() int      { return r.maxTokens }
func (r *Request) System() []Block     { return r.system }
func (r *Request) Tools() []Tool       { return r.tools }
func (r *Request) Messages() []Message { return r.messages }

// Field returns any top-level field as raw JSON, for fields that have no
// accessor (metadata, thinking, context_management, and whatever comes
// next).
func (r *Request) Field(name string) (json.RawMessage, bool) {
	return find(r.members, name)
}

func (r *Request) SetModel(model string) {
	r.model = model
	r.replace("model", encode(model))
}

func (r *Request) SetTools(tools []Tool) {
	r.tools = tools
	raws := make([]json.RawMessage, len(tools))
	for i, t := range tools {
		raws[i] = t.raw
	}
	r.replace("tools", encodeArray(raws))
}

func (r *Request) SetMessages(messages []Message) {
	r.messages = messages
	raws := make([]json.RawMessage, len(messages))
	for i, m := range messages {
		raws[i] = m.raw
	}
	r.replace("messages", encodeArray(raws))
}

func (r *Request) replace(key string, val json.RawMessage) {
	r.members = set(r.members, key, val)
	r.modified = true
}

// CacheBreakpoints counts the cache_control markers in the request: on
// system blocks, tools, and message content blocks.
func (r *Request) CacheBreakpoints() int {
	n := 0
	for _, b := range r.system {
		if b.HasCacheControl() {
			n++
		}
	}
	for _, t := range r.tools {
		if _, ok := find(t.members, "cache_control"); ok {
			n++
		}
	}
	for _, m := range r.messages {
		for _, b := range m.blocks {
			if b.HasCacheControl() {
				n++
			}
		}
	}
	return n
}

// Message is one entry of the messages array.
type Message struct {
	Role string

	raw     json.RawMessage
	members []member
	blocks  []Block
	// plain is set when content was a bare string rather than an array of
	// blocks. Blocks still returns it, as a single text block.
	plain bool
}

func parseMessages(data json.RawMessage) ([]Message, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, err
	}
	messages := make([]Message, len(raws))
	for i, raw := range raws {
		members, err := parseObject(raw)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		m := Message{raw: raw, members: members}
		if v, ok := find(members, "role"); ok {
			if err := json.Unmarshal(v, &m.Role); err != nil {
				return nil, fmt.Errorf("[%d].role: %w", i, err)
			}
		}
		if v, ok := find(members, "content"); ok {
			if m.blocks, m.plain, err = parseContent(v); err != nil {
				return nil, fmt.Errorf("[%d].content: %w", i, err)
			}
		}
		messages[i] = m
	}
	return messages, nil
}

// Blocks is the message's content. A message whose content was a plain
// string yields one text block holding it.
func (m Message) Blocks() []Block { return m.blocks }

// Raw is the message exactly as it will be sent.
func (m Message) Raw() json.RawMessage { return m.raw }

// WithBlocks returns a copy of the message with its content replaced. Every
// other field of the message, known or not, is kept.
func (m Message) WithBlocks(blocks []Block) Message {
	raws := make([]json.RawMessage, len(blocks))
	for i, b := range blocks {
		raws[i] = b.raw
	}
	m.members = set(m.members, "content", encodeArray(raws))
	m.raw = encodeObject(m.members)
	m.blocks = blocks
	m.plain = false
	return m
}

// WithRole returns a copy of the message with its role replaced. Every
// other field of the message, known or not, is kept.
func (m Message) WithRole(role string) Message {
	m.members = set(m.members, "role", encode(role))
	m.raw = encodeObject(m.members)
	m.Role = role
	return m
}

// parseContent reads a content value, which the API allows to be either a
// string or an array of blocks. It is also the shape of the system field.
func parseContent(data json.RawMessage) (blocks []Block, plain bool, err error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, false, err
		}
		return []Block{NewTextBlock(text)}, true, nil
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, false, err
	}
	blocks = make([]Block, len(raws))
	for i, raw := range raws {
		if blocks[i], err = NewBlock(raw); err != nil {
			return nil, false, fmt.Errorf("[%d]: %w", i, err)
		}
	}
	return blocks, false, nil
}

// Block is one content block. Its Type says what it is; blocks of a type
// janus does not know are held and forwarded like any other.
type Block struct {
	Type string

	raw     json.RawMessage
	members []member
}

// NewBlock wraps the JSON for one content block.
func NewBlock(raw json.RawMessage) (Block, error) {
	members, err := parseObject(raw)
	if err != nil {
		return Block{}, err
	}
	b := Block{raw: raw, members: members}
	if v, ok := find(members, "type"); ok {
		if err := json.Unmarshal(v, &b.Type); err != nil {
			return Block{}, fmt.Errorf("type: %w", err)
		}
	}
	return b, nil
}

// NewTextBlock builds a text block.
func NewTextBlock(text string) Block {
	members := []member{{"type", encode("text")}, {"text", encode(text)}}
	return Block{Type: "text", raw: encodeObject(members), members: members}
}

// Raw is the block exactly as it will be sent.
func (b Block) Raw() json.RawMessage { return b.raw }

// Decode reads the block into one of the typed views below, or any struct
// with json tags.
func (b Block) Decode(v any) error { return json.Unmarshal(b.raw, v) }

// Field returns one field of the block as raw JSON.
func (b Block) Field(name string) (json.RawMessage, bool) { return find(b.members, name) }

// Text is the text of a text block, and empty for any other type.
func (b Block) Text() string {
	if b.Type != "text" {
		return ""
	}
	var t TextBlock
	_ = b.Decode(&t) // a text block that does not decode has no text to give
	return t.Text
}

func (b Block) HasCacheControl() bool {
	_, ok := find(b.members, "cache_control")
	return ok
}

// With returns a copy of the block with one field set, in place if the
// block already had it and at the end if not. Every other field is kept.
func (b Block) With(key string, val json.RawMessage) Block {
	b.members = set(b.members, key, val)
	b.raw = encodeObject(b.members)
	return b
}

// Without returns a copy of the block without one field. A block that did
// not have it is returned as it was, bytes included.
func (b Block) Without(key string) Block {
	kept := make([]member, 0, len(b.members))
	for _, m := range b.members {
		if m.key != key {
			kept = append(kept, m)
		}
	}
	if len(kept) == len(b.members) {
		return b
	}
	b.members = kept
	b.raw = encodeObject(kept)
	return b
}

// Typed views of the block types janus reads. Decode a Block into one of
// these after checking its Type.
type (
	TextBlock struct {
		Text string `json:"text"`
	}

	ThinkingBlock struct {
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
	}

	ToolUseBlock struct {
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}

	// ToolResultBlock's Content is a string or an array of blocks; pass it
	// to ParseContent.
	ToolResultBlock struct {
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}

	// ImageBlock's Source is left raw: janus forwards images and never
	// looks inside them.
	ImageBlock struct {
		Source json.RawMessage `json:"source"`
	}

	// CacheControl is the value of a cache_control field.
	CacheControl struct {
		Type string `json:"type"`
		TTL  string `json:"ttl"`
	}
)

// ParseContent reads a content value that may be a string or an array of
// blocks, as found in tool results. A string comes back as one text block.
func ParseContent(data json.RawMessage) ([]Block, error) {
	blocks, _, err := parseContent(data)
	return blocks, err
}

// Tool is one entry of the tools array.
type Tool struct {
	Name string

	raw     json.RawMessage
	members []member
}

// NewTool wraps the JSON for one tool definition.
func NewTool(raw json.RawMessage) (Tool, error) {
	members, err := parseObject(raw)
	if err != nil {
		return Tool{}, err
	}
	t := Tool{raw: raw, members: members}
	if v, ok := find(members, "name"); ok {
		if err := json.Unmarshal(v, &t.Name); err != nil {
			return Tool{}, fmt.Errorf("name: %w", err)
		}
	}
	return t, nil
}

func parseTools(data json.RawMessage) ([]Tool, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, err
	}
	tools := make([]Tool, len(raws))
	for i, raw := range raws {
		var err error
		if tools[i], err = NewTool(raw); err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
	}
	return tools, nil
}

// Raw is the tool definition exactly as it will be sent.
func (t Tool) Raw() json.RawMessage { return t.raw }

// Field returns one field of the tool definition as raw JSON.
func (t Tool) Field(name string) (json.RawMessage, bool) { return find(t.members, name) }
