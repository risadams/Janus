package openaitest

import (
	"encoding/json"
	"io"
	"net/http"
	"unicode/utf8"
)

// A Reply is how the fake answers one request to /v1/responses.
//
// By default it is a completed response holding Output, streamed or not as
// the request asked. HTTP makes it an error before any response; Status
// "incomplete" or "failed" changes how it ends; StreamError and Break end
// a stream early.
type Reply struct {
	// Output is what the model produced, in order.
	Output []Item
	// Usage is reported when the response completes or is incomplete.
	Usage Usage
	// Status is "completed" when empty, or "incomplete" or "failed".
	Status string
	// Reason is the incomplete_details.reason of an incomplete response,
	// such as "max_output_tokens", or the error code of a failed one,
	// such as "server_error".
	Reason string
	// Message is a failed response's error message.
	Message string
	// HTTP, if set, is answered instead of any response.
	HTTP *HTTPError
	// StreamError, if set, ends a stream after Output with an "error"
	// event and no final event.
	StreamError *StreamError
	// Break drops the connection after Output, before the final event, so
	// the client reads an unexpected end of stream.
	Break bool
	// Header is added to the answer's headers. Rate-limit headers set here
	// replace the defaults.
	Header http.Header
}

// An HTTPError is an error answered with an HTTP status and OpenAI's error
// body: {"error": {"message", "type", "param", "code"}}.
type HTTPError struct {
	Status  int
	Type    string
	Code    string
	Message string
}

// A StreamError is the "error" event OpenAI can send mid-stream.
type StreamError struct {
	Code    string
	Message string
}

// Usage is a response's token counts as OpenAI reports them. Input
// includes the cached and written tokens, unlike Anthropic's input_tokens;
// Output includes Reasoning.
type Usage struct {
	Input     int
	Cached    int // read from the cache
	Write30m  int // written to the cache for 30 minutes
	Write12h  int // written to the cache for 12 hours
	Output    int
	Reasoning int
}

// An Item is one entry of a response's output: Text, Refusal, Call, or
// Reasoning.
type Item interface{ isItem() }

// Text is an assistant message with one output_text part.
type Text struct{ Text string }

// Refusal is an assistant message with one refusal part.
type Refusal struct{ Refusal string }

// Call is a function call. Arguments is its JSON, as a string.
type Call struct {
	CallID    string
	Name      string
	Arguments string
}

// Reasoning is a reasoning item. Summary is streamed only if it is not
// empty; Encrypted is the encrypted_content, sent only once the item is
// done.
type Reasoning struct {
	Summary   string
	Encrypted string
}

func (Text) isItem()      {}
func (Refusal) isItem()   {}
func (Call) isItem()      {}
func (Reasoning) isItem() {}

// Answer is a completed response holding items.
func Answer(items ...Item) Reply { return Reply{Output: items} }

// Error is a reply answered with an HTTP error in OpenAI's shape.
func Error(status int, typ, code, message string) HTTPError {
	return HTTPError{Status: status, Type: typ, Code: code, Message: message}
}

// Fail is a reply answered with an HTTP error.
func Fail(status int, typ, code, message string) Reply {
	e := Error(status, typ, code, message)
	return Reply{HTTP: &e}
}

// RateLimited is a 429 with code, such as "rate_limit_exceeded" or
// "insufficient_quota", and the headers a per-minute limit comes with:
// Retry-After in seconds and resets as durations. A quota or spend error
// carries no reset, so pass retryAfter "" for those, and none is sent.
func RateLimited(code, retryAfter string) Reply {
	r := Fail(http.StatusTooManyRequests, "requests", code, "Rate limit reached ("+code+").")
	if code == "insufficient_quota" {
		r.HTTP.Type = "insufficient_quota"
		r.HTTP.Message = "You exceeded your current quota, please check your plan and billing details."
	}
	r.Header = http.Header{"X-Ratelimit-Remaining-Requests": {"0"}, "X-Ratelimit-Remaining-Tokens": {"0"}}
	if retryAfter != "" {
		r.Header.Set("Retry-After", retryAfter)
		r.Header.Set("X-Ratelimit-Reset-Requests", retryAfter+"s")
		r.Header.Set("X-Ratelimit-Reset-Tokens", retryAfter+"s")
	}
	return r
}

// response is the response object being built for one request.
type response struct {
	id              string
	n               int
	model           string
	store           bool
	truncation      string
	status          string
	output          []obj
	usage           any // null until the response ends
	err, incomplete any // null unless it failed or is incomplete
	completedAt     any // null until it completes
	items           int // output items made, for their ids
	sequence        int // the next event's sequence_number
	omitEventLines  bool
}

func newResponse(n int, model string, store *bool, truncation string) *response {
	if truncation == "" {
		truncation = "disabled"
	}
	return &response{
		id: "resp_" + itoa(n), n: n, model: model, store: store == nil || *store,
		truncation: truncation, status: "in_progress",
	}
}

