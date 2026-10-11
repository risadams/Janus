package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/risadams/janus/internal/anthropic"
)

// Exchange describes one request janus handled, reported when it is over.
type Exchange struct {
	// ID is unique to this request and sorts by time. Captures and ledger
	// records for the same request share it.
	ID     string
	Start  time.Time
	Method string
	Path   string
	// Header is the request's headers as the client sent them, credentials
	// included. Whoever receives an Exchange is responsible for not writing
	// those anywhere readable.
	Header http.Header
	// Status is the response status, or 0 if no response was started.
	Status int
	// TTFB is the time until the first byte of the response body arrived
	// from upstream; zero if there was none.
	TTFB     time.Duration
	Duration time.Duration
	// Cancelled means the client went away before the response finished.
	Cancelled bool

	// Summary is what janus read from a Messages API response as it went
	// past: model, stop reason, token usage. It is nil for every other kind
	// of request.
	Summary *anthropic.Summary
	// Unread says why a Messages response has no Summary, when that is the
	// case: for example a content encoding janus cannot decode. The
	// response was still relayed.
	Unread string

	// RequestBody is kept for Messages requests always, and for everything
	// else only when Options.KeepBodies is set. ResponseBody is kept only
	// with KeepBodies; it is decompressed where janus can do that, and
	// otherwise the bytes as they arrived. Either may be cut short at
	// maxKept, which the Truncated flags report.
	RequestBody       []byte
	RequestTruncated  bool
	ResponseHeader    http.Header
	ResponseBody      []byte
	ResponseTruncated bool
}

// IsMessages reports whether this was a call to the Messages API itself,
// as opposed to token counting, model listing, or anything else.
func (ex Exchange) IsMessages() bool {
	return ex.Method == http.MethodPost && ex.Path == "/v1/messages"
}

// Options configures the proxy handler.
type Options struct {
	Upstream *url.URL
	// Done is called once per request, after the response has ended.
	Done func(Exchange)
	// KeepBodies makes every Exchange carry its request and response
	// bodies, for capture logging. Off, only Messages request bodies are
	// kept, which the ledger needs.
	KeepBodies bool
}

// maxKept bounds how much of one body janus holds in memory. Past it the
// traffic is still relayed in full; only the kept copy stops growing.
const maxKept = 32 << 20

// exchangeState carries what the response hook learns back to the handler
// that started the request.
type exchangeState struct {
	messages       bool
	keep           bool
	observation    *observation
	unread         string
	responseHeader http.Header
	firstByte      time.Time
}

type stateKey struct{}

// New returns a handler that forwards every request to the upstream and
// relays the response as it arrives. Nothing in either direction is altered
// beyond what HTTP requires of a proxy (hop-by-hop headers are dropped and
// Host is set to the upstream's), with one exception: Accept-Encoding on
// Messages requests, see narrowEncoding. Credentials pass through as the
// client sent them.
//
// Traffic is also read on its way past, without being delayed or changed,
// so that Done can be told what happened.
func New(opts Options) http.Handler {
	// The default transport asks for gzip and decodes it when the client
	// did not ask for compression itself. Turning that off keeps the
	// response body exactly as the upstream sent it.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true

	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(opts.Upstream)
			if isMessages(r.In) {
				narrowEncoding(r.Out.Header)
			}
		},
		Transport: transport,
		// Flush after every write, so a streamed completion reaches the
		// client token by token rather than when a buffer fills.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			state, _ := resp.Request.Context().Value(stateKey{}).(*exchangeState)
			if state == nil {
				return nil
			}
			state.responseHeader = resp.Header.Clone()
			if state.messages || state.keep {
				var unsupported bool
				state.observation, unsupported = newObservation(
					resp.Header.Get("Content-Type"), resp.Header.Get("Content-Encoding"), state.messages, state.keep)
				if unsupported && state.messages {
					state.unread = "content-encoding " + resp.Header.Get("Content-Encoding")
				}
			}
			resp.Body = &observedBody{ReadCloser: resp.Body, state: state}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				// The client went away; there is nobody to answer.
				return
			}
			writeError(w, http.StatusBadGateway, "janus: could not reach upstream: "+err.Error())
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		state := &exchangeState{
			messages: isMessages(r),
			keep:     opts.KeepBodies,
		}
		sw := &statusWriter{ResponseWriter: w}

		var requestBody *cappedBuffer
		if (state.messages || state.keep) && r.Body != nil {
			requestBody = &cappedBuffer{limit: maxKept}
			r.Body = &teeBody{ReadCloser: r.Body, copy: requestBody}
		}

		// Deferred because the reverse proxy aborts a response that fails
		// part-way by panicking; the exchange still has to be reported.
		defer func() {
			ex := Exchange{
				ID:             newID(start),
				Start:          start,
				Method:         r.Method,
				Path:           r.URL.Path,
				Header:         r.Header.Clone(),
				Status:         sw.status,
				Duration:       time.Since(start),
				Cancelled:      r.Context().Err() != nil,
				Unread:         state.unread,
				ResponseHeader: state.responseHeader,
			}
			if !state.firstByte.IsZero() {
				ex.TTFB = state.firstByte.Sub(start)
			}
			if requestBody != nil {
				ex.RequestBody, ex.RequestTruncated = requestBody.snapshot()
			}
			if o := state.observation; o != nil {
				summary, unread := o.finish()
				switch {
				case unread != "":
					ex.Unread = unread
				case summary != nil:
					ex.Summary = summary
				}
				if o.kept != nil {
					ex.ResponseBody, ex.ResponseTruncated = o.kept.snapshot()
				}
			}
			opts.Done(ex)
		}()
		rp.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), stateKey{}, state)))
	})
}

