package guard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Restorer wraps a streamed Messages response and puts secrets back as the
// stream goes by.
//
// A stream delivers text in small pieces, and a placeholder can be cut
// anywhere between two of them. So for each content block the Restorer
// holds back any tail that could be the start of a placeholder, until the
// next piece shows whether it is. Events that cannot involve a placeholder
// pass through as the exact bytes they arrived in.
type Restorer struct {
	src io.ReadCloser
	rd  *Redaction

	in      []byte         // input not yet a whole event
	out     []byte         // output not yet read
	pending map[int]string // per content block, the tail held back
	kind    map[int]string // per content block, the delta type last seen
	stopped bool

	// Restored counts placeholders replaced so far.
	Restored int
	// Err is set if the stream was stopped because the model referred to
	// a secret in a form that could not be restored.
	Err error
}

// Restore returns body with secrets put back as it is read.
func (rd *Redaction) Restore(body io.ReadCloser) *Restorer {
	return &Restorer{src: body, rd: rd, pending: map[int]string{}, kind: map[int]string{}}
}

func (r *Restorer) Close() error { return r.src.Close() }

func (r *Restorer) Read(p []byte) (int, error) {
	for len(r.out) == 0 {
		if r.stopped {
			return 0, io.EOF
		}
		buf := make([]byte, 32<<10)
		n, err := r.src.Read(buf)
		r.in = append(r.in, buf[:n]...)
		r.drain()
		if err != nil {
			if !r.stopped {
				// Whatever is left is not a whole event; pass it on.
				r.out = append(r.out, r.in...)
				r.in = nil
			}
			if len(r.out) == 0 {
				return 0, err
			}
			r.stopped = true
		}
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

// drain handles every whole event in the input.
func (r *Restorer) drain() {
	for !r.stopped {
		end, sep := eventEnd(r.in)
		if end < 0 {
			return
		}
		event := r.in[:end+sep]
		r.in = r.in[end+sep:]
		r.handle(event)
	}
}

// eventEnd finds the blank line that ends the first event in b.
func eventEnd(b []byte) (index, separator int) {
	lf, crlf := bytes.Index(b, []byte("\n\n")), bytes.Index(b, []byte("\r\n\r\n"))
	switch {
	case lf < 0 && crlf < 0:
		return -1, 0
	case crlf >= 0 && (lf < 0 || crlf < lf):
		return crlf, 4
	}
	return lf, 2
}

func (r *Restorer) handle(event []byte) {
	data := dataOf(event)
	// The common case by far: nothing about this event can involve a
	// placeholder, so it goes through untouched.
	if !bytes.Contains(data, []byte(`"content_block_`)) {
		r.out = append(r.out, event...)
		return
	}
	var ev struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type        string  `json:"type"`
			Text        *string `json:"text"`
			Thinking    *string `json:"thinking"`
			PartialJSON *string `json:"partial_json"`
		} `json:"delta"`
	}
	if json.Unmarshal(data, &ev) != nil {
		r.out = append(r.out, event...)
		return
	}

	switch ev.Type {
	case "content_block_delta":
		var piece *string
		switch ev.Delta.Type {
		case "text_delta":
			piece = ev.Delta.Text
		case "thinking_delta":
			piece = ev.Delta.Thinking
		case "input_json_delta":
			piece = ev.Delta.PartialJSON
		}
		if piece == nil {
			r.out = append(r.out, event...) // a signature, or a kind not known
			return
		}
		r.kind[ev.Index] = ev.Delta.Type
		work := r.pending[ev.Index] + *piece
		hold := heldBack(work)
		if r.pending[ev.Index] == "" && hold == 0 && !strings.Contains(strings.ToUpper(work), placeholderPrefix) {
			r.out = append(r.out, event...)
			return
		}
		ready := work[:len(work)-hold]
		r.pending[ev.Index] = work[len(work)-hold:]
		r.emit(ev.Index, ev.Delta.Type, ready)

	case "content_block_stop":
		// Whatever was held back never became a placeholder. If it is
		// only ordinary text that happened to look like the start of one,
		// it is released; if it is a cut-off placeholder, that is an error.
		if held := r.pending[ev.Index]; held != "" {
			delete(r.pending, ev.Index)
			r.emit(ev.Index, r.kind[ev.Index], held)
		}
		if !r.stopped {
			r.out = append(r.out, event...)
		}

	default:
		r.out = append(r.out, event...)
	}
}

// emit restores secrets in a piece of a block and writes it as an event.
func (r *Restorer) emit(index int, deltaType, piece string) {
	if piece == "" {
		return
	}
	escape := func(s string) string { return s }
	field := "text"
	switch deltaType {
	case "thinking_delta":
		field = "thinking"
	case "input_json_delta":
		// The piece is JSON text, and the placeholder sits inside one of
		// its strings, so the secret has to be escaped to sit there too.
		field, escape = "partial_json", jsonEscape
	}
	restored, n, err := r.rd.restoreIn(piece, escape)
	if err == nil && damaged(restored, r.rd) {
		err = ErrUnresolvable
	}
	// Prose is read, not run. A token janus cannot restore is only
	// dangerous in a tool call, where it would become a command or an
	// edit; in text or thinking it is left as the model wrote it.
	if err == ErrUnresolvable && deltaType != "input_json_delta" {
		err = nil
	}
	if err != nil {
		r.fail(err)
		return
	}
	r.Restored += n

	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]string{"type": deltaType, field: restored},
	})
	r.out = append(r.out, "event: content_block_delta\ndata: "...)
	r.out = append(r.out, bytes.TrimRight(b.Bytes(), "\n")...)
	r.out = append(r.out, "\n\n"...)
}

// fail stops the stream with an error event. What has already been sent
// cannot be taken back, but a client treats a stream that ends in an error
// as a failed turn and does not act on it.
func (r *Restorer) fail(err error) {
	r.Err = err
	r.stopped = true
	r.in = nil
	msg, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "api_error", "message": fmt.Sprintf("janus: %v. The turn was stopped so that nothing acts on it; try again.", err)},
	})
	r.out = append(r.out, "event: error\ndata: "...)
	r.out = append(r.out, msg...)
	r.out = append(r.out, "\n\n"...)
}

// dataOf is the data of an event: its "data:" lines, joined.
func dataOf(event []byte) []byte {
	var data []byte
	for _, line := range bytes.Split(event, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			if data != nil {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimPrefix(rest, []byte(" "))...)
		}
	}
	return data
}

// heldBack is the length of the longest tail of s that could be the start
// of a placeholder and is not yet a whole one.
func heldBack(s string) int {
	whole := len(placeholderPrefix) + placeholderIDLength
	for k := min(len(s), whole-1); k > 0; k-- {
		if couldBegin(s[len(s)-k:]) {
			return k
		}
	}
	return 0
}

// couldBegin reports whether t is a proper beginning of a placeholder.
func couldBegin(t string) bool {
	if len(t) <= len(placeholderPrefix) {
		return strings.HasPrefix(placeholderPrefix, t)
	}
	if !strings.HasPrefix(t, placeholderPrefix) {
		return false
	}
	for _, c := range t[len(placeholderPrefix):] {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