// createdAt is a fixed time, so a test's output is the same every run.
const createdAt = 1741487325

func (r *response) object() obj {
	output := r.output
	if output == nil {
		output = []obj{}
	}
	return obj{
		"id": r.id, "object": "response", "created_at": createdAt, "status": r.status,
		"completed_at": r.completedAt, "error": r.err, "incomplete_details": r.incomplete,
		"instructions": nil, "max_output_tokens": nil, "model": r.model, "output": output,
		"parallel_tool_calls": true, "previous_response_id": nil, "store": r.store,
		"text": obj{"format": obj{"type": "text"}}, "tool_choice": "auto", "tools": []any{},
		"truncation": r.truncation, "usage": r.usage, "metadata": obj{},
	}
}

// finish fills in the output and ending of a response that is not
// streamed: every item complete, and the status the reply asks for.
func (r *response) finish(reply Reply) {
	for _, it := range reply.Output {
		r.output = append(r.output, r.done(it))
	}
	r.end(reply)
}

// end sets the response's final status, error, and usage.
func (r *response) end(reply Reply) {
	switch reply.Status {
	case "incomplete":
		r.status = "incomplete"
		r.incomplete = obj{"reason": reply.Reason}
		r.usage = reply.Usage.object()
	case "failed":
		r.status = "failed"
		r.err = obj{"code": reply.Reason, "message": reply.Message}
	default:
		r.status = "completed"
		r.completedAt = createdAt + 1
		r.usage = reply.Usage.object()
	}
}

func (u Usage) object() obj {
	return obj{
		"input_tokens": u.Input,
		"input_tokens_details": obj{
			"cached_tokens":      u.Cached,
			"cache_write_tokens": u.Write30m + u.Write12h,
			"cache_write_tokens_details": obj{
				"ttl_30m_tokens": u.Write30m,
				"ttl_12h_tokens": u.Write12h,
			},
		},
		"output_tokens":         u.Output,
		"output_tokens_details": obj{"reasoning_tokens": u.Reasoning},
		"total_tokens":          u.Input + u.Output,
	}
}

// nextID returns the id of the next output item, with the prefix OpenAI
// uses for its type.
func (r *response) nextID(prefix string) string {
	r.items++
	return prefix + "_" + itoa(r.n) + "_" + itoa(r.items)
}

// done returns an item as it is once complete, giving it its id.
func (r *response) done(it Item) obj {
	switch it := it.(type) {
	case Text:
		return message(r.nextID("msg"), "completed", obj{"type": "output_text", "text": it.Text, "annotations": []any{}, "logprobs": []any{}})
	case Refusal:
		return message(r.nextID("msg"), "completed", obj{"type": "refusal", "refusal": it.Refusal})
	case Call:
		return call(r.nextID("fc"), "completed", it, it.Arguments)
	case Reasoning:
		return reasoning(r.nextID("rs"), it, true)
	}
	panic("openaitest: unknown item type")
}

func message(id, status string, parts ...obj) obj {
	if parts == nil {
		parts = []obj{}
	}
	return obj{"id": id, "type": "message", "status": status, "role": "assistant", "content": parts}
}

func call(id, status string, c Call, arguments string) obj {
	return obj{"id": id, "type": "function_call", "status": status, "call_id": c.CallID, "name": c.Name, "arguments": arguments}
}

// reasoning returns a reasoning item. Its encrypted_content is there only
// once it is done, as OpenAI's reference says.
func reasoning(id string, it Reasoning, done bool) obj {
	summary := []obj{}
	if done && it.Summary != "" {
		summary = append(summary, obj{"type": "summary_text", "text": it.Summary})
	}
	o := obj{"id": id, "type": "reasoning", "summary": summary}
	if done {
		o["encrypted_content"] = it.Encrypted
	}
	return o
}

