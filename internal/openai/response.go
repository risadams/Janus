package openai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ResponseOptions are what translating an answer back needs from the
// request it answers.
type ResponseOptions struct {
	// Model is the model Claude Code asked for, which the answer names, as
	// for LM Studio.
	Model string
	// Tools decodes the tool names OpenAI calls (TranslateRequest).
	Tools ToolNames
	// Summary is set when Claude Code asked to see thinking
	// (display "summarized"), so that reasoning summaries stream as
	// thinking text. Otherwise reasoning shows as an empty thinking block,
	// as Claude's does with display "omitted".
	Summary bool
}

// Usage is OpenAI's own count for one response, for the ledger (#381).
type Usage struct {
	Input, Cached, CacheWrite, CacheWrite30m, CacheWrite12h int
	Output, Reasoning                                       int
}

// Result is what one answer came to.
type Result struct {
	// Model is the model OpenAI says answered, for the ledger's
	// response.model: the answer Claude Code sees names the model it asked
	// for, which the ledger has as request.model.
	Model string
	// StopReason is the Anthropic stop_reason sent to Claude Code, or empty
	// if the answer did not finish.
	StopReason string
	Usage      Usage
	// Err is set when OpenAI failed the response part way.
	Err *Failure
}

// ErrBrokenStream is TranslateStream's answer when OpenAI's stream ends
// before the response does. What was translated has been written, and
// nothing that would end the Anthropic stream properly: the caller drops
// the connection, so Claude Code sees a broken stream, as it would from
// Anthropic.
var ErrBrokenStream = errors.New("openai: the stream ended before the response did")

