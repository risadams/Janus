package proxy

import (
	"compress/gzip"
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
				_, err = io.Copy(sinkWriter{o}, zr)
			}
			o.failed = err
			// Keep reading whatever happens, or the relay would block on
			// a pipe nobody is draining.
			io.Copy(io.Discard, pr)
		}()
	}
	return o, false
}

// Write takes the next piece of the body as it arrived from upstream.
func (o *observation) Write(p []byte) (int, error) {
	if o.pipe != nil {
		o.pipe.Write(p)
		return len(p), nil
	}
	o.sink(p)
	return len(p), nil
}

// sink receives the body in readable form.
func (o *observation) sink(p []byte) {
	if o.observer != nil {
		o.observer.Write(p)
	}
	if o.kept != nil {
		o.kept.Write(p)
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
		o.pipe.Close()
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
