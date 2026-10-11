package route

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/guard"
	"github.com/risadams/janus/internal/proxy"
)

// What the policy protects in protectedBody, and what it does not.
var (
	protectedContent = []string{"hunter2-do-not-share", "the acquisition target is Acme"}
	ordinaryContent  = []string{"package main", "look at the config"}
)

// protectedBody is a conversation that read an ordinary file, a file under
// secrets/, and a document carrying a marker.
func protectedBody(model string) string {
	use := func(id, path string) map[string]any {
		return map[string]any{"role": "assistant", "content": []map[string]any{
			{"type": "tool_use", "id": id, "name": "Read", "input": map[string]any{"file_path": path}}}}
	}
	result := func(id, content string) map[string]any {
		return map[string]any{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": id, "content": content}}}
	}
	body, _ := json.Marshal(map[string]any{
		"model": model, "stream": true, "max_tokens": 1024,
		"messages": []map[string]any{
			{"role": "user", "content": "look at the config"},
			use("t1", "/app/src/main.go"), result("t1", "package main"),
			use("t2", "/app/secrets/db.yaml"), result("t2", "value: hunter2-do-not-share"),
			use("t3", "/app/docs/plan.md"), result("t3", "COMPANY CONFIDENTIAL\nthe acquisition target is Acme"),
		},
	})
	return string(body)
}

const ordinaryBody = `{"model":"claude-opus-5-5","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`

// site is janus with a local-only policy, between a fake Anthropic that
// remembers everything it is sent and a fake LM Studio.
type site struct {
	t      *testing.T
	janus  *httptest.Server
	mu     sync.Mutex
	sent   [][]byte // every body the upstream received
	local  chan received
	report chan proxy.Exchange
}

type siteOptions struct {
	action   string
	rules    []Rule
	redact   bool
	upstream http.HandlerFunc // how the fake Anthropic answers; a stream if nil
	lmStudio http.HandlerFunc
	noLocal  bool // LM Studio is not there at all
}

func newSite(t *testing.T, o siteOptions) *site {
	t.Helper()
	s := &site{t: t, report: make(chan proxy.Exchange, 4)}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.sent = append(s.sent, body)
		s.mu.Unlock()
		switch {
		case o.upstream != nil:
			o.upstream(w, r)
		case strings.HasSuffix(r.URL.Path, "/count_tokens"):
			io.WriteString(w, `{"input_tokens":42}`)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, strings.ReplaceAll(localStream, "qwen/qwen3.8-27b", "claude-opus-5-5"))
		}
	}))
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)

	var local *backend.Local
	if o.noLocal {
		gone, _ := url.Parse("http://127.0.0.1:1")
		local = backend.NewLocal(gone, nil)
	} else {
		local, s.local = lmStudio(t, o.lmStudio)
	}
	r := &Router{Upstream: backend.NewUpstream(u, nil), Local: local, Aliases: qwen, Rules: o.rules}
	policy, err := guard.NewPolicy([]string{"secrets/**"}, []string{"COMPANY CONFIDENTIAL"})
	if err != nil {
		t.Fatal(err)
	}
	opts := proxy.Options{
		Upstream: r.Upstream, Plan: r.Plan, KeepBodies: true,
		Policy: policy, PolicyAction: o.action, LocalOnly: r.LocalOnly(""),
		Done: func(ex proxy.Exchange) { r.Report(ex); s.report <- ex },
	}
	if o.redact {
		opts.Redactor = guard.NewRedactor(&guard.Detector{})
	}
	s.janus = httptest.NewServer(proxy.New(opts))
	t.Cleanup(s.janus.Close)
	return s
}

// post sends one request through janus as a subagent would, with any extra
// headers given as pairs.
func (s *site) post(path, body string, header ...string) (status int, answer string, ex proxy.Exchange) {
	s.t.Helper()
	req, _ := http.NewRequest("POST", s.janus.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("X-Claude-Code-Session-Id", "s1")
	req.Header.Set("X-Claude-Code-Agent-Id", "a1")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	select {
	case ex = <-s.report:
	case <-time.After(3 * time.Second):
		s.t.Fatal("no exchange reported")
	}
	return resp.StatusCode, string(b), ex
}

// upstream returns every body the fake Anthropic received, having checked
// that none of them holds protected content.
func (s *site) upstream() [][]byte {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, body := range s.sent {
		for _, secret := range protectedContent {
			if bytes.Contains(body, []byte(secret)) {
				s.t.Errorf("upstream request %d of %d contains local-only content %q", i+1, len(s.sent), secret)
			}
		}
	}
	return s.sent
}

// withheldWhole checks that one upstream body is the conversation with the
// protected content replaced and everything else in place.
func withheldWhole(t *testing.T, body []byte) {
	t.Helper()
	for _, kept := range ordinaryContent {
		if !bytes.Contains(body, []byte(kept)) {
			t.Errorf("ordinary content %q is missing from what went upstream", kept)
		}
	}
	if n := bytes.Count(body, []byte("Withheld by janus")); n != 2 {
		t.Errorf("%d notices in what went upstream, want 2", n)
	}
	var parsed struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Messages) != 7 {
		t.Errorf("what went upstream is not the same conversation: %d messages, %v", len(parsed.Messages), err)
	}
}

