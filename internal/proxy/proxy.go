package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/risadams/janus/internal/anthropic"
)

// Exchange describes one request janus handled, reported when it is over.
type Exchange struct {
	Method   string
	Path     string
	Status   int
	Duration time.Duration
	// Summary is what janus read from a Messages API response as it went
	// past: model, stop reason, token usage. It is nil for every other kind
	// of request.
	Summary *anthropic.Summary
	// Unread says why a Messages response has no Summary, when that is the
	// case: for example a content encoding janus cannot decode. The
	// response was still relayed.
	Unread string
}

// exchangeState carries what the response hook learns back to the handler
// that started the request.
type exchangeState struct {
	messages    bool
	observation *observation
	unread      string
}

type stateKey struct{}

// New returns a handler that forwards every request to upstream and relays
// the response as it arrives. Nothing in either direction is altered beyond
// what HTTP requires of a proxy: hop-by-hop headers are dropped and Host is
// set to the upstream's. Credentials pass through as the client sent them.
//
// Messages API responses are also read on their way past, without being
// delayed or changed, so that done can be told what they contained. done is
// called once per request, after the response has ended.
func New(upstream *url.URL, done func(Exchange)) http.Handler {
	// The default transport asks for gzip and decodes it when the client
	// did not ask for compression itself. Turning that off keeps the
	// response body exactly as the upstream sent it.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true

	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
		},
		Transport: transport,
		// Flush after every write, so a streamed completion reaches the
		// client token by token rather than when a buffer fills.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			state, _ := resp.Request.Context().Value(stateKey{}).(*exchangeState)
			if state == nil || !state.messages {
				return nil
			}
			state.observation = newObservation(resp.Header.Get("Content-Type"), resp.Header.Get("Content-Encoding"))
			if state.observation == nil {
				state.unread = "content-encoding " + resp.Header.Get("Content-Encoding")
				return nil
			}
			resp.Body = &observedBody{ReadCloser: resp.Body, observation: state.observation}
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
		state := &exchangeState{messages: r.Method == http.MethodPost && r.URL.Path == "/v1/messages"}
		sw := &statusWriter{ResponseWriter: w}

		// Deferred because the reverse proxy aborts a response that fails
		// part-way by panicking; the exchange still has to be reported.
		defer func() {
			ex := Exchange{
				Method:   r.Method,
				Path:     r.URL.Path,
				Status:   sw.status,
				Duration: time.Since(start),
				Unread:   state.unread,
			}
			if state.observation != nil {
				if summary, unread := state.observation.finish(); unread != "" {
					ex.Unread = unread
				} else {
					ex.Summary = &summary
				}
			}
			done(ex)
		}()
		rp.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), stateKey{}, state)))
	})
}

// observedBody passes a response body through unchanged while showing a
// copy of each read to the observer.
type observedBody struct {
	io.ReadCloser
	observation *observation
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.observation.Write(p[:n])
	}
	return n, err
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
