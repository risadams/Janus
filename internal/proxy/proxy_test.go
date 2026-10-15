package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func quiet(Exchange) {}

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
	front := httptest.NewServer(New(Options{Upstream: backend.NewUpstream(u, nil), Done: quiet}))
	t.Cleanup(front.Close)
	return front.URL
}

// plainClient does no transparent decompression, so tests see the bytes
// janus actually sent.
var plainClient = &http.Client{Transport: &http.Transport{DisableCompression: true}}

// unreachable is the address of an upstream that refuses every connection.
// Nothing listens on port 1, and it is never given to a test server. A port
// freed by closing a server is not safe: the next server started can be
// given it, and a janus whose upstream is itself forwards to itself until
// the test hangs (#211).
func unreachable() *url.URL {
	u, _ := url.Parse("http://127.0.0.1:1")
	return u
}

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
	u := unreachable()
	front := httptest.NewServer(New(Options{Upstream: backend.NewUpstream(u, nil), Done: quiet}))
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

// The ledger and the capture must say why janus answered 502, which no
// backend did: on 2026-10-12 nine of them said nothing (#95).
func TestUpstreamUnreachableIsExplained(t *testing.T) {
	u := unreachable()
	reported := make(chan Exchange, 1)
	front := httptest.NewServer(New(Options{Upstream: backend.NewUpstream(u, nil), Done: func(ex Exchange) { reported <- ex }, KeepBodies: true}))
	defer front.Close()

	resp, err := plainClient.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	sent, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var ex Exchange
	select {
	case ex = <-reported:
	case <-time.After(3 * time.Second):
		t.Fatal("no exchange was reported for the request")
	}
	if !strings.HasPrefix(ex.Fault, "could not reach upstream: ") {
		t.Errorf("Fault = %q, want why upstream could not be reached", ex.Fault)
	}
	if !ex.BackendFault {
		t.Error("BackendFault is not set")
	}
	if !bytes.Equal(ex.ResponseBody, sent) {
		t.Errorf("kept response body = %q, want the error janus sent: %q", ex.ResponseBody, sent)
	}
	if ct := ex.ResponseHeader.Get("Content-Type"); ct != "application/json" {
		t.Errorf("kept response Content-Type = %q, want application/json", ct)
	}
}

// serve runs janus in front of upstream, makes one request, and returns the
// response body with the Exchange janus reported for it.
func serve(t *testing.T, upstream http.HandlerFunc, method, path string) ([]byte, Exchange) {
	t.Helper()
	return serveWith(t, upstream, method, path, false)
}

func serveWith(t *testing.T, upstream http.HandlerFunc, method, path string, keepBodies bool) ([]byte, Exchange) {
	t.Helper()
	up := httptest.NewServer(upstream)
	defer up.Close()
	u, _ := url.Parse(up.URL)

	// The exchange is reported after the response has gone out, so wait
	// for it rather than reading shared state when the client returns.
	reported := make(chan Exchange, 2)
	front := httptest.NewServer(New(Options{Upstream: backend.NewUpstream(u, nil), Done: func(ex Exchange) { reported <- ex }, KeepBodies: keepBodies}))
	defer front.Close()

	req, _ := http.NewRequest(method, front.URL+path, strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, err := plainClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	select {
	case ex := <-reported:
		select {
		case extra := <-reported:
			t.Errorf("a second exchange was reported: %+v", extra)
		case <-time.After(50 * time.Millisecond):
		}
		return body, ex
	case <-time.After(3 * time.Second):
		t.Fatal("no exchange was reported for the request")
		return nil, Exchange{}
	}
}

const sampleStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5-5","usage":{"input_tokens":2,"cache_read_input_tokens":900,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func TestReportsMessagesUsageWithoutChangingTheStream(t *testing.T) {
	body, ex := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Split mid-event, as a real network would.
		io.WriteString(w, sampleStream[:70])
		w.(http.Flusher).Flush()
		io.WriteString(w, sampleStream[70:])
	}, "POST", "/v1/messages")

	if string(body) != sampleStream {
		t.Error("the stream the client received differs from what the upstream sent")
	}
	// Windows' clock ticks about once a millisecond, so a fast local
	// exchange can take no measurable time; only a negative one is wrong.
	if ex.Method != "POST" || ex.Path != "/v1/messages" || ex.Status != 200 || ex.Duration < 0 {
		t.Errorf("exchange = %+v", ex)
	}
	s := ex.Summary
	if s == nil {
		t.Fatalf("no summary for a Messages response (unread: %q)", ex.Unread)
	}
	if !s.Complete || s.Model != "claude-opus-5-5" || s.StopReason != "end_turn" ||
		s.Usage.Input != 2 || s.Usage.Output != 42 || s.Usage.CacheRead == nil || *s.Usage.CacheRead != 900 {
		t.Errorf("summary = %+v", *s)
	}
}