var subagentsLocal = []Rule{{Name: "subagents", Class: Subagent, Route: RouteLocal, Local: qwen["janus-local"]}}

func TestWithholdOnEveryWayUpstream(t *testing.T) {
	broken := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model crashed", http.StatusInternalServerError)
	}
	empty := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.ReplaceAll(localStream, "Two lines.", ""))
	}
	cases := []struct {
		name   string
		opts   siteOptions
		path   string
		header []string
	}{
		{name: "no rule matches", path: "/v1/messages"},
		{name: "token count", path: "/v1/messages/count_tokens"},
		{name: "override header", path: "/v1/messages", header: []string{backend.RouteHeader, "upstream"}},
		{name: "local rule, LM Studio absent", path: "/v1/messages", opts: siteOptions{rules: subagentsLocal, noLocal: true}},
		{name: "local rule, LM Studio failing", path: "/v1/messages", opts: siteOptions{rules: subagentsLocal, lmStudio: broken}},
		{name: "escalation after an empty local answer", path: "/v1/messages", opts: siteOptions{lmStudio: empty,
			rules: []Rule{{Name: "try", Class: Subagent, Route: RouteEscalate, Local: qwen["janus-local"], TrialTimeout: time.Minute}}}},
		{name: "with the redactor on", path: "/v1/messages", opts: siteOptions{redact: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSite(t, c.opts)
			status, answer, ex := s.post(c.path, protectedBody("claude-opus-5-5"), c.header...)
			if status != 200 {
				t.Fatalf("status %d: %s", status, answer)
			}
			sent := s.upstream()
			if len(sent) != 1 {
				t.Fatalf("upstream received %d requests, want 1", len(sent))
			}
			withheldWhole(t, sent[0])
			if ex.Backend != "upstream" || ex.Guard == nil || ex.Guard.Withheld != 2 || len(ex.Guard.LocalOnly) != 2 {
				t.Errorf("backend %q, guard %+v", ex.Backend, ex.Guard)
			}
			if c.path == "/v1/messages" && !bytes.Equal(ex.SentBody, sent[0]) {
				t.Error("the exchange does not record what was actually sent")
			}
		})
	}
}

func TestWithholdLeavesLocalRequestsWhole(t *testing.T) {
	s := newSite(t, siteOptions{rules: subagentsLocal})
	status, answer, ex := s.post("/v1/messages", protectedBody("claude-opus-5-5"))
	if status != 200 || ex.Backend != "local" {
		t.Fatalf("status %d, backend %q: %s", status, ex.Backend, answer)
	}
	if sent := s.upstream(); len(sent) != 0 {
		t.Errorf("upstream received %d requests", len(sent))
	}
	got := <-s.local
	for _, secret := range protectedContent {
		if !bytes.Contains(got.Body, []byte(secret)) {
			t.Errorf("the local model was not given %q", secret)
		}
	}
	if ex.Guard == nil || ex.Guard.Withheld != 0 || len(ex.Guard.LocalOnly) != 2 {
		t.Errorf("guard %+v", ex.Guard)
	}
}

func TestOrdinaryRequestsAreUntouchedByThePolicy(t *testing.T) {
	for _, action := range []string{"withhold", "local"} {
		s := newSite(t, siteOptions{action: action})
		status, _, ex := s.post("/v1/messages", ordinaryBody)
		sent := s.upstream()
		if status != 200 || len(sent) != 1 || string(sent[0]) != ordinaryBody {
			t.Errorf("%s: status %d, upstream got %q", action, status, sent)
		}
		if ex.Guard != nil || ex.SentBody != nil {
			t.Errorf("%s: guard %+v, sent body %q", action, ex.Guard, ex.SentBody)
		}
	}
}

func TestRequestsThatCannotBeCheckedAreNotSent(t *testing.T) {
	for _, action := range []string{"withhold", "local"} {
		for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
			s := newSite(t, siteOptions{action: action})
			status, answer, ex := s.post(path, `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"/app/secrets/db.yaml hunter2-do-not-share"`)
			if status != 500 || !strings.Contains(answer, "was not sent") || ex.Backend != "none" {
				t.Errorf("%s %s: status %d, backend %q: %s", action, path, status, ex.Backend, answer)
			}
			if sent := s.upstream(); len(sent) != 0 {
				t.Errorf("%s %s: upstream received %d requests", action, path, len(sent))
			}
		}
	}
}

