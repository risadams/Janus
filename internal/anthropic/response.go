package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Summary is what janus learns from a Messages response by watching it go
// past: enough for the ledger and for validating a local answer, without
// holding the response itself.
type Summary struct {
	ID         string
	Model      string
	StopReason string
	// Blocks lists the type of each content block in order, for example
	// ["thinking", "text", "tool_use"].
	Blocks []string
	// Tools lists the name of each tool the model called, in order.
	Tools []string
	Usage Usage
	// Error is set when the response was an API error, either as the whole
	// body or as an error event in the middle of a stream.
	Error *APIError
	// Complete means the response ended the way the protocol says it
	// should: a stream reached message_stop, or a JSON body parsed. A
	// stream that was cut off is not complete, and its usage may be short.
	Complete bool
	// Malformed counts stream events that could not be read. They are
	// skipped, not fatal: the response is still relayed either way.
	Malformed int
}

// Usage is the token accounting from a response. Input and Output are
// always reported. The rest are pointers because servers differ in what
// they send (LM Studio reports no cache writes at all) and absent must not
// be mistaken for zero.
type Usage struct {
	Input  int
	Output int
	// CacheRead and CacheWrite are tokens read from and written to the
	// prompt cache. CacheWrite is the total; the two below split it by TTL
	// when the server says.
	CacheRead    *int
	CacheWrite   *int
	CacheWrite5m *int
	CacheWrite1h *int
	// Thinking is the part of Output spent on thinking.
	Thinking *int
	// Speed is "fast" when the request ran in fast mode, which is priced
	// differently; otherwise "standard" or empty.
	Speed string
}