func TestReportsAPIErrorBody(t *testing.T) {
	_, ex := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}, "POST", "/v1/messages")

	if ex.Status != 429 || ex.Summary == nil || ex.Summary.Error == nil || ex.Summary.Error.Type != "rate_limit_error" {
		t.Errorf("exchange = %+v, summary = %+v", ex, ex.Summary)
	}
}

func TestOnlyMessagesResponsesAreRead(t *testing.T) {
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/models"},
		{"GET", "/v1/messages"},
		{"POST", "/v1/messages/count_tokens"},
		{"HEAD", "/api/hello"},
	} {
		_, ex := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"type":"message","usage":{"input_tokens":1,"output_tokens":1}}`)
		}, c.method, c.path)
		if ex.Summary != nil || ex.Unread != "" {
			t.Errorf("%s %s: summary=%+v unread=%q, want neither", c.method, c.path, ex.Summary, ex.Unread)
		}
		if ex.Status != 200 || ex.Path != c.path {
			t.Errorf("%s %s: exchange = %+v", c.method, c.path, ex)
		}
	}
}

func TestReadsGzipStreamAndRelaysItCompressed(t *testing.T) {
	var sent bytes.Buffer
	body, ex := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		// Compress and flush event by event, as the real API does.
		zw := gzip.NewWriter(io.MultiWriter(w, &sent))
		for _, part := range strings.SplitAfter(sampleStream, "\n\n") {
			io.WriteString(zw, part)
			zw.Flush()
			w.(http.Flusher).Flush()
		}
		zw.Close()
	}, "POST", "/v1/messages")

	if !bytes.Equal(body, sent.Bytes()) {
		t.Error("the compressed bytes the client received differ from what the upstream sent")
	}
	if ex.Summary == nil {
		t.Fatalf("no summary for a gzip response (unread: %q)", ex.Unread)
	}
	if s := ex.Summary; !s.Complete || s.StopReason != "end_turn" || s.Usage.Output != 42 {
		t.Errorf("summary = %+v", *s)
	}
}

func TestCorruptGzipIsRelayedAndReportedUnread(t *testing.T) {
	junk := []byte("this is not gzip at all")
	body, ex := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(junk)
	}, "POST", "/v1/messages")

	if !bytes.Equal(body, junk) {
		t.Error("body changed in transit")
	}
	if ex.Summary != nil || !strings.HasPrefix(ex.Unread, "gzip: ") {
		t.Errorf("summary=%+v unread=%q, want no summary and a gzip reason", ex.Summary, ex.Unread)
	}
}

func TestGzipStreamCutOffIsIncompleteNotUnread(t *testing.T) {
	_, ex := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		io.WriteString(zw, sampleStream[:strings.Index(sampleStream, "event: message_delta")])
		zw.Flush()
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}, "POST", "/v1/messages")

	if ex.Summary == nil || ex.Unread != "" {
		t.Fatalf("summary=%v unread=%q, want a summary of what arrived", ex.Summary, ex.Unread)
	}
	if ex.Summary.Complete || ex.Summary.Model != "claude-opus-5-5" {
		t.Errorf("summary = %+v, want incomplete", *ex.Summary)
	}
}

func TestUnsupportedEncodingIsRelayedButNotRead(t *testing.T) {
	body, ex := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		w.Write([]byte{0x0b, 0x02, 0x80, 0x68, 0x69, 0x03})
	}, "POST", "/v1/messages")

	if !bytes.Equal(body, []byte{0x0b, 0x02, 0x80, 0x68, 0x69, 0x03}) {
		t.Error("compressed body changed in transit")
	}
	if ex.Summary != nil || ex.Unread != "content-encoding br" {
		t.Errorf("summary=%+v unread=%q, want no summary and the reason", ex.Summary, ex.Unread)
	}
}

func TestReportsAStreamThatFailsPartWay(t *testing.T) {
	_, ex := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sampleStream[:strings.Index(sampleStream, "event: message_delta")])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // drop the connection mid-stream
	}, "POST", "/v1/messages")

	if ex.Summary == nil {
		t.Fatal("no summary for a stream that was cut off")
	}
	if ex.Summary.Complete || ex.Summary.Model != "claude-opus-5-5" || len(ex.Summary.Blocks) != 1 {
		t.Errorf("summary = %+v, want incomplete with what arrived before the cut", *ex.Summary)
	}
	if ex.Fault == "" {
		t.Error("a response the backend dropped part-way has no Fault saying so")
	}
}

func TestExchangeIdentifiesAndTimesTheRequest(t *testing.T) {
	before := time.Now()
	_, ex := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Request-Id", "req_abc")
		time.Sleep(20 * time.Millisecond)
		io.WriteString(w, sampleStream)
	}, "POST", "/v1/messages")

	if len(ex.ID) != len("20261011T143927.123-ab12cd") || ex.ID[8] != 'T' || ex.ID[19] != '-' {
		t.Errorf("ID = %q, want a timestamp and a suffix", ex.ID)
	}
	if ex.Start.Before(before) || ex.Start.After(time.Now()) {
		t.Errorf("Start = %v", ex.Start)
	}
	if ex.TTFB < 20*time.Millisecond || ex.TTFB > ex.Duration {
		t.Errorf("TTFB = %v, Duration = %v; want 20ms <= TTFB <= Duration", ex.TTFB, ex.Duration)
	}
	if ex.Header.Get("Authorization") != "Bearer secret-token" {
		t.Errorf("request headers were not carried: %v", ex.Header)
	}
	if ex.ResponseHeader.Get("Request-Id") != "req_abc" {
		t.Errorf("response headers were not carried: %v", ex.ResponseHeader)
	}
	if ex.Cancelled || !ex.IsMessages() {
		t.Errorf("cancelled=%v messages=%v", ex.Cancelled, ex.IsMessages())
	}

	_, other := serve(t, func(http.ResponseWriter, *http.Request) {}, "POST", "/v1/messages")
	if other.ID == ex.ID {
		t.Error("two requests got the same ID")
	}
	if other.TTFB != 0 {
		t.Errorf("TTFB = %v for a response with no body, want 0", other.TTFB)
	}
}

func TestKeepsBodiesOnlyWhereNeeded(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}

	_, ex := serveWith(t, upstream, "POST", "/v1/messages", false)
	if string(ex.RequestBody) != `{"model":"m"}` || ex.ResponseBody != nil {
		t.Errorf("Messages without KeepBodies: request=%q response=%q, want the request only", ex.RequestBody, ex.ResponseBody)
	}

	_, ex = serveWith(t, upstream, "POST", "/v1/messages/count_tokens", false)
	if ex.RequestBody != nil || ex.ResponseBody != nil {
		t.Errorf("other path without KeepBodies: request=%q response=%q, want neither", ex.RequestBody, ex.ResponseBody)
	}

	_, ex = serveWith(t, upstream, "POST", "/v1/messages/count_tokens", true)
	if string(ex.RequestBody) != `{"model":"m"}` || string(ex.ResponseBody) != `{"ok":true}` {
		t.Errorf("with KeepBodies: request=%q response=%q, want both", ex.RequestBody, ex.ResponseBody)
	}
	if ex.Summary != nil {
		t.Error("a non-Messages response was summarised because its body was kept")
	}
}

func TestKeptResponseIsDecompressed(t *testing.T) {
	_, ex := serveWith(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		io.WriteString(zw, sampleStream)
		zw.Close()
	}, "POST", "/v1/messages", true)

	if string(ex.ResponseBody) != sampleStream {
		t.Errorf("kept response = %q, want the decompressed stream", ex.ResponseBody)
	}
	if ex.Summary == nil || ex.Summary.Usage.Output != 42 {
		t.Errorf("summary = %+v", ex.Summary)
	}
}

func TestKeptResponseInUnsupportedEncodingIsRaw(t *testing.T) {
	raw := []byte{0x0b, 0x02, 0x80, 0x68, 0x69, 0x03}
	_, ex := serveWith(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		w.Write(raw)
	}, "POST", "/v1/messages", true)

	if !bytes.Equal(ex.ResponseBody, raw) || ex.Unread != "content-encoding br" || ex.Summary != nil {
		t.Errorf("body=%v unread=%q summary=%v", ex.ResponseBody, ex.Unread, ex.Summary)
	}
}

func TestReportsClientDisconnectAsCancelled(t *testing.T) {
	started := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sampleStream[:70])
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	reported := make(chan Exchange, 1)
	front := httptest.NewServer(New(Options{Upstream: backend.NewUpstream(u, nil), Done: func(ex Exchange) { reported <- ex }}))
	defer front.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", front.URL+"/v1/messages", strings.NewReader("{}"))
	resp, err := plainClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-started
	cancel()

	select {
	case ex := <-reported:
		if !ex.Cancelled || ex.Status != 200 {
			t.Errorf("cancelled=%v status=%d, want a cancelled 200", ex.Cancelled, ex.Status)
		}
		if ex.Fault != "" {
			t.Errorf("Fault = %q; the client leaving is not a fault in the backend", ex.Fault)
		}
		if ex.Summary == nil || ex.Summary.Complete {
			t.Errorf("summary = %+v, want an incomplete one", ex.Summary)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a cancelled request was never reported")
	}
}

func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{limit: 5}
	for _, part := range []string{"abc", "defg", "hi"} {
		if n, err := b.Write([]byte(part)); n != len(part) || err != nil {
			t.Fatalf("Write(%q) = %d, %v; it must always accept everything", part, n, err)
		}
	}
	data, truncated := b.snapshot()
	if string(data) != "abcde" || !truncated {
		t.Errorf("snapshot = %q truncated=%v, want abcde and true", data, truncated)
	}
}

func TestNarrowsAcceptEncodingOnMessagesOnly(t *testing.T) {
	cases := []struct {
		name, method, path string
		offered            []string // nil means the header is not sent
		want               string   // what the upstream should see; "" means no header
	}{
		{"claude code's list", "POST", "/v1/messages", []string{"gzip, deflate, br, zstd"}, "gzip"},
		{"gzip among weighted codings", "POST", "/v1/messages", []string{"br;q=1.0, gzip;q=0.8, *;q=0.1"}, "gzip"},
		{"wildcard", "POST", "/v1/messages", []string{"*"}, "gzip"},
		{"x-gzip", "POST", "/v1/messages", []string{"X-GZIP"}, "gzip"},
		{"split across header lines", "POST", "/v1/messages", []string{"br", "gzip"}, "gzip"},
		{"gzip refused", "POST", "/v1/messages", []string{"br, gzip;q=0"}, "identity"},
		{"only what janus cannot read", "POST", "/v1/messages", []string{"br, zstd"}, "identity"},
		{"identity", "POST", "/v1/messages", []string{"identity"}, "identity"},
		{"nothing offered", "POST", "/v1/messages", nil, ""},
		{"other paths are left alone", "POST", "/v1/messages/count_tokens", []string{"gzip, deflate, br, zstd"}, "gzip, deflate, br, zstd"},
		{"other methods are left alone", "GET", "/v1/messages", []string{"br, zstd"}, "br, zstd"},
	}
	for _, c := range cases {
		var got []string
		front := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Values("Accept-Encoding")
		}), "")
		req, _ := http.NewRequest(c.method, front+c.path, strings.NewReader("{}"))
		for _, v := range c.offered {
			req.Header.Add("Accept-Encoding", v)
		}
		resp, err := plainClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		if joined := strings.Join(got, ", "); joined != c.want {
			t.Errorf("%s: upstream saw Accept-Encoding %q, want %q", c.name, joined, c.want)
		}
	}
}

// A plan's Release is how a planner limits the requests in flight to a
// backend (#128): it must run exactly once, and as soon as the backend is
// done with the request, not when the fallback is. It runs before the
// exchange is reported, so the count is read inside Done: read after Done,
// it raced with a Release that ran later (#211).
func TestPlanReleaseRunsOnceAndEarly(t *testing.T) {
	answer := `{"type":"message","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	for _, c := range []struct {
		name   string
		trial  bool
		accept bool
	}{
		{"relayed", false, false},
		{"trial accepted", true, true},
		{"trial rejected, then the fallback", true, false},
	} {
		var released atomic.Int32
		var releasedWhenFallbackAsked int32 = -1
		first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, answer)
		}))
		fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			releasedWhenFallbackAsked = released.Load()
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, answer)
		}))
		firstURL, _ := url.Parse(first.URL)
		fallbackURL, _ := url.Parse(fallback.URL)

		plan := func(ctx context.Context, h http.Header, req *anthropic.Request) (Plan, *Refusal) {
			p := Plan{Backend: backend.NewUpstream(firstURL, nil), Route: "local", Reason: "test",
				Release: func() { released.Add(1) }}
			if c.trial {
				p.Else = &Plan{Backend: backend.NewUpstream(fallbackURL, nil), Route: "upstream", Reason: "fallback"}
				p.Accept = func(Fetched) (bool, string, string) { return c.accept, "empty", "not good enough" }
			}
			return p, nil
		}
		reported := make(chan int32, 1)
		front := httptest.NewServer(New(Options{Upstream: backend.NewUpstream(firstURL, nil), Plan: plan, Done: func(Exchange) { reported <- released.Load() }}))

		resp, err := plainClient.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if n := <-reported; n != 1 {
			t.Errorf("%s: Release ran %d times before the exchange was reported, want once", c.name, n)
		}
		if c.trial && !c.accept && releasedWhenFallbackAsked != 1 {
			t.Errorf("%s: the fallback was asked before Release ran (released %d)", c.name, releasedWhenFallbackAsked)
		}
		front.Close()
		first.Close()
		fallback.Close()
	}
}

// A client that sends Expect: 100-continue, as curl does with a large body,
// gets an interim 100 Continue from the backend before the real answer.
// The exchange records the real answer's status (#178).
func TestInterimContinueIsNotTheRecordedStatus(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // reading the body sends the 100 Continue
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message"}`)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	reported := make(chan Exchange, 1)
	front := httptest.NewServer(New(Options{Upstream: backend.NewUpstream(u, nil), Done: func(ex Exchange) { reported <- ex }}))
	defer front.Close()

	req, _ := http.NewRequest("POST", front.URL+"/v1/messages", strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("Expect", "100-continue")
	client := &http.Client{Transport: &http.Transport{ExpectContinueTimeout: 5 * time.Second}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("client got %d", resp.StatusCode)
	}
	select {
	case ex := <-reported:
		if ex.Status != 200 {
			t.Errorf("recorded status %d, want 200", ex.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no exchange reported")
	}
}
