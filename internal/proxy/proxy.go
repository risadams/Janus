package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// New returns a handler that forwards every request to upstream and relays
// the response as it arrives. Nothing in either direction is inspected or
// altered beyond what HTTP requires of a proxy: hop-by-hop headers are
// dropped and Host is set to the upstream's. Credentials pass through as
// the client sent them.
//
// logf receives one line per request; pass log.Printf, or a no-op in tests.
func New(upstream *url.URL, logf func(format string, args ...any)) http.Handler {
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
		sw := &statusWriter{ResponseWriter: w}
		rp.ServeHTTP(sw, r)
		logf("%s %s -> %d (%s)", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
	})
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
