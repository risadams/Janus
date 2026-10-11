package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func quiet(string, ...any) {}

// start runs janus in front of the given upstream handler and returns
// janus's URL.
func start(t *testing.T, upstream http.Handler, basePath string) string {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	u, err := url.Parse(up.URL + basePath)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(New(u, quiet))
	t.Cleanup(front.Close)
	return front.URL
}

// plainClient does no transparent decompression, so tests see the bytes
// janus actually sent.
var plainClient = &http.Client{Transport: &http.Transport{DisableCompression: true}}

func TestForwardsRequestUntouched(t *testing.T) {
	var got *http.Request
	var gotBody []byte
	front := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		gotBody, _ = io.ReadAll(r.Body)
	}), "")

	body := `{"model":"claude-opus-5-5","stream":true}`
	req, _ := http.NewRequest("POST", front+"/v1/messages?beta=true", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("X-Api-Key", "sk-secret")
	req.Header.Set("Anthropic-Beta", "a,b")
	req.Header.Set("Connection", "X-Hop")
	req.Header.Set("X-Hop", "dropped")
	resp, err := plainClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got.Method != "POST" || got.URL.Path != "/v1/messages" || got.URL.RawQuery != "beta=true" {
		t.Errorf("upstream saw %s %s?%s", got.Method, got.URL.Path, got.URL.RawQuery)
	}
	if string(gotBody) != body {
		t.Errorf("upstream body = %q, want %q", gotBody, body)
	}
	for h, want := range map[string]string{
		"Authorization":  "Bearer secret-token",
		"X-Api-Key":      "sk-secret",
		"Anthropic-Beta": "a,b",
	} {
		if got.Header.Get(h) != want {
			t.Errorf("upstream %s = %q, want %q", h, got.Header.Get(h), want)
		}
	}
	if got.Header.Get("X-Hop") != "" {
		t.Error("a header named in Connection was forwarded")
	}
	if got.Header.Get("X-Forwarded-For") != "" {
		t.Error("X-Forwarded-For was added; upstream should not learn it is behind a proxy")
	}
	if !strings.HasPrefix(got.Host, "127.0.0.1:") || "http://"+got.Host == front {
		t.Errorf("upstream Host = %q, want the upstream's own address", got.Host)
	}
}

func TestJoinsUpstreamBasePath(t *testing.T) {
	var path string
	front := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
	}), "/gateway/")

	resp, err := plainClient.Get(front + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if path != "/gateway/v1/messages" {
		t.Errorf("upstream path = %q, want /gateway/v1/messages", path)
	}
}

func TestRelaysResponseUntouched(t *testing.T) {
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	zw.Write([]byte(`{"type":"message"}`))
	zw.Close()

	front := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Request-Id", "req_123")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write(zipped.Bytes())
	}), "")

	// No Accept-Encoding from the client: the transport janus uses must not
	// add one and then decode the answer.
	resp, err := plainClient.Get(front + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Request-Id") != "req_123" || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("response headers changed: %v", resp.Header)
	}
	if !bytes.Equal(body, zipped.Bytes()) {
		t.Error("response body bytes changed in transit")
	}
}

func TestStreamsAsChunksArrive(t *testing.T) {
	release := make(chan struct{})
	front := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "event: message_stop\n\n")
	}), "")

	resp, err := plainClient.Get(front + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// The first event must be readable while the upstream is still holding
	// the response open. If janus buffered, this read would block until the
	// deadline.
	first := make([]byte, len("event: message_start\n\n"))
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(resp.Body, first); done <- err }()
	select {
	case err := <-done:
		if err != nil || string(first) != "event: message_start\n\n" {
			t.Fatalf("first chunk = %q, err = %v", first, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first event did not arrive before the upstream finished: the proxy is buffering")
	}

	close(release)
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "event: message_stop\n\n" {
		t.Errorf("rest of stream = %q", rest)
	}
}

func TestClientDisconnectCancelsUpstream(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	front := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}), "")

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", front+"/v1/messages", nil)
	resp, err := plainClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-started
	cancel()

	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request kept running after the client disconnected")
	}
}

func TestUpstreamUnreachable(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	u, _ := url.Parse(dead.URL)
	dead.Close()
	front := httptest.NewServer(New(u, quiet))
	defer front.Close()

	resp, err := plainClient.Get(front.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	var body struct {
		Type  string
		Error struct{ Type, Message string }
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if body.Type != "error" || body.Error.Type != "api_error" || !strings.Contains(body.Error.Message, "janus") {
		t.Errorf("error body = %+v, want an Anthropic-shaped error that names janus", body)
	}
}

func TestLogsOneLinePerRequest(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)

	// The line is written after the response has gone out, so wait for it
	// rather than reading shared state as soon as the client returns.
	lines := make(chan string, 2)
	front := httptest.NewServer(New(u, func(f string, a ...any) { lines <- fmt.Sprintf(f, a...) }))
	defer front.Close()

	resp, err := plainClient.Get(front.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case line := <-lines:
		if !strings.HasPrefix(line, "GET /v1/models -> 418") {
			t.Errorf("log line = %q, want it to start GET /v1/models -> 418", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no log line was written for the request")
	}
	select {
	case extra := <-lines:
		t.Errorf("a second log line was written: %q", extra)
	case <-time.After(50 * time.Millisecond):
	}
}
