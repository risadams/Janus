package proxy

// The fidelity suite: janus with every feature off must be
// indistinguishable from a direct connection, to both ends.
//
// Each case is sent twice to the same fake upstream, once directly and once
// through janus. The upstream must see the same request both times, and the
// client must get the same response both times. The comparison is of
// everything, so a difference has to be on the short list of allowed ones
// below or the suite fails.

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"fmt"
	"github.com/risadams/janus/internal/backend"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// seen is one request as the upstream received it.
type seen struct {
	Method string
	URI    string
	Host   string
	Header http.Header
	Body   []byte
}

// got is one response as the client received it.
type got struct {
	Status           int
	Header           http.Header
	Body             []byte
	ContentLength    int64
	TransferEncoding []string
}

type fidelityCase struct {
	name   string
	method string
	path   string // path and query, sent as written
	header http.Header
	body   []byte
	// respond is the upstream's behaviour for this case.
	respond http.HandlerFunc
	// messages marks a Messages API request, the only kind on which janus
	// is allowed to change Accept-Encoding.
	messages bool
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func h(pairs ...string) http.Header {
	header := http.Header{}
	for i := 0; i < len(pairs); i += 2 {
		header.Add(pairs[i], pairs[i+1])
	}
	return header
}

// streamEvents writes an event stream one event at a time, flushing after
// each, compressed if the request allows gzip. That is how the real API
// behaves, and it makes the bytes on the wire depend on the request's
// Accept-Encoding only through "is gzip allowed".
func streamEvents(stream []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Request-Id", "req_fidelity")
		var out io.Writer = w
		var zw *gzip.Writer
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			zw = gzip.NewWriter(w)
			out = zw
		}
		for _, event := range bytes.SplitAfter(stream, []byte("\n\n")) {
			out.Write(event)
			if zw != nil {
				zw.Flush()
			}
			w.(http.Flusher).Flush()
		}
		if zw != nil {
			zw.Close()
		}
	}
}

func reply(status int, header http.Header, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for k, vs := range header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(status)
		w.Write(body)
	}
}

