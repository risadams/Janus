package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
)

// Fetched is a backend's whole answer to a Messages request, held in memory
// rather than relayed.
type Fetched struct {
	// Err is set when no response came back at all.
	Err    error
	Status int
	Header http.Header
	// Body is the response body, up to the limit Fetch was given;
	// Truncated says it was longer.
	Body      []byte
	Truncated bool
	// Fault is what went wrong reading the body, if it broke off.
	Fault    string
	Summary  anthropic.Summary
	TTFB     time.Duration
	Duration time.Duration
}

// Fetch sends a Messages request to a backend and reads the whole answer.
// It is for the cases where the answer must be seen before anyone decides
// what to do with it: a local-first trial, a shadow run, a replay.
//
// header is the client's headers; the backend's Prepare decides what of
// them the backend may see. limit bounds how much of the body is kept.
func Fetch(ctx context.Context, client *http.Client, b backend.Backend, header http.Header, body []byte, limit int) Fetched {
	start := time.Now()
	out, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(b.URL().String(), "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Fetched{Err: err}
	}
	for _, name := range []string{"Content-Type", "Anthropic-Version", "Anthropic-Beta", "Authorization", "X-Api-Key", "Accept-Encoding"} {
		if values := header.Values(name); len(values) > 0 {
			out.Header[name] = append([]string(nil), values...)
		}
	}
	if out.Header.Get("Content-Type") == "" {
		out.Header.Set("Content-Type", "application/json")
	}
	b.Prepare(out, true)

	resp, err := client.Do(out)
	if err != nil {
		return Fetched{Err: err, Duration: time.Since(start)}
	}
	defer resp.Body.Close()

	f := Fetched{Status: resp.StatusCode, Header: resp.Header}
	observation, unsupported := newObservation(resp.Header.Get("Content-Type"), resp.Header.Get("Content-Encoding"), true, false)
	var kept bytes.Buffer
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if f.TTFB == 0 {
				f.TTFB = time.Since(start)
			}
			observation.Write(buf[:n])
			if room := limit - kept.Len(); n > room {
				kept.Write(buf[:max(room, 0)])
				f.Truncated = true
			} else {
				kept.Write(buf[:n])
			}
		}
		if err != nil {
			if err != io.EOF {
				f.Fault = err.Error()
			}
			break
		}
	}
	if summary, unread := observation.finish(); summary != nil && unread == "" && !unsupported {
		f.Summary = *summary
	}
	f.Body, f.Duration = kept.Bytes(), time.Since(start)
	return f
}
