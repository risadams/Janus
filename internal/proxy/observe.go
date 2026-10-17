package proxy

import (
	"compress/gzip"
	"fmt"
	"io"
	"strings"

	"github.com/risadams/janus/internal/anthropic"
)

// observation reads a copy of a response body as the original is relayed.
// The client always gets the upstream's bytes; if those are compressed, the
// copy is decompressed on the side so it can still be read.
//
// The copy goes to up to two places: an Observer that summarises a Messages
// response, and a buffer that keeps the body for capture.
type observation struct {
	observer *anthropic.Observer
	kept     *cappedBuffer

	// For a compressed body: the copy is written into pipe, and a goroutine
	// decompresses from the other end.
	pipe   *io.PipeWriter
	done   chan struct{}
	failed error
}

// maxDecompressed bounds how much of a gzip body is decompressed for the
// copy. The Observer and the kept body are capped already, but each
// content block an event stream starts still adds to the summary, and the
// decompressing goes on as long as the body does, so without a bound a
// small body that expands enormously (gzip runs to about 1000 to 1) costs
// memory and CPU on the side of every relay. A Messages response at the
// largest output any model allows is a few tens of megabytes as an event
// stream, well inside this. It is a variable so that tests can lower it;
// the client gets every byte the upstream sends either way.
var maxDecompressed int64 = 256 << 20

// newObservation sets up reading a response. unsupported is true when the
// content encoding is one janus cannot decode: then nothing is summarised,
// and a kept body holds the bytes as they arrived. Only gzip is supported:
// it is what the Anthropic API sends Claude Code, and the standard library
// has no brotli or zstd.
func newObservation(contentType, contentEncoding string, summarise, keep bool) (o *observation, unsupported bool) {
	o = &observation{}
	if keep {
		o.kept = &cappedBuffer{limit: maxKept}
	}

	encoding := strings.ToLower(strings.TrimSpace(contentEncoding))
	if encoding != "" && encoding != "identity" && encoding != "gzip" {
		return o, true
	}
	if summarise {
		o.observer = anthropic.NewObserver(contentType)
	}
	if encoding == "gzip" {
		pr, pw := io.Pipe()
		o.pipe, o.done = pw, make(chan struct{})
		go func() {
			defer close(o.done)
			zr, err := gzip.NewReader(pr)
			if err == nil {
				var n int64
				n, err = io.Copy(sinkWriter{o}, io.LimitReader(zr, maxDecompressed+1))
				if err == nil && n > maxDecompressed {
					err = fmt.Errorf("the body is more than %d MiB once decompressed, so it was not read", maxDecompressed>>20)
				}
			}
			o.failed = err
			// Keep reading whatever happens, or the relay would block on
			// a pipe nobody is draining. Errors here are the relay's to
			// report, not this copy's.
			_, _ = io.Copy(io.Discard, pr)
		}()
	}
	return o, false
}

// Write takes the next piece of the body as it arrived from upstream.
func (o *observation) Write(p []byte) (int, error) {
	if o.pipe != nil {
		// The pipe fails only once the reader is gone, and the reader
		// drains to the end whatever happens.
		_, _ = o.pipe.Write(p)
		return len(p), nil
	}
	o.sink(p)
	return len(p), nil
}

// sink receives the body in readable form.
func (o *observation) sink(p []byte) {
	// Neither fails a write: the Observer and the capped buffer only ever
	// stop keeping what they are given.
	if o.observer != nil {
		_, _ = o.observer.Write(p)
	}
	if o.kept != nil {
		_, _ = o.kept.Write(p)
	}
}

type sinkWriter struct{ o *observation }

func (w sinkWriter) Write(p []byte) (int, error) {
	w.o.sink(p)
	return len(p), nil
}

// finish ends the observation. summary is nil if this response was not
// being summarised; unread is set if the copy could not be decompressed.
func (o *observation) finish() (summary *anthropic.Summary, unread string) {
	if o.pipe != nil {
		_ = o.pipe.Close() // a PipeWriter's Close always succeeds
		<-o.done
		// A response cut off part-way ends the gzip stream early. That is
		// an incomplete response, which the summary already says; it is not
		// a failure to read.
		if o.failed != nil && o.failed != io.ErrUnexpectedEOF && o.failed != io.EOF {
			return nil, "gzip: " + o.failed.Error()
		}
	}
	if o.observer == nil {
		return nil, ""
	}
	s := o.observer.Summary()
	return &s, ""
}