func fidelityCases(t *testing.T) []fidelityCase {
	request := mustRead(t, "../anthropic/testdata/request.json")
	stream := mustRead(t, "../anthropic/testdata/stream.sse")
	random := make([]byte, 2<<20)
	rand.Read(random)

	claudeCode := h(
		"Content-Type", "application/json",
		"Authorization", "Bearer secret-token",
		"Anthropic-Version", "2023-06-01",
		"Anthropic-Beta", "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14",
		"Accept", "application/json",
		"Accept-Encoding", "gzip, deflate, br, zstd",
		"User-Agent", "claude-cli/2.1.296 (external, sdk-cli)",
		"X-App", "cli",
		"X-Claude-Code-Session-Id", "session-1",
		"X-Stainless-Retry-Count", "0",
	)
	json := h("Content-Type", "application/json")
	message := []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"Ready."}],` +
		`"model":"claude-opus-5-5","stop_reason":"end_turn","usage":{"input_tokens":17,"output_tokens":3}}`)

	return []fidelityCase{
		{name: "streamed tool-use turn, gzip, Claude Code's headers", method: "POST", path: "/v1/messages?beta=true",
			header: claudeCode, body: request, respond: streamEvents(stream), messages: true},
		{name: "streamed turn, client offers no compression", method: "POST", path: "/v1/messages",
			header: h("Content-Type", "application/json", "X-Api-Key", "sk-secret"), body: request, respond: streamEvents(stream), messages: true},
		{name: "non-streaming message", method: "POST", path: "/v1/messages",
			header: h("Content-Type", "application/json", "X-Api-Key", "sk-secret"), body: []byte(`{"model":"claude-opus-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`),
			respond: reply(200, json, message), messages: true},
		{name: "subagent turn", method: "POST", path: "/v1/messages",
			header: h("Content-Type", "application/json", "Authorization", "Bearer t", "X-Claude-Code-Session-Id", "s", "X-Claude-Code-Agent-Id", "a"),
			body:   request, respond: streamEvents(stream), messages: true},
		{name: "count_tokens", method: "POST", path: "/v1/messages/count_tokens",
			header: claudeCode, body: request, respond: reply(200, json, []byte(`{"input_tokens":4213}`))},
		{name: "rate limited", method: "POST", path: "/v1/messages", header: claudeCode, body: request, messages: true,
			respond: reply(429, h("Content-Type", "application/json", "Retry-After", "17", "Anthropic-Ratelimit-Requests-Remaining", "0"),
				[]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))},
		{name: "overloaded", method: "POST", path: "/v1/messages", header: claudeCode, body: request, messages: true,
			respond: reply(529, json, []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))},
		{name: "server error that is not JSON", method: "POST", path: "/v1/messages", header: claudeCode, body: request, messages: true,
			respond: reply(502, h("Content-Type", "text/html"), []byte("<html>Bad Gateway</html>"))},
		{name: "invalid request", method: "POST", path: "/v1/messages", header: json, body: []byte(`{"model":`), messages: true,
			respond: reply(400, json, []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad json"}}`))},
		{name: "model list with a query", method: "GET", path: "/v1/models?limit=5&after_id=abc",
			header: h("X-Api-Key", "sk-secret", "Accept-Encoding", "br, zstd"), respond: reply(200, json, []byte(`{"data":[]}`))},
		{name: "connectivity check", method: "HEAD", path: "/api/hello", header: h("User-Agent", "claude-cli/2.1.296"), respond: reply(200, nil, nil)},
		{name: "root", method: "GET", path: "/", respond: reply(200, h("Content-Type", "text/plain"), []byte("Anthropic API\n"))},
		{name: "a path janus has never heard of", method: "DELETE", path: "/v1/files/file_123", header: h("X-Api-Key", "sk-secret"),
			respond: reply(200, json, []byte(`{"id":"file_123","type":"file_deleted"}`))},
		{name: "escaped path and repeated query keys", method: "GET", path: "/v1/thing%20with%2Fescapes?a=1&a=2&b=%26&empty=",
			respond: reply(404, json, []byte(`{"type":"error","error":{"type":"not_found_error","message":"no"}}`))},
		{name: "no content", method: "POST", path: "/v1/something", header: json, body: []byte("{}"), respond: reply(204, nil, nil)},
		{name: "large binary bodies both ways", method: "PUT", path: "/v1/files/upload",
			header: h("Content-Type", "application/octet-stream"), body: random, respond: reply(200, h("Content-Type", "application/octet-stream"), random[1<<20:])},
		{name: "repeated and unusual headers", method: "POST", path: "/v1/messages", messages: true, body: request,
			header: h("Content-Type", "application/json", "Anthropic-Beta", "one", "Anthropic-Beta", "two", "X-Custom", "a, b",
				"x-lowercase-name", "kept", "Cookie", "c=1", "Accept-Encoding", "gzip"),
			respond: reply(200, h("Content-Type", "application/json", "Set-Cookie", "a=1", "Set-Cookie", "b=2", "X-Odd", ""), message)},
	}
}