// APIError is the error object the Messages API returns.
type APIError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// wireUsage is usage as it appears on the wire. Every field is optional so
// that a later, partial usage object only overwrites what it mentions.
type wireUsage struct {
	InputTokens              *int `json:"input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	CacheCreation            *struct {
		Ephemeral5m *int `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h *int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	OutputTokensDetails *struct {
		ThinkingTokens *int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
	Speed string `json:"speed"`
}

// merge applies w on top of u. Streams report usage twice, at the start and
// cumulatively at the end, and the second report can omit fields the first
// one had.
func (u *Usage) merge(w *wireUsage) {
	if w == nil {
		return
	}
	if w.InputTokens != nil {
		u.Input = *w.InputTokens
	}
	if w.OutputTokens != nil {
		u.Output = *w.OutputTokens
	}
	if w.CacheReadInputTokens != nil {
		u.CacheRead = w.CacheReadInputTokens
	}
	if w.CacheCreationInputTokens != nil {
		u.CacheWrite = w.CacheCreationInputTokens
	}
	if c := w.CacheCreation; c != nil {
		if c.Ephemeral5m != nil {
			u.CacheWrite5m = c.Ephemeral5m
		}
		if c.Ephemeral1h != nil {
			u.CacheWrite1h = c.Ephemeral1h
		}
	}
	if d := w.OutputTokensDetails; d != nil && d.ThinkingTokens != nil {
		u.Thinking = d.ThinkingTokens
	}
	if w.Speed != "" {
		u.Speed = w.Speed
	}
}

// wireMessage is a whole message: the body of a non-streaming response, and
// the payload of a stream's message_start event.
type wireMessage struct {
	Type       string     `json:"type"`
	ID         string     `json:"id"`
	Model      string     `json:"model"`
	StopReason string     `json:"stop_reason"`
	Usage      *wireUsage `json:"usage"`
	Content    []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"content"`
	Error *APIError `json:"error"`
}

// Observer watches a Messages response and builds a Summary. Write the
// response body to it as the body is relayed; it never fails and never
// holds more than one stream event, or one JSON body up to a limit.
//
// It handles both forms of response: a server-sent event stream, and a
// single JSON document. Which one is decided from the content type.
type Observer struct {
	sse     bool
	buf     []byte
	dropped bool // the buffer overflowed and the rest was discarded
	summary Summary

	event string // SSE event name being assembled
	data  []byte // SSE data lines being assembled
}

// maxBuffered bounds what an Observer keeps: the longest stream line, or the
// largest JSON body, it will try to read. Past it the response is still
// relayed; janus just stops learning from it.
const maxBuffered = 16 << 20

// NewObserver returns an Observer for a response with the given
// Content-Type header.
func NewObserver(contentType string) *Observer {
	return &Observer{sse: strings.HasPrefix(strings.TrimSpace(strings.ToLower(contentType)), "text/event-stream")}
}

// Write feeds the next piece of the response body. It always accepts
// everything it is given.
func (o *Observer) Write(p []byte) (int, error) {
	if o.dropped {
		return len(p), nil
	}
	o.buf = append(o.buf, p...)
	if o.sse {
		o.drainLines()
	}
	if len(o.buf) > maxBuffered {
		o.buf, o.dropped = nil, true
	}
	return len(p), nil
}

// Summary reports what has been seen. Call it once the body has ended; for
// a JSON response nothing is known before then.
func (o *Observer) Summary() Summary {
	if !o.sse && !o.dropped && len(o.buf) > 0 {
		o.readJSONBody()
		o.buf = nil
	}
	return o.summary
}

func (o *Observer) readJSONBody() {
	var m wireMessage
	if err := json.Unmarshal(o.buf, &m); err != nil {
		o.summary.Malformed++
		return
	}
	o.summary.Complete = true
	if m.Type == "error" || (m.Error != nil && m.Type == "") {
		o.summary.Error = m.Error
		return
	}
	o.summary.ID, o.summary.Model, o.summary.StopReason = m.ID, m.Model, m.StopReason
	o.summary.Usage.merge(m.Usage)
	for _, b := range m.Content {
		o.summary.Blocks = append(o.summary.Blocks, b.Type)
		if b.Type == "tool_use" {
			o.summary.Tools = append(o.summary.Tools, b.Name)
		}
	}
}

// drainLines consumes every complete line in the buffer. A partial line at
// the end stays for the next Write.
func (o *Observer) drainLines() {
	for {
		i := bytes.IndexByte(o.buf, '\n')
		if i < 0 {
			return
		}
		line := bytes.TrimSuffix(o.buf[:i], []byte("\r"))
		o.line(line)
		o.buf = o.buf[i+1:]
	}
}

// line handles one line of the event stream format: "field: value" lines
// build up an event, and a blank line dispatches it.
func (o *Observer) line(line []byte) {
	switch {
	case len(line) == 0:
		if o.data != nil {
			o.dispatch()
		}
		o.event, o.data = "", nil
	case line[0] == ':':
		// A comment; servers use these as keep-alives.
	default:
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			o.event = string(value)
		case "data":
			if o.data != nil {
				o.data = append(o.data, '\n')
			}
			o.data = append(o.data, value...)
		}
	}
}

func (o *Observer) dispatch() {
	var ev struct {
		Type    string       `json:"type"`
		Message *wireMessage `json:"message"`
		Block   *struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta *struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage *wireUsage `json:"usage"`
		Error *APIError  `json:"error"`
	}
	if err := json.Unmarshal(o.data, &ev); err != nil {
		o.summary.Malformed++
		return
	}

	// The type inside the data is authoritative; the event name is only a
	// fallback for a server that leaves it out.
	kind := ev.Type
	if kind == "" {
		kind = o.event
	}
	switch kind {
	case "message_start":
		if m := ev.Message; m != nil {
			o.summary.ID, o.summary.Model = m.ID, m.Model
			o.summary.Usage.merge(m.Usage)
		}
	case "content_block_start":
		if ev.Block != nil {
			o.summary.Blocks = append(o.summary.Blocks, ev.Block.Type)
			if ev.Block.Type == "tool_use" {
				o.summary.Tools = append(o.summary.Tools, ev.Block.Name)
			}
		}
	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			o.summary.StopReason = ev.Delta.StopReason
		}
		o.summary.Usage.merge(ev.Usage)
	case "message_stop":
		o.summary.Complete = true
	case "error":
		o.summary.Error = ev.Error
	}
}
