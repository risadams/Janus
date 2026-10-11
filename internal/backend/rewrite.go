package backend

import (
	"bytes"
	"io"
	"strings"

	"github.com/risadams/janus/internal/anthropic"
)

// RenameModel wraps a Messages response body so that the client sees the
// model name it asked for rather than the local model's id. Only the one
// place the name appears is touched: the model field of a JSON message, or
// of a stream's message_start event.
//
// A stream is rewritten as it flows: nothing is held back beyond the
// current line, and once the name has been replaced the rest passes
// through untouched.
func RenameModel(body io.ReadCloser, contentType, model string) io.ReadCloser {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream") {
		return &streamRenamer{src: body, model: model}
	}
	return &jsonRenamer{src: body, model: model}
}

// maxRename bounds how much a renamer will hold while looking for the
// model name. Past it, it gives up and passes everything through.
const maxRename = 8 << 20

type streamRenamer struct {
	src   io.ReadCloser
	model string

	pending []byte // an unfinished line from the source
	out     []byte // rewritten bytes not yet handed to the reader
	done    bool   // the name has been replaced, or the search abandoned
	err     error
}

func (r *streamRenamer) Read(p []byte) (int, error) {
	for len(r.out) == 0 && r.err == nil {
		buf := make([]byte, 32<<10)
		n, err := r.src.Read(buf)
		r.err = err
		r.feed(buf[:n])
		if err != nil {
			// Whatever is left never got its newline; pass it on as it is.
			r.out = append(r.out, r.pending...)
			r.pending = nil
		}
	}
	if len(r.out) == 0 {
		return 0, r.err
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

func (r *streamRenamer) feed(chunk []byte) {
	if r.done {
		r.out = append(r.out, chunk...)
		return
	}
	r.pending = append(r.pending, chunk...)
	for !r.done {
		i := bytes.IndexByte(r.pending, '\n')
		if i < 0 {
			if len(r.pending) > maxRename {
				r.done = true
			}
			break
		}
		line := r.pending[:i+1]
		r.out = append(r.out, r.rewriteLine(line)...)
		r.pending = r.pending[i+1:]
	}
	if r.done {
		r.out = append(r.out, r.pending...)
		r.pending = nil
	}
}

// rewriteLine replaces the model in a "data:" line holding message_start,
// keeping the line's own prefix and ending.
func (r *streamRenamer) rewriteLine(line []byte) []byte {
	payload, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok || !bytes.Contains(payload, []byte("message_start")) {
		return line
	}
	body := bytes.TrimRight(payload, "\r\n")
	ending := payload[len(body):]
	trimmed := bytes.TrimLeft(body, " ")
	lead := body[:len(body)-len(trimmed)]

	rewritten, ok := anthropic.SetResponseModel(trimmed, r.model)
	if !ok {
		return line
	}
	r.done = true
	out := append([]byte("data:"), lead...)
	out = append(out, rewritten...)
	return append(out, ending...)
}

func (r *streamRenamer) Close() error { return r.src.Close() }

// jsonRenamer reads a whole JSON body before releasing any of it, since
// the model field can be anywhere in it.
type jsonRenamer struct {
	src   io.ReadCloser
	model string
	out   io.Reader
}

func (r *jsonRenamer) Read(p []byte) (int, error) {
	if r.out == nil {
		data, err := io.ReadAll(io.LimitReader(r.src, maxRename+1))
		switch {
		case err != nil:
			// Pass on what arrived, then the failure.
			r.out = io.MultiReader(bytes.NewReader(data), failing{err})
		case len(data) > maxRename:
			// Too large to be a message: hand it on unchanged, followed
			// by whatever the limit left unread.
			r.out = io.MultiReader(bytes.NewReader(data), r.src)
		default:
			if rewritten, ok := anthropic.SetResponseModel(data, r.model); ok {
				data = rewritten
			}
			r.out = bytes.NewReader(data)
		}
	}
	return r.out.Read(p)
}

func (r *jsonRenamer) Close() error { return r.src.Close() }

type failing struct{ err error }

func (f failing) Read([]byte) (int, error) { return 0, f.err }