// perform sends one case to target and returns the response as received.
func perform(t *testing.T, target string, c fidelityCase) got {
	t.Helper()
	g, err := tryPerform(target, c)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func tryPerform(target string, c fidelityCase) (got, error) {
	req, err := http.NewRequest(c.method, target+c.path, bytes.NewReader(c.body))
	if err != nil {
		return got{}, err
	}
	for k, vs := range c.header {
		req.Header[k] = append([]string(nil), vs...)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "fidelity-test")
	}
	resp, err := plainClient.Do(req)
	if err != nil {
		return got{}, fmt.Errorf("sending: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return got{}, fmt.Errorf("reading the response after %d bytes: %w", len(body), err)
	}
	return got{resp.StatusCode, resp.Header, body, resp.ContentLength, resp.TransferEncoding}, nil
}

func headerDiff(a, b http.Header, ignore ...string) string {
	skip := map[string]bool{}
	for _, k := range ignore {
		skip[k] = true
	}
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var diffs []string
	for k := range keys {
		if !skip[k] && !reflect.DeepEqual(a[k], b[k]) {
			diffs = append(diffs, fmt.Sprintf("%s: direct %q, through janus %q", k, a[k], b[k]))
		}
	}
	sort.Strings(diffs)
	return strings.Join(diffs, "; ")
}

func TestFidelity(t *testing.T) {
	for _, keepBodies := range []bool{false, true} {
		for _, c := range fidelityCases(t) {
			name := c.name
			if keepBodies {
				name += ", capture on"
			}
			t.Run(name, func(t *testing.T) {
				requests := make(chan seen, 1)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					requests <- seen{r.Method, r.RequestURI, r.Host, r.Header.Clone(), body}
					c.respond(w, r)
				}))
				defer upstream.Close()
				u, _ := url.Parse(upstream.URL)
				janus := httptest.NewServer(New(Options{Upstream: backend.NewUpstream(u, nil), Done: quiet, KeepBodies: keepBodies}))
				defer janus.Close()

				directResp := perform(t, upstream.URL, c)
				direct := <-requests
				viaResp := perform(t, janus.URL, c)
				via := <-requests

				// What the upstream saw.
				if via.Method != direct.Method || via.URI != direct.URI || via.Host != direct.Host {
					t.Errorf("request line: direct %s %s (Host %s), through janus %s %s (Host %s)",
						direct.Method, direct.URI, direct.Host, via.Method, via.URI, via.Host)
				}
				if !bytes.Equal(via.Body, direct.Body) {
					t.Errorf("request body differs: %d bytes direct, %d through janus", len(direct.Body), len(via.Body))
				}
				allowed := []string{}
				if c.messages {
					// The one sanctioned change (FR-1.2): checked exactly
					// below rather than merely tolerated.
					allowed = append(allowed, "Accept-Encoding")
				}
				if d := headerDiff(direct.Header, via.Header, allowed...); d != "" {
					t.Errorf("request headers differ: %s", d)
				}
				if c.messages {
					want := []string(nil)
					if offered := direct.Header.Values("Accept-Encoding"); len(offered) > 0 {
						want = []string{"identity"}
						if strings.Contains(strings.Join(offered, ","), "gzip") {
							want = []string{"gzip"}
						}
					}
					if have := via.Header.Values("Accept-Encoding"); !reflect.DeepEqual(have, want) {
						t.Errorf("Accept-Encoding through janus = %q, want %q", have, want)
					}
				}

				// What the client got. Date is the upstream's clock at two
				// different moments, so it alone may differ.
				if viaResp.Status != directResp.Status {
					t.Errorf("status: direct %d, through janus %d", directResp.Status, viaResp.Status)
				}
				if !bytes.Equal(viaResp.Body, directResp.Body) {
					t.Errorf("response body differs: %d bytes direct, %d through janus", len(directResp.Body), len(viaResp.Body))
				}
				if d := headerDiff(directResp.Header, viaResp.Header, "Date"); d != "" {
					t.Errorf("response headers differ: %s", d)
				}
				if viaResp.ContentLength != directResp.ContentLength || !reflect.DeepEqual(viaResp.TransferEncoding, directResp.TransferEncoding) {
					t.Errorf("framing: direct length %d %v, through janus length %d %v",
						directResp.ContentLength, directResp.TransferEncoding, viaResp.ContentLength, viaResp.TransferEncoding)
				}
			})
		}
	}
}

// median runs fn n times and returns the middle of the durations it
// reports.
func median(n int, fn func() time.Duration) time.Duration {
	times := make([]time.Duration, n)
	for i := range times {
		times[i] = fn()
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	return times[n/2]
}

// NFR-1: passing through janus adds under 10 ms at the median, both to a
// whole request and to the first byte of a stream. The budget is generous
// for a loopback hop; the test exists to catch a change that starts
// buffering or blocking, not to measure microseconds.
func TestOverheadIsWithinBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	const budget = 10 * time.Millisecond
	request := mustRead(t, "../anthropic/testdata/request.json")
	stream := mustRead(t, "../anthropic/testdata/stream.sse")

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if r.URL.Query().Get("hold") == "" {
			w.Write(stream)
			return
		}
		// Send the first event, then hold the stream open until told.
		first, rest, _ := bytes.Cut(stream, []byte("\n\n"))
		w.Write(append(first, "\n\n"...))
		w.(http.Flusher).Flush()
		<-release
		w.Write(rest)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	// Capture on: the most work janus does per request.
	janus := httptest.NewServer(New(Options{Upstream: backend.NewUpstream(u, nil), Done: quiet, KeepBodies: true}))
	defer janus.Close()

	// whole times a request from sending it to reading the last byte.
	whole := func(target string) func() time.Duration {
		return func() time.Duration {
			start := time.Now()
			resp, err := plainClient.Post(target+"/v1/messages", "application/json", bytes.NewReader(request))
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return time.Since(start)
		}
	}
	// firstByte times a request from sending it to reading the first byte
	// of a stream the upstream is still holding open.
	firstByte := func(target string) func() time.Duration {
		return func() time.Duration {
			start := time.Now()
			resp, err := plainClient.Post(target+"/v1/messages?hold=1", "application/json", bytes.NewReader(request))
			if err != nil {
				t.Fatal(err)
			}
			one := make([]byte, 1)
			if _, err := io.ReadFull(resp.Body, one); err != nil {
				t.Fatal(err)
			}
			elapsed := time.Since(start)
			release <- struct{}{}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return elapsed
		}
	}

	for _, target := range []string{upstream.URL, janus.URL} {
		median(20, whole(target)) // warm up connections
	}
	directWhole, viaWhole := median(300, whole(upstream.URL)), median(300, whole(janus.URL))
	directFirst, viaFirst := median(100, firstByte(upstream.URL)), median(100, firstByte(janus.URL))

	t.Logf("whole request: direct %s, through janus %s, added %s", directWhole, viaWhole, viaWhole-directWhole)
	t.Logf("first byte:    direct %s, through janus %s, added %s", directFirst, viaFirst, viaFirst-directFirst)
	if added := viaWhole - directWhole; added > budget {
		t.Errorf("janus adds %s to a whole request at the median; the budget is %s", added, budget)
	}
	if added := viaFirst - directFirst; added > budget {
		t.Errorf("janus adds %s to the first byte of a stream at the median; the budget is %s", added, budget)
	}
}

