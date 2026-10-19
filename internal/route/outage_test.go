package route

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/proxy"
)

// A stand-in answers when Claude is down for a while, not on a failure
// Claude Code would retry past (#150). A session limit says so in its
// headers and is stood in for at once; anything else counts toward an
// outage, which takes three failures within two minutes with no answer
// from Claude between them.
func TestAStandInWaitsForClaudeToBeDown(t *testing.T) {
	now := time.Date(2026, 10, 14, 21, 47, 59, 0, time.UTC)
	o := &outage{now: func() time.Time { return now }}
	limit := http.Header{
		"Retry-After":                           {"9720"},
		"Anthropic-Ratelimit-Unified-Status":    {"rejected"},
		"Anthropic-Ratelimit-Unified-Reset":     {strconv.FormatInt(now.Add(162*time.Minute).Unix(), 10)},
		"X-Should-Retry":                        {"true"}, // as real session limits say
		"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"},
	}
	if !o.standIn(429, limit) {
		t.Error("a session limit hours from its reset was not stood in for")
	}

	brief := http.Header{"Retry-After": {"5"}}
	for i, want := range []bool{false, false, true} {
		if got := o.standIn(529, brief); got != want {
			t.Errorf("overload %d: stand in %v, want %v", i+1, got, want)
		}
	}

	// Claude answering ends it: the next failure is a first again.
	o.recovered()
	if o.standIn(500, nil) {
		t.Error("a single server error after Claude answered was stood in for")
	}
	// Failures older than the window don't count.
	o.standIn(0, nil)
	now = now.Add(3 * time.Minute)
	if o.standIn(0, nil) {
		t.Error("two failures three minutes apart made an outage")
	}

	// A request Claude refused is not Claude being down, and counts for
	// nothing.
	o.recovered()
	for range 5 {
		if o.standIn(400, nil) {
			t.Fatal("a 400 was stood in for")
		}
	}
	if o.standIn(529, nil) {
		t.Error("400s were counted toward an outage")
	}
}

func TestRetryWait(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		h    http.Header
		want time.Duration
	}{
		"nothing":            {nil, 0},
		"seconds":            {http.Header{"Retry-After": {"90"}}, 90 * time.Second},
		"an HTTP date":       {http.Header{"Retry-After": {now.Add(10 * time.Minute).Format(http.TimeFormat)}}, 10 * time.Minute},
		"a past date":        {http.Header{"Retry-After": {now.Add(-time.Minute).Format(http.TimeFormat)}}, 0},
		"garbage":            {http.Header{"Retry-After": {"soon"}}, 0},
		"unified reset":      {http.Header{"Anthropic-Ratelimit-Unified-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-Reset": {strconv.FormatInt(now.Add(time.Hour).Unix(), 10)}}, time.Hour},
		"reset, but allowed": {http.Header{"Anthropic-Ratelimit-Unified-Status": {"allowed"}, "Anthropic-Ratelimit-Unified-Reset": {strconv.FormatInt(now.Add(time.Hour).Unix(), 10)}}, 0},
		"the longer of the two": {http.Header{"Retry-After": {"20"}, "Anthropic-Ratelimit-Unified-Status": {"rejected"},
			"Anthropic-Ratelimit-Unified-Reset": {strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10)}}, 2 * time.Hour},
	}
	for name, c := range cases {
		if got := retryWait(c.h, now); got != c.want {
			t.Errorf("%s: retryWait = %s, want %s", name, got, c.want)
		}
	}
}

// Through the proxy, with a router that has seen no failures: a brief
// overload reaches Claude Code as Claude sent it, for it to retry; the
// third in a row brings the stand-in; and a session limit brings it at
// once.
func TestABriefFailureReachesClaudeCodeToRetry(t *testing.T) {
	lms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v0/models" {
			io.WriteString(w, modelList)
			return
		}
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, localStream)
	}))
	defer lms.Close()
	var answer func(w http.ResponseWriter)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		answer(w)
	}))
	defer up.Close()
	upURL, _ := url.Parse(up.URL)
	lmsURL, _ := url.Parse(lms.URL)
	model := LocalModel{ID: "qwen/qwen3.8-27b", Timeout: 5 * time.Second}
	r := &Router{Upstream: backend.NewUpstream(upURL, nil), Local: backend.NewLocal(lmsURL, nil),
		Rules: []Rule{{Name: "main", Class: Main, Route: RouteUpstream, IfDown: &model}}}
	janus := httptest.NewServer(proxy.New(proxy.Options{Upstream: r.Upstream, Plan: r.Plan, Done: r.Report}))
	defer janus.Close()

	send := func() (int, string) {
		req, _ := http.NewRequest("POST", janus.URL+"/v1/messages", strings.NewReader(opusRequest))
		req.Header.Set("X-Claude-Code-Session-Id", "s1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("X-Janus-Fallback")
	}
	overloaded := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(529)
		io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	}

	answer = overloaded
	for i := 1; i <= 2; i++ {
		if status, fellBack := send(); status != 529 || fellBack != "" {
			t.Errorf("overload %d: client got %d (fallback %q), want Claude's 529 to retry", i, status, fellBack)
		}
	}
	if status, fellBack := send(); status != 200 || fellBack == "" {
		t.Errorf("third overload: client got %d (fallback %q), want the stand-in", status, fellBack)
	}

	// Claude answers: the outage is over, and the next overload is a
	// first again.
	answer = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","role":"assistant","content":[],"model":"claude-opus-5-5","stop_reason":"end_turn"}`)
	}
	send()
	// The router hears of the answer from the proxy's Done, which runs
	// after the client has the response. Only a reused connection makes
	// the next request wait for it; on a new one, the next overload can
	// arrive first and count as a fourth. Wait for it.
	waitFor(t, "the router to hear Claude answered", func() bool {
		r.down.mu.Lock()
		defer r.down.mu.Unlock()
		return len(r.down.failures) == 0
	})
	answer = overloaded
	if status, _ := send(); status != 529 {
		t.Errorf("overload after Claude answered: client got %d, want 529", status)
	}

	// A session limit is stood in for at once.
	answer = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "9720")
		w.WriteHeader(429)
		io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."}}`)
	}
	r.down.recovered()
	if status, fellBack := send(); status != 200 || !strings.Contains(fellBack, "HTTP 429") {
		t.Errorf("session limit: client got %d (fallback %q), want the stand-in", status, fellBack)
	}
}

// waitFor polls until done reports true, failing the test if that takes
// more than a few seconds. It is for state set after a response has ended.
func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !done(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