// The Responses objects read here.
type (
	wireResponse struct {
		ID                string     `json:"id"`
		Model             string     `json:"model"`
		Status            string     `json:"status"`
		Output            []wireItem `json:"output"`
		Usage             *wireUsage `json:"usage"`
		Error             *wireError `json:"error"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}

	wireItem struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Content   []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
		Summary []struct {
			Text string `json:"text"`
		} `json:"summary"`
	}

	wireUsage struct {
		InputTokens        int `json:"input_tokens"`
		InputTokensDetails struct {
			CachedTokens            int `json:"cached_tokens"`
			CacheWriteTokens        int `json:"cache_write_tokens"`
			CacheWriteTokensDetails struct {
				TTL30m int `json:"ttl_30m_tokens"`
				TTL12h int `json:"ttl_12h_tokens"`
			} `json:"cache_write_tokens_details"`
		} `json:"input_tokens_details"`
		OutputTokens        int `json:"output_tokens"`
		OutputTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	}

	wireError struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}

	// event is one streaming event. Janus reads its type from the data, as
	// the Codex CLI does, not from an SSE event: line, which OpenAI's docs
	// do not show (docs/research/openai.md, Response stream).
	event struct {
		Type        string        `json:"type"`
		OutputIndex int           `json:"output_index"`
		Item        *wireItem     `json:"item"`
		Delta       string        `json:"delta"`
		Response    *wireResponse `json:"response"`
		Code        string        `json:"code"`
		Message     string        `json:"message"`
	}
)

func (u *wireUsage) usage() Usage {
	if u == nil {
		return Usage{}
	}
	d := u.InputTokensDetails
	return Usage{
		Input: u.InputTokens, Cached: d.CachedTokens, CacheWrite: d.CacheWriteTokens,
		CacheWrite30m: d.CacheWriteTokensDetails.TTL30m, CacheWrite12h: d.CacheWriteTokensDetails.TTL12h,
		Output: u.OutputTokens, Reasoning: u.OutputTokensDetails.ReasoningTokens,
	}
}

// anthropicUsage is Anthropic's usage for OpenAI's counts. OpenAI's
// input_tokens includes the tokens read from and written to the cache, and
// Anthropic's does not; the four input fields must still add up to the
// true total, since Claude Code judges how full its context is from them.
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreation            struct {
		Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	OutputTokens int `json:"output_tokens"`
}

func (u Usage) anthropic() anthropicUsage {
	a := anthropicUsage{
		InputTokens:              max(u.Input-u.Cached-u.CacheWrite, 0),
		CacheCreationInputTokens: u.CacheWrite,
		CacheReadInputTokens:     u.Cached,
		OutputTokens:             u.Output,
	}
	// Both of Claude's lifetimes went as OpenAI's 30 minutes, so its writes
	// are counted as the 5-minute ones.
	a.CacheCreation.Ephemeral5m = u.CacheWrite30m
	a.CacheCreation.Ephemeral1h = u.CacheWrite12h
	return a
}

// policyCodes are the codes of a response OpenAI failed for its usage
// policies. Claude Code is told it as a refusal, which it is, rather than
// as an error to retry.
var policyCodes = map[string]bool{"cyber_policy": true, "bio_policy": true, "misalignment_policy_violation": true}

// stopReason maps how a response ended onto Anthropic's stop_reason.
func stopReason(status, incomplete string, lastIsCall, refused bool) string {
	switch {
	case refused:
		return "refusal"
	case status == "incomplete" && incomplete == "max_output_tokens":
		return "max_tokens"
	case status == "incomplete" && incomplete == "content_filter":
		return "refusal"
	case lastIsCall:
		return "tool_use"
	default:
		return "end_turn"
	}
}

// messageID makes the Anthropic message id from OpenAI's response id.
func messageID(responseID string) string {
	return "msg_janus_" + strings.TrimPrefix(responseID, "resp_")
}

// toolInput is a call's arguments as a tool_use input, which must be an
// object. Arguments that are not one go wrapped, so that Claude Code sees
// them and reports the call as malformed rather than janus guessing.
func toolInput(arguments string) json.RawMessage {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return json.RawMessage(`{}`)
	}
	if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		var compact bytes.Buffer
		_ = json.Compact(&compact, []byte(trimmed)) // valid, so it compacts
		return compact.Bytes()
	}
	wrapped, _ := json.Marshal(map[string]string{"arguments": arguments})
	return wrapped
}

// TranslateResponse turns a non-streamed Responses answer into a Messages
// body.
func TranslateResponse(body []byte, opts ResponseOptions) ([]byte, Result, error) {
	var r wireResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, Result{}, fmt.Errorf("openai: the response is not JSON: %w", err)
	}
	res := Result{Model: r.Model, Usage: r.Usage.usage()}
	if r.Status == "failed" && r.Error != nil && !policyCodes[r.Error.Code] {
		f := classify(http.StatusInternalServerError, r.Error)
		res.Err = &f
		return nil, res, nil
	}
	type block struct {
		Type      string          `json:"type"`
		Text      *string         `json:"text,omitempty"`
		Thinking  *string         `json:"thinking,omitempty"`
		Signature *string         `json:"signature,omitempty"`
		ID        string          `json:"id,omitempty"`
		Name      string          `json:"name,omitempty"`
		Input     json.RawMessage `json:"input,omitempty"`
	}
	content := []block{}
	refused := false
	lastIsCall := false
	for _, it := range r.Output {
		lastIsCall = false
		switch it.Type {
		case "reasoning":
			var text, signature string
			if opts.Summary {
				for _, s := range it.Summary {
					text += s.Text
				}
			}
			content = append(content, block{Type: "thinking", Thinking: &text, Signature: &signature})
		case "message":
			var text string
			for _, p := range it.Content {
				switch p.Type {
				case "output_text":
					text += p.Text
				case "refusal":
					text += p.Refusal
					refused = true
				}
			}
			content = append(content, block{Type: "text", Text: &text})
		case "function_call":
			content = append(content, block{Type: "tool_use", ID: it.CallID, Name: opts.Tools.decode(it.Name), Input: toolInput(it.Arguments)})
			lastIsCall = true
		}
	}
	if r.Status == "failed" && r.Error != nil {
		// A policy failure: say it as the refusal it is.
		text := "OpenAI declined to answer: " + r.Error.Message
		content = append(content, block{Type: "text", Text: &text})
		refused = true
	}
	var incomplete string
	if r.IncompleteDetails != nil {
		incomplete = r.IncompleteDetails.Reason
	}
	res.StopReason = stopReason(r.Status, incomplete, lastIsCall, refused)
	out, err := json.Marshal(struct {
		ID           string         `json:"id"`
		Type         string         `json:"type"`
		Role         string         `json:"role"`
		Model        string         `json:"model"`
		Content      []block        `json:"content"`
		StopReason   string         `json:"stop_reason"`
		StopSequence *string        `json:"stop_sequence"`
		Usage        anthropicUsage `json:"usage"`
	}{messageID(r.ID), "message", "assistant", opts.Model, content, res.StopReason, nil, res.Usage.anthropic()})
	if err != nil {
		return nil, Result{}, fmt.Errorf("openai: %w", err)
	}
	return out, res, nil
}

func (n ToolNames) decode(name string) string {
	if claude, ok := n.Claude(name); ok {
		return claude
	}
	return name // a tool the request did not offer; Claude Code says so
}

// streamState is a stream being translated.
type streamState struct {
	w    io.Writer
	opts ResponseOptions
	err  error

	started bool
	model   string      // as OpenAI reported it
	next    int         // the next Anthropic content block's index
	open    map[int]int // OpenAI output_index to open Anthropic block index
	kinds   map[int]string
	refused bool
	lastIs  string // the type of the latest output item
}

// TranslateStream reads a Responses stream from src and writes Claude Code
// the Messages stream it stands for, event by event, as each arrives. It
// returns when the response ends, or with ErrBrokenStream if the stream
// stops first, or with the error from writing to w.
func TranslateStream(w io.Writer, src io.Reader, opts ResponseOptions) (Result, error) {
	s := &streamState{w: w, opts: opts, open: map[int]int{}, kinds: map[int]string{}}
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	var data bytes.Buffer
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			if data.Len() == 0 {
				continue
			}
			res, done := s.dispatch(data.Bytes())
			data.Reset()
			if s.err != nil {
				return Result{}, s.err
			}
			if done {
				return res, nil
			}
			continue
		}
		if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimPrefix(rest, []byte(" ")))
		}
		// event:, id:, retry:, and comments are ignored.
	}
	if data.Len() > 0 {
		if res, done := s.dispatch(data.Bytes()); done && s.err == nil {
			return res, nil
		}
		if s.err != nil {
			return Result{}, s.err
		}
	}
	return Result{}, ErrBrokenStream
}

// send writes one Anthropic event.
func (s *streamState) send(typ string, v any) {
	if s.err != nil {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		s.err = err
		return
	}
	_, s.err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", typ, data)
}

type obj = map[string]any

func (s *streamState) start(responseID string) {
	if s.started {
		return
	}
	s.started = true
	s.send("message_start", obj{"type": "message_start", "message": obj{
		"id": messageID(responseID), "type": "message", "role": "assistant", "model": s.opts.Model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": anthropicUsage{},
	}})
}

func (s *streamState) openBlock(outputIndex int, kind string, block obj) {
	index := s.next
	s.next++
	s.open[outputIndex] = index
	s.kinds[outputIndex] = kind
	s.send("content_block_start", obj{"type": "content_block_start", "index": index, "content_block": block})
}

func (s *streamState) delta(outputIndex int, delta obj) {
	index, ok := s.open[outputIndex]
	if !ok {
		return // a delta for an item never opened carries nothing to place
	}
	s.send("content_block_delta", obj{"type": "content_block_delta", "index": index, "delta": delta})
}

func (s *streamState) closeBlock(outputIndex int) {
	index, ok := s.open[outputIndex]
	if !ok {
		return
	}
	if s.kinds[outputIndex] == "reasoning" {
		// Without #410's envelope, OpenAI's reasoning has no signature
		// Claude would accept, and an empty one says so: Janus drops it on
		// the way back to Claude (RepairForUpstream).
		s.send("content_block_delta", obj{"type": "content_block_delta", "index": index, "delta": obj{"type": "signature_delta", "signature": ""}})
	}
	s.send("content_block_stop", obj{"type": "content_block_stop", "index": index})
	delete(s.open, outputIndex)
}

// closeAll closes the blocks still open, lowest first.
func (s *streamState) closeAll() {
	for len(s.open) > 0 {
		lowest, first := 0, true
		for oi, bi := range s.open {
			if first || bi < s.open[lowest] {
				lowest, first = oi, false
			}
		}
		s.closeBlock(lowest)
	}
}

// dispatch handles one event, and reports whether the response ended.
func (s *streamState) dispatch(data []byte) (Result, bool) {
	var e event
	if err := json.Unmarshal(data, &e); err != nil {
		return Result{}, false // not an event janus knows how to read
	}
	switch e.Type {
	case "response.created":
		id := ""
		if e.Response != nil {
			id, s.model = e.Response.ID, e.Response.Model
		}
		s.start(id)
	case "response.output_item.added":
		if e.Item == nil {
			return Result{}, false
		}
		s.start("")
		s.lastIs = e.Item.Type
		switch e.Item.Type {
		case "reasoning":
			s.openBlock(e.OutputIndex, "reasoning", obj{"type": "thinking", "thinking": "", "signature": ""})
		case "message":
			s.openBlock(e.OutputIndex, "message", obj{"type": "text", "text": ""})
		case "function_call":
			s.openBlock(e.OutputIndex, "function_call", obj{"type": "tool_use", "id": e.Item.CallID, "name": s.opts.Tools.decode(e.Item.Name), "input": obj{}})
		}
	case "response.output_text.delta":
		s.delta(e.OutputIndex, obj{"type": "text_delta", "text": e.Delta})
	case "response.refusal.delta":
		s.refused = true
		s.delta(e.OutputIndex, obj{"type": "text_delta", "text": e.Delta})
	case "response.function_call_arguments.delta":
		s.delta(e.OutputIndex, obj{"type": "input_json_delta", "partial_json": e.Delta})
	case "response.reasoning_summary_text.delta":
		if s.opts.Summary {
			s.delta(e.OutputIndex, obj{"type": "thinking_delta", "thinking": e.Delta})
		}
	case "response.output_item.done":
		s.closeBlock(e.OutputIndex)
	case "response.completed", "response.incomplete":
		return s.finish(e.Response), true
	case "response.failed":
		var we *wireError
		if e.Response != nil {
			we = e.Response.Error
		}
		if we != nil && policyCodes[we.Code] {
			return s.declined(e.Response, we.Message), true
		}
		return s.fail(we), true
	case "error":
		return s.fail(&wireError{Code: e.Code, Message: e.Message}), true
	}
	return Result{}, false
}

// finish ends the Anthropic stream: any open block closed, then
// message_delta with the stop reason and usage, then message_stop.
func (s *streamState) finish(r *wireResponse) Result {
	s.start("")
	s.closeAll()
	res := Result{Model: s.model}
	status, incomplete := "completed", ""
	if r != nil {
		res.Usage = r.Usage.usage()
		if r.Model != "" {
			res.Model = r.Model
		}
		status = r.Status
		if r.IncompleteDetails != nil {
			incomplete = r.IncompleteDetails.Reason
		}
	}
	res.StopReason = stopReason(status, incomplete, s.lastIs == "function_call", s.refused)
	s.send("message_delta", obj{"type": "message_delta",
		"delta": obj{"stop_reason": res.StopReason, "stop_sequence": nil},
		"usage": res.Usage.anthropic()})
	s.send("message_stop", obj{"type": "message_stop"})
	return res
}

// declined ends a response OpenAI failed for its policies as the refusal
// it is, with OpenAI's message as text.
func (s *streamState) declined(r *wireResponse, message string) Result {
	s.start("")
	s.closeAll()
	s.openBlock(-1, "message", obj{"type": "text", "text": ""})
	s.delta(-1, obj{"type": "text_delta", "text": "OpenAI declined to answer: " + message})
	s.closeBlock(-1)
	s.refused = true
	return s.finish(r)
}

// fail ends the stream with an Anthropic error event, as Anthropic ends a
// stream that fails part way.
func (s *streamState) fail(we *wireError) Result {
	if we == nil {
		we = &wireError{Message: "the response failed without saying why"}
	}
	f := classify(http.StatusInternalServerError, we)
	s.send("error", f.body())
	return Result{Model: s.model, Err: &f}
}