func benchmarkMessages(b *testing.B, throughJanus bool) {
	request, _ := os.ReadFile("../anthropic/testdata/request.json")
	stream, _ := os.ReadFile("../anthropic/testdata/stream.sse")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(stream)
	}))
	defer upstream.Close()
	target := upstream.URL
	if throughJanus {
		u, _ := url.Parse(upstream.URL)
		janus := httptest.NewServer(New(Options{Upstream: backend.NewUpstream(u, nil), Done: quiet, KeepBodies: true}))
		defer janus.Close()
		target = janus.URL
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := plainClient.Post(target+"/v1/messages", "application/json", bytes.NewReader(request))
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func BenchmarkMessagesDirect(b *testing.B)       { benchmarkMessages(b, false) }
func BenchmarkMessagesThroughJanus(b *testing.B) { benchmarkMessages(b, true) }

// Megabyte bodies in both directions, many times over.
//
// This guards a real failure. A proxy starts relaying the response while
// its transport may still be finishing the request body, and an HTTP/1
// server closes that body as soon as the response starts unless full duplex
// is enabled. The transport then drops the backend connection and the
// client gets a truncated response. It happened about once in several
// hundred large requests on Linux and never on macOS, which is why this
// repeats: one pass of the fidelity suite is not enough to see it.
func TestLargeBodiesRepeatedly(t *testing.T) {
	if testing.Short() {
		t.Skip("repetition test")
	}
	var large fidelityCase
	for _, c := range fidelityCases(t) {
		if c.name == "large binary bodies both ways" {
			large = c
		}
	}
	if large.respond == nil {
		t.Fatal("the large-body case is missing from fidelityCases")
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("upstream could not read the request after %d bytes: %v", n, err)
		}
		large.respond(w, r)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	var mu sync.Mutex
	var faults []string
	janus := httptest.NewServer(New(Options{Upstream: backend.NewUpstream(u, nil), KeepBodies: true, Done: func(ex Exchange) {
		if ex.Fault != "" || ex.Status != 200 || ex.Cancelled {
			mu.Lock()
			faults = append(faults, fmt.Sprintf("status %d, cancelled %v, fault %q, %d response bytes kept", ex.Status, ex.Cancelled, ex.Fault, len(ex.ResponseBody)))
			mu.Unlock()
		}
	}}))

	want := 1 << 20
	for i := 0; i < 400; i++ {
		for leg, target := range map[string]string{"direct": upstream.URL, "through janus": janus.URL} {
			g, err := tryPerform(target, large)
			if err != nil || len(g.Body) != want {
				t.Errorf("run %d, %s: %d of %d bytes, err: %v", i, leg, len(g.Body), want, err)
			}
		}
	}
	// Closing waits for every handler to return, so every exchange has
	// been reported by the time the faults are read.
	janus.Close()
	mu.Lock()
	defer mu.Unlock()
	for _, f := range faults {
		t.Errorf("janus reported: %s", f)
	}
}
