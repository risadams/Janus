package proxy

import (
	"compress/gzip"
	"io"
	"strings"

	"github.com/risadams/janus/internal/anthropic"
)

// observation reads a copy of a response body as the original is relayed.
// The client always gets the upstream's bytes; if those are compressed, the
// copy is decompressed on the side so the observer can still read it.
type observation struct {
	observer *anthropic.Observer

	// For a compressed body: the copy is written into pipe, and a goroutine
	// decompresses from the other end into the observer.
	pipe   *io.PipeWriter
	done   chan struct{}
	failed error
}

// newObservation returns nil when the encoding is one janus cannot decode.
// Only gzip is supported: it is what the Anthropic API sends Claude Code,
// and the standard library has no brotli or zstd.
func newObservation(contentType, contentEncoding string) *observation {
	o := &observation{observer: anthropic.NewObserver(contentType)}
	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "", "identity":
		return o
	case "gzip":
	default:
		return nil
	}

	pr, pw := io.Pipe()
	o.pipe, o.done = pw, make(chan struct{})
	go func() {
		defer close(o.done)
		zr, err := gzip.NewReader(pr)
		if err == nil {
			_, err = io.Copy(o.observer, zr)
		}
		o.failed = err
		// Keep reading whatever happens, or the relay would block on a
		// pipe nobody is draining.
		io.Copy(io.Discard, pr)
	}()
	return o
}

func (o *observation) Write(p []byte) (int, error) {
	if o.pipe == nil {
		return o.observer.Write(p)
	}
	o.pipe.Write(p)
	return len(p), nil
}

// finish ends the observation and reports what was read. unread is set if
// the copy could not be decompressed.
func (o *observation) finish() (summary anthropic.Summary, unread string) {
	if o.pipe != nil {
		o.pipe.Close()
		<-o.done
		// A response cut off part-way ends the gzip stream early. That is
		// an incomplete response, which the summary already says; it is not
		// a failure to read.
		if o.failed != nil && o.failed != io.ErrUnexpectedEOF && o.failed != io.EOF {
			return anthropic.Summary{}, "gzip: " + o.failed.Error()
		}
	}
	return o.observer.Summary(), ""
}