// stream answers with the response as server-sent events, in the order
// OpenAI's streaming reference gives them.
func (s *Server) stream(w http.ResponseWriter, r *response, reply Reply) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	r.omitEventLines = s.OmitEventLines
	send := func(e obj) {
		e["sequence_number"] = r.sequence
		r.sequence++
		data, err := json.Marshal(e)
		if err != nil {
			panic(err) // the fake's own objects always marshal
		}
		if !r.omitEventLines {
			_, _ = io.WriteString(w, "event: "+e["type"].(string)+"\n")
		}
		_, _ = io.WriteString(w, "data: "+string(data)+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	send(obj{"type": "response.created", "response": r.object()})
	send(obj{"type": "response.in_progress", "response": r.object()})
	for i, it := range reply.Output {
		r.output = append(r.output, r.streamItem(send, i, it))
	}

	switch {
	case reply.StreamError != nil:
		send(obj{"type": "error", "code": reply.StreamError.Code, "message": reply.StreamError.Message, "param": nil})
	case reply.Break:
		panic(http.ErrAbortHandler) // drops the connection without ending the stream
	default:
		r.end(reply)
		final := map[string]string{"incomplete": "response.incomplete", "failed": "response.failed"}[r.status]
		if final == "" {
			final = "response.completed"
		}
		send(obj{"type": final, "response": r.object()})
	}
}

// obfuscation is the padding OpenAI adds to delta events by default. Real
// padding is random; a fixed string keeps a test's output the same.
const obfuscation = "x7Gq"

// streamItem sends the events for one output item and returns the item as
// it is when done.
func (r *response) streamItem(send func(obj), index int, it Item) obj {
	switch it := it.(type) {
	case Text:
		id := r.nextID("msg")
		send(obj{"type": "response.output_item.added", "output_index": index, "item": message(id, "in_progress")})
		part := obj{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}
		send(obj{"type": "response.content_part.added", "item_id": id, "output_index": index, "content_index": 0, "part": part})
		for _, d := range chunks(it.Text) {
			send(obj{"type": "response.output_text.delta", "item_id": id, "output_index": index, "content_index": 0, "delta": d, "logprobs": []any{}, "obfuscation": obfuscation})
		}
		send(obj{"type": "response.output_text.done", "item_id": id, "output_index": index, "content_index": 0, "text": it.Text, "logprobs": []any{}})
		part = obj{"type": "output_text", "text": it.Text, "annotations": []any{}, "logprobs": []any{}}
		send(obj{"type": "response.content_part.done", "item_id": id, "output_index": index, "content_index": 0, "part": part})
		item := message(id, "completed", part)
		send(obj{"type": "response.output_item.done", "output_index": index, "item": item})
		return item
	case Refusal:
		id := r.nextID("msg")
		send(obj{"type": "response.output_item.added", "output_index": index, "item": message(id, "in_progress")})
		send(obj{"type": "response.content_part.added", "item_id": id, "output_index": index, "content_index": 0, "part": obj{"type": "refusal", "refusal": ""}})
		for _, d := range chunks(it.Refusal) {
			send(obj{"type": "response.refusal.delta", "item_id": id, "output_index": index, "content_index": 0, "delta": d, "obfuscation": obfuscation})
		}
		send(obj{"type": "response.refusal.done", "item_id": id, "output_index": index, "content_index": 0, "refusal": it.Refusal})
		part := obj{"type": "refusal", "refusal": it.Refusal}
		send(obj{"type": "response.content_part.done", "item_id": id, "output_index": index, "content_index": 0, "part": part})
		item := message(id, "completed", part)
		send(obj{"type": "response.output_item.done", "output_index": index, "item": item})
		return item
	case Call:
		id := r.nextID("fc")
		send(obj{"type": "response.output_item.added", "output_index": index, "item": call(id, "in_progress", it, "")})
		for _, d := range chunks(it.Arguments) {
			send(obj{"type": "response.function_call_arguments.delta", "item_id": id, "output_index": index, "delta": d, "obfuscation": obfuscation})
		}
		send(obj{"type": "response.function_call_arguments.done", "item_id": id, "output_index": index, "arguments": it.Arguments})
		item := call(id, "completed", it, it.Arguments)
		send(obj{"type": "response.output_item.done", "output_index": index, "item": item})
		return item
	case Reasoning:
		id := r.nextID("rs")
		send(obj{"type": "response.output_item.added", "output_index": index, "item": reasoning(id, it, false)})
		if it.Summary != "" {
			send(obj{"type": "response.reasoning_summary_part.added", "item_id": id, "output_index": index, "summary_index": 0, "part": obj{"type": "summary_text", "text": ""}})
			for _, d := range chunks(it.Summary) {
				send(obj{"type": "response.reasoning_summary_text.delta", "item_id": id, "output_index": index, "summary_index": 0, "delta": d, "obfuscation": obfuscation})
			}
			send(obj{"type": "response.reasoning_summary_text.done", "item_id": id, "output_index": index, "summary_index": 0, "text": it.Summary})
			send(obj{"type": "response.reasoning_summary_part.done", "item_id": id, "output_index": index, "summary_index": 0, "part": obj{"type": "summary_text", "text": it.Summary}})
		}
		item := reasoning(id, it, true)
		send(obj{"type": "response.output_item.done", "output_index": index, "item": item})
		return item
	}
	panic("openaitest: unknown item type")
}

// chunkSize is how many bytes, at most, each delta carries. It is small,
// so that a short test string still arrives in several deltas.
const chunkSize = 8

// chunks splits s into deltas of at most chunkSize bytes, never inside a
// character, as a stream might.
func chunks(s string) []string {
	var out []string
	for len(s) > 0 {
		n := min(chunkSize, len(s))
		for n < len(s) && !utf8.RuneStart(s[n]) {
			n++
		}
		out = append(out, s[:n])
		s = s[n:]
	}
	return out
}