func isMessages(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/v1/messages"
}

// narrowEncoding limits what a Messages request asks the upstream to
// compress its response with to what janus can read: gzip, or nothing.
//
// This is the one request header janus changes. The ledger depends on
// reading every response, the standard library cannot decode brotli or
// zstd, and Claude Code offers both. The client still only ever receives
// an encoding it asked for: gzip if it offered gzip, otherwise none.
func narrowEncoding(h http.Header) {
	offered := h.Values("Accept-Encoding")
	if len(offered) == 0 {
		return // no compression was asked for, and none will be used
	}
	if acceptsGzip(offered) {
		h.Set("Accept-Encoding", "gzip")
	} else {
		h.Set("Accept-Encoding", "identity")
	}
}

// acceptsGzip reports whether an Accept-Encoding header allows gzip, either
// by name or through the wildcard, and not with a quality of zero.
func acceptsGzip(values []string) bool {
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
			coding = strings.ToLower(strings.TrimSpace(coding))
			if coding != "gzip" && coding != "x-gzip" && coding != "*" {
				continue
			}
			q := strings.TrimSpace(params)
			if weight, ok := strings.CutPrefix(q, "q="); ok {
				if f, err := strconv.ParseFloat(strings.TrimSpace(weight), 64); err == nil && f == 0 {
					continue
				}
			}
			return true
		}
	}
	return false
}

// newID is a timestamp to the millisecond plus a random suffix: unique
// enough for one machine, and it sorts in the order requests began.
func newID(t time.Time) string {
	var suffix [3]byte
	rand.Read(suffix[:])
	return t.UTC().Format("20060102T150405.000") + "-" + hex.EncodeToString(suffix[:])
}

// observedBody passes a response body through unchanged, noting when its
// first byte arrives and showing a copy of each read to the observation.
type observedBody struct {
	io.ReadCloser
	state *exchangeState
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		if b.state.firstByte.IsZero() {
			b.state.firstByte = time.Now()
		}
		if b.state.observation != nil {
			b.state.observation.Write(p[:n])
		}
	}
	return n, err
}

// teeBody passes a request body through unchanged while keeping a copy.
type teeBody struct {
	io.ReadCloser
	copy *cappedBuffer
}

func (b *teeBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.copy.Write(p[:n])
	}
	return n, err
}

// cappedBuffer keeps what is written to it up to a limit and counts the
// rest as truncated. It is locked because the transport reads a request
// body on its own goroutine, which may still be running when the handler
// reports the exchange.
type cappedBuffer struct {
	mu        sync.Mutex
	limit     int
	data      []byte
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - len(b.data); len(p) > room {
		b.data = append(b.data, p[:room]...)
		b.truncated = true
	} else {
		b.data = append(b.data, p...)
	}
	return len(p), nil
}

func (b *cappedBuffer) snapshot() (data []byte, truncated bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.data), b.truncated
}

// writeError answers in the Anthropic API's error shape, so a client built
// for that API reports janus's own failures the way it reports upstream
// ones.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "api_error", "message": message},
	})
}

// statusWriter remembers the status code for the request log. Unwrap lets
// http.ResponseController reach the real writer, which is how the reverse
// proxy flushes.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