func TestLocalActionKeepsTheWholeRequestLocal(t *testing.T) {
	cases := []struct {
		name   string
		rules  []Rule
		header []string
	}{
		{name: "no rule matches"},
		{name: "override header says upstream", header: []string{backend.RouteHeader, "upstream"}},
		{name: "a rule says upstream", rules: []Rule{{Name: "all", Route: RouteUpstream}}},
		{name: "a rule says escalate", rules: []Rule{{Name: "try", Class: Subagent, Route: RouteEscalate, Local: qwen["janus-local"], TrialTimeout: time.Minute}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSite(t, siteOptions{action: "local", rules: c.rules})
			status, answer, ex := s.post("/v1/messages", protectedBody("claude-opus-5-5"), c.header...)
			if status != 200 || !strings.Contains(answer, "Two lines.") {
				t.Fatalf("status %d: %s", status, answer)
			}
			if sent := s.upstream(); len(sent) != 0 {
				t.Errorf("upstream received %d requests", len(sent))
			}
			if ex.Backend != "local" || !strings.HasPrefix(ex.Reason, "local-only policy: ") || ex.Guard == nil || len(ex.Guard.LocalOnly) != 2 {
				t.Errorf("backend %q, reason %q, guard %+v", ex.Backend, ex.Reason, ex.Guard)
			}
			// The client asked for Claude and is told Claude answered.
			if !strings.Contains(answer, `"model":"claude-opus-5-5"`) {
				t.Errorf("the answer does not carry the model the client asked for: %s", answer)
			}
		})
	}
}

func TestLocalActionRefusesRatherThanGoUpstream(t *testing.T) {
	broken := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model crashed", http.StatusInternalServerError)
	}
	cases := []struct {
		name string
		opts siteOptions
	}{
		{"LM Studio absent", siteOptions{noLocal: true}},
		{"LM Studio failing", siteOptions{lmStudio: broken}},
		{"LM Studio failing, with a rule that falls back", siteOptions{lmStudio: broken, rules: subagentsLocal}},
		{"LM Studio absent, with a rule that falls back", siteOptions{noLocal: true, rules: subagentsLocal}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.opts.action = "local"
			s := newSite(t, c.opts)
			status, answer, ex := s.post("/v1/messages", protectedBody("claude-opus-5-5"))
			if status < 400 {
				t.Fatalf("status %d: %s", status, answer)
			}
			if sent := s.upstream(); len(sent) != 0 {
				t.Errorf("upstream received %d requests", len(sent))
			}
			if ex.Backend == "upstream" {
				t.Errorf("backend %q", ex.Backend)
			}
			for _, secret := range protectedContent {
				if strings.Contains(answer, secret) || strings.Contains(ex.Reason, secret) {
					t.Errorf("the refusal repeats local-only content %q", secret)
				}
			}
		})
	}
}

func TestLocalActionStillWithholdsFromTokenCounts(t *testing.T) {
	s := newSite(t, siteOptions{action: "local"})
	status, answer, ex := s.post("/v1/messages/count_tokens", protectedBody("claude-opus-5-5"))
	sent := s.upstream()
	if status != 200 || len(sent) != 1 {
		t.Fatalf("status %d, %d upstream requests: %s", status, len(sent), answer)
	}
	withheldWhole(t, sent[0])
	if ex.Guard == nil || ex.Guard.Withheld != 2 {
		t.Errorf("guard %+v", ex.Guard)
	}
}

func TestWithholdingIsTheSameEveryTurn(t *testing.T) {
	// The withheld conversation is a prefix Anthropic caches; it must not
	// change from one request to the next.
	s := newSite(t, siteOptions{})
	s.post("/v1/messages", protectedBody("claude-opus-5-5"))
	s.post("/v1/messages", protectedBody("claude-opus-5-5"))
	if sent := s.upstream(); len(sent) != 2 || !bytes.Equal(sent[0], sent[1]) {
		t.Errorf("two identical requests were sent upstream differently")
	}
}

// A search that quotes a protected file has those lines withheld under
// either action, and does not send the request to the local model.
func TestSearchOutputIsWithheldUnderEitherAction(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"model": "claude-opus-5-5", "stream": true, "max_tokens": 1024,
		"messages": []map[string]any{
			{"role": "user", "content": "where is the password set?"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": "t1", "name": "Grep", "input": map[string]any{"pattern": "password", "path": "/app"}}}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "t1", "content": "src/db.go:8:// reads the password\nsecrets/db.yaml:2:value: hunter2-do-not-share"}}},
		},
	})
	for _, action := range []string{"withhold", "local"} {
		s := newSite(t, siteOptions{action: action})
		status, answer, ex := s.post("/v1/messages", string(body))
		sent := s.upstream()
		if status != 200 || len(sent) != 1 || ex.Backend != "upstream" {
			t.Fatalf("%s: status %d, %d upstream requests, backend %q: %s", action, status, len(sent), ex.Backend, answer)
		}
		if !bytes.Contains(sent[0], []byte("src/db.go:8:// reads the password")) || !bytes.Contains(sent[0], []byte("janus withheld this line")) {
			t.Errorf("%s: upstream got %s", action, sent[0])
		}
		if ex.Guard == nil || ex.Guard.Withheld != 1 || len(ex.Guard.LocalOnly) != 1 {
			t.Errorf("%s: guard %+v", action, ex.Guard)
		}
	}
}
