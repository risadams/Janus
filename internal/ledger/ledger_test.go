package ledger

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/proxy"
	"github.com/risadams/janus/internal/transform"
)

func n(i int) *int { return &i }

// exchange is a successful subagent turn, as the proxy would report it.
func exchange() proxy.Exchange {
	header := http.Header{}
	header.Set("X-Claude-Code-Session-Id", "sess-1")
	header.Set("X-Claude-Code-Agent-Id", "agent-1")
	header.Set("Authorization", "Bearer secret-token")
	return proxy.Exchange{
		ID:       "20261011T143927.123-ab12cd",
		Start:    time.Date(2026, 10, 11, 10, 39, 27, 123e6, time.FixedZone("EDT", -4*3600)),
		Method:   "POST",
		Path:     "/v1/messages",
		Header:   header,
		Status:   200,
		Backend:  "upstream",
		Route:    "upstream",
		Reason:   "passthrough",
		TTFB:     812 * time.Millisecond,
		Duration: 1931 * time.Millisecond,
		RequestBody: []byte(`{"model":"claude-opus-5-5","stream":true,"max_tokens":100,` +
			`"system":[{"type":"text","text":"secret system prompt","cache_control":{"type":"ephemeral"}}],` +
			`"tools":[{"name":"Read"},{"name":"Bash"}],` +
			`"messages":[{"role":"user","content":"private user text"}]}`),
		Summary: &anthropic.Summary{
			ID: "msg_1", Model: "claude-opus-5-5", StopReason: "tool_use", Complete: true,
			Blocks: []string{"thinking", "tool_use"},
			Usage:  anthropic.Usage{Input: 2, Output: 118, CacheRead: n(18327), CacheWrite: n(0), CacheWrite1h: n(0), Thinking: n(40)},
		},
	}
}

func TestFromExchange(t *testing.T) {
	rec := FromExchange(exchange())

	if rec.V != SchemaVersion || rec.ID != "20261011T143927.123-ab12cd" {
		t.Errorf("v=%d id=%q", rec.V, rec.ID)
	}
	if got := rec.Time.Format(time.RFC3339Nano); got != "2026-10-11T14:39:27.123Z" {
		t.Errorf("time = %s, want it in UTC", got)
	}
	if rec.Session != "sess-1" || rec.Agent != "agent-1" || rec.Class != "subagent" {
		t.Errorf("session=%q agent=%q class=%q", rec.Session, rec.Agent, rec.Class)
	}
	if rec.Route != "upstream" || rec.Reason != "passthrough" || rec.Backend != "upstream" || rec.Stages == nil || len(rec.Stages) != 0 {
		t.Errorf("decision = route %q reason %q backend %q stages %v", rec.Route, rec.Reason, rec.Backend, rec.Stages)
	}
	if rec.Status != 200 || rec.Outcome != OutcomeOK || rec.TTFBMs != 812 || rec.DurationMs != 1931 {
		t.Errorf("status=%d outcome=%q ttfb=%d duration=%d", rec.Status, rec.Outcome, rec.TTFBMs, rec.DurationMs)
	}
	want := Request{Model: "claude-opus-5-5", Stream: true, Bytes: len(exchange().RequestBody), Messages: 1, Tools: 2, CacheBreakpoints: 1}
	if rec.Request == nil || *rec.Request != want {
		t.Errorf("request = %+v, want %+v", rec.Request, want)
	}
	if rec.Response == nil || rec.Response.StopReason != "tool_use" || strings.Join(rec.Response.Blocks, ",") != "thinking,tool_use" {
		t.Errorf("response = %+v", rec.Response)
	}
	u := rec.Usage
	if u == nil || u.Input != 2 || u.Output != 118 || *u.CacheRead != 18327 || *u.CacheWrite != 0 || *u.Thinking != 40 || u.CacheWrite5m != nil {
		t.Errorf("usage = %+v", u)
	}
}

// A rate limit or a server error keeps what the API said about when it
// would answer again, for the response and for an upstream attempt given
// up on. Nothing else from the headers is kept.
func TestLimitsAreKeptForRateLimitsAndServerErrors(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "17")
	h.Set("X-Should-Retry", "true")
	h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Reset", "1760385600")
	h.Set("Set-Cookie", "session=secret")
	h.Set("Request-Id", "req_1")
	want := map[string]string{"retry-after": "17", "x-should-retry": "true",
		"anthropic-ratelimit-unified-status": "rejected", "anthropic-ratelimit-unified-reset": "1760385600"}

	limited := exchange()
	limited.Status, limited.ResponseHeader = 429, h
	if got := FromExchange(limited).Limits; !reflect.DeepEqual(got, want) {
		t.Errorf("429 limits = %v, want %v", got, want)
	}
	ok := exchange()
	ok.ResponseHeader = h
	if got := FromExchange(ok).Limits; got != nil {
		t.Errorf("a 200 kept limits: %v", got)
	}

	stoodIn := exchange()
	stoodIn.Backend = "local"
	stoodIn.Attempt = &proxy.Attempt{Backend: "upstream", Why: "upstream answered HTTP 429", Fault: true, ResponseHeader: h}
	rec := FromExchange(stoodIn)
	if rec.Attempt == nil || !reflect.DeepEqual(rec.Attempt.Limits, want) {
		t.Errorf("attempt = %+v", rec.Attempt)
	}
	line, _ := json.Marshal(rec)
	if strings.Contains(string(line), "secret") || strings.Contains(string(line), "req_1") {
		t.Errorf("the record kept other headers: %s", line)
	}
}

func TestQuotaIsRecordedOnEveryUpstreamResponse(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.19")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1792114200")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.06")
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", "1792195200")
	h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
	h.Set("Anthropic-Ratelimit-Unified-Overage-Disabled-Reason", "org_level_disabled")
	want := &anthropic.Quota{Status: "allowed", Binding: "five_hour", Windows: map[string]anthropic.QuotaWindow{
		"5h": {Utilization: 0.19, Status: "allowed", Resets: time.Unix(1792114200, 0).UTC()},
		"7d": {Utilization: 0.06, Resets: time.Unix(1792195200, 0).UTC()},
	}}

	ok := exchange()
	ok.ResponseHeader = h
	if got := FromExchange(ok).Quota; !reflect.DeepEqual(got, want) {
		t.Errorf("quota on a 200 = %+v, want %+v", got, want)
	}

	// When the local model stood in, Claude's refusal still says where
	// the limits are.
	stoodIn := exchange()
	stoodIn.Backend = "local"
	stoodIn.Attempt = &proxy.Attempt{Backend: "upstream", Why: "upstream answered HTTP 429", Fault: true, ResponseHeader: h}
	if got := FromExchange(stoodIn).Quota; !reflect.DeepEqual(got, want) {
		t.Errorf("quota on a stand-in = %+v, want %+v", got, want)
	}

	local := exchange()
	local.Backend = "local"
	local.ResponseHeader = h
	if got := FromExchange(local).Quota; got != nil {
		t.Errorf("a local answer has a quota: %+v", got)
	}
	plain := exchange()
	plain.ResponseHeader = http.Header{"Retry-After": {"5"}}
	if got := FromExchange(plain).Quota; got != nil {
		t.Errorf("a response without the headers has a quota: %+v", got)
	}
}

func TestRecordHoldsNoContentOrCredentials(t *testing.T) {
	line, err := json.Marshal(FromExchange(exchange()))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-token", "Bearer", "secret system prompt", "private user text"} {
		if strings.Contains(string(line), secret) {
			t.Errorf("the ledger line contains %q:\n%s", secret, line)
		}
	}
}

func TestAbsentUsageFieldsAreLeftOut(t *testing.T) {
	ex := exchange()
	ex.Summary.Usage = anthropic.Usage{Input: 17, Output: 3, CacheRead: n(0)}
	line, _ := json.Marshal(FromExchange(ex))

	if !strings.Contains(string(line), `"usage":{"input":17,"output":3,"cache_read":0}`) {
		t.Errorf("usage is not exactly input, output, and a zero cache_read:\n%s", line)
	}
}

func TestOutcomes(t *testing.T) {
	cases := map[string]struct {
		edit        func(*proxy.Exchange)
		wantOutcome string
		wantError   string
		wantUsage   bool
	}{
		"ok":        {func(ex *proxy.Exchange) {}, OutcomeOK, "", true},
		"cut off":   {func(ex *proxy.Exchange) { ex.Summary.Complete = false }, OutcomeIncomplete, "", true},
		"cancelled": {func(ex *proxy.Exchange) { ex.Cancelled = true; ex.Summary.Complete = false }, OutcomeCancelled, "", true},
		"api error": {func(ex *proxy.Exchange) {
			ex.Status = 429
			ex.Summary = &anthropic.Summary{Complete: true, Error: &anthropic.APIError{Type: "rate_limit_error"}}
		}, OutcomeAPIError, "rate_limit_error", false},
		"error janus could not read": {func(ex *proxy.Exchange) { ex.Status = 502; ex.Summary = nil }, OutcomeHTTPError, "", false},
		"upstream unreachable":       {func(ex *proxy.Exchange) { ex.Status = 0; ex.Summary = nil }, OutcomeNoResponse, "", false},
		"unreadable encoding":        {func(ex *proxy.Exchange) { ex.Summary = nil; ex.Unread = "content-encoding br" }, OutcomeOK, "", false},
	}
	for name, c := range cases {
		ex := exchange()
		c.edit(&ex)
		rec := FromExchange(ex)
		if rec.Outcome != c.wantOutcome || rec.Error != c.wantError || (rec.Usage != nil) != c.wantUsage {
			t.Errorf("%s: outcome=%q error=%q usage=%v, want %q %q usage=%v",
				name, rec.Outcome, rec.Error, rec.Usage != nil, c.wantOutcome, c.wantError, c.wantUsage)
		}
	}
}

func TestNonMessagesRecord(t *testing.T) {
	rec := FromExchange(proxy.Exchange{
		ID: "x", Start: time.Now(), Method: "HEAD", Path: "/api/hello", Header: http.Header{}, Status: 200,
	})
	if rec.Class != "connectivity" || rec.Outcome != OutcomeOK || rec.Request != nil || rec.Response != nil || rec.Usage != nil {
		t.Errorf("record = %+v", rec)
	}
}

func TestQuotaCheckHasItsOwnClass(t *testing.T) {
	rec := FromExchange(proxy.Exchange{
		ID: "q", Start: time.Now(), Method: "POST", Path: "/v1/messages",
		Header:      http.Header{"X-Claude-Code-Session-Id": {"s1"}},
		RequestBody: []byte(`{"model":"claude-opus-5-5","max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`),
		Status:      429,
	})
	if rec.Class != "quota" {
		t.Errorf("class = %q", rec.Class)
	}
}

func TestCompactionHasItsOwnClass(t *testing.T) {
	rec := FromExchange(proxy.Exchange{
		ID: "c", Start: time.Now(), Method: "POST", Path: "/v1/messages",
		Header:      http.Header{"X-Claude-Code-Session-Id": {"s1"}},
		RequestBody: []byte(`{"model":"claude-opus-5-5","max_tokens":128000,"messages":[{"role":"user","content":"Your task is to create a detailed summary of the conversation so far."}]}`),
		Status:      200,
	})
	if rec.Class != "compaction" {
		t.Errorf("class = %q", rec.Class)
	}
}

func TestUnparseableOrTruncatedRequestStillGetsARecord(t *testing.T) {
	for name, edit := range map[string]func(*proxy.Exchange){
		"not json":  func(ex *proxy.Exchange) { ex.RequestBody = []byte("not json") },
		"truncated": func(ex *proxy.Exchange) { ex.RequestTruncated = true },
	} {
		ex := exchange()
		edit(&ex)
		rec := FromExchange(ex)
		if rec.Request != nil || rec.Usage == nil || rec.Outcome != OutcomeOK {
			t.Errorf("%s: request=%+v usage=%+v outcome=%q; want no request facts but the rest intact", name, rec.Request, rec.Usage, rec.Outcome)
		}
	}
}

func readLines(t *testing.T, path string) []Record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var recs []Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("ledger line is not a record: %v\n%s", err, sc.Text())
		}
		recs = append(recs, rec)
	}
	return recs
}

func TestAppendWritesOneFilePerUTCDay(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l := Open(dir)

	late := FromExchange(exchange()) // 14:39 UTC on the 11th
	next := FromExchange(exchange())
	next.ID, next.Time = "next", time.Date(2026, 10, 11, 21, 0, 0, 0, time.FixedZone("EDT", -4*3600)) // 01:00 UTC on the 12th
	for _, rec := range []Record{late, late, next} {
		if err := l.Append(rec); err != nil {
			t.Fatal(err)
		}
	}

	if got := readLines(t, filepath.Join(dir, "2026-10-11.jsonl")); len(got) != 2 || got[0].ID != late.ID {
		t.Errorf("2026-10-11.jsonl has %d records", len(got))
	}
	if got := readLines(t, filepath.Join(dir, "2026-10-12.jsonl")); len(got) != 1 || got[0].ID != "next" {
		t.Errorf("2026-10-12.jsonl = %+v", got)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "2026-10-11.jsonl"): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		// Windows has no such modes; the files take the directory's access list.
		if runtime.GOOS != "windows" && info.Mode().Perm() != want {
			t.Errorf("%s mode = %o, want %o", filepath.Base(path), info.Mode().Perm(), want)
		}
	}
}

func TestAppendFromManyGoroutinesKeepsLinesWhole(t *testing.T) {
	dir := t.TempDir()
	l := Open(dir)
	rec := FromExchange(exchange())

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Append(rec); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if got := readLines(t, filepath.Join(dir, "2026-10-11.jsonl")); len(got) != 50 {
		t.Errorf("%d whole records, want 50", len(got))
	}
}

func TestAppendReportsFailure(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o600)
	if err := Open(filepath.Join(blocker, "ledger")).Append(FromExchange(exchange())); err == nil {
		t.Error("Append into a path that cannot be a directory succeeded")
	}
}

func TestLocalExchangeRecord(t *testing.T) {
	ex := exchange()
	ex.Backend, ex.Route, ex.Reason = "local", "local", "model alias janus-local"
	ex.Stages = []string{"set_model", "lift_tool_additions"}
	ex.BackendModel = "qwen/qwen3.8-27b"
	ex.RequestBody = []byte(`{"model":"janus-local","messages":[{"role":"user","content":"hi"}]}`)
	ex.Summary = &anthropic.Summary{Model: "qwen/qwen3.8-27b", StopReason: "end_turn", Complete: true,
		Usage: anthropic.Usage{Input: 363, Output: 30, CacheRead: n(256)}}

	rec := FromExchange(ex)
	if rec.Backend != "local" || rec.Route != "local" || rec.Reason != "model alias janus-local" || strings.Join(rec.Stages, ",") != "set_model,lift_tool_additions" {
		t.Errorf("decision = %+v", rec)
	}
	// The request names what the client asked for; the response, what
	// actually answered.
	if rec.Request.Model != "janus-local" || rec.Response.Model != "qwen/qwen3.8-27b" {
		t.Errorf("request model %q, response model %q", rec.Request.Model, rec.Response.Model)
	}

	// LM Studio reported 363 input of which 256 came from its cache; the
	// record separates them the way the Anthropic API would have.
	if rec.Usage.Input != 107 || *rec.Usage.CacheRead != 256 {
		t.Errorf("usage = input %d, cache read %d; want 107 and 256", rec.Usage.Input, *rec.Usage.CacheRead)
	}
	upstream := exchange()
	upstream.Summary.Usage = anthropic.Usage{Input: 2, Output: 1, CacheRead: n(18327)}
	if got := FromExchange(upstream).Usage.Input; got != 2 {
		t.Errorf("an upstream record's input was adjusted to %d", got)
	}

	rep := Summarise([]Record{rec}, DefaultPrices)
	if rep.Total.CostUSD != 0 || rep.Total.Unpriced != 0 || rep.ByBackend[0].Key != "local" {
		t.Errorf("a local answer should cost nothing upstream and not be unpriced: %+v", rep.Total)
	}
}

func TestAttemptRecords(t *testing.T) {
	// Accepted: the record is the local answer, and the attempt says so.
	accepted := exchange()
	accepted.Backend, accepted.Route, accepted.Reason = "local", "local", "rule subagents: local answer accepted"
	accepted.Summary = &anthropic.Summary{Model: "qwen/qwen3.8-27b", StopReason: "end_turn", Complete: true, Usage: anthropic.Usage{Input: 300, Output: 30, CacheRead: n(100)}}
	accepted.Attempt = &proxy.Attempt{Backend: "local", Model: "qwen/qwen3.8-27b", Accepted: true, Duration: 9 * time.Second}
	rec := FromExchange(accepted)
	if a := rec.Attempt; a == nil || a.Verdict != VerdictAccepted || a.Why != "" || a.DurationMs != 9000 || a.Usage != nil {
		t.Errorf("accepted attempt = %+v", a)
	}
	if rec.Backend != "local" || rec.Usage.Input != 200 {
		t.Errorf("an accepted record should be the local answer: backend %q, usage %+v", rec.Backend, rec.Usage)
	}

	// Rejected: the record is Claude's answer; the attempt holds why, and
	// what the local model spent.
	rejected := exchange() // an upstream exchange
	rejected.Reason = "rule subagents: the local answer has no text and no tool call; sent upstream"
	rejected.Attempt = &proxy.Attempt{Backend: "local", Model: "qwen/qwen3.8-27b", Why: "the local answer has no text and no tool call", Duration: 4 * time.Second,
		Summary: &anthropic.Summary{Complete: true, Usage: anthropic.Usage{Input: 65, Output: 14, CacheRead: n(60)}}}
	rec = FromExchange(rejected)
	if a := rec.Attempt; a == nil || a.Verdict != VerdictRejected || !strings.Contains(a.Why, "no text") || a.Usage == nil || a.Usage.Input != 5 || a.Usage.Output != 14 {
		t.Errorf("rejected attempt = %+v usage %+v", a, a.Usage)
	}
	if rec.Backend != "upstream" || rec.Response.Model != "claude-opus-5-5" {
		t.Errorf("a rejected record should be Claude's answer: %+v", rec.Response)
	}

	// Failed: the local model was not there.
	failed := exchange()
	failed.Attempt = &proxy.Attempt{Backend: "local", Model: "qwen/qwen3.8-27b", Fault: true, Why: "could not reach local: connection refused", Duration: 3 * time.Millisecond}
	if a := FromExchange(failed).Attempt; a.Verdict != VerdictFailed || a.Usage != nil {
		t.Errorf("failed attempt = %+v", a)
	}

	// Claude was down and the local model stood in.
	stoodIn := exchange()
	stoodIn.Backend, stoodIn.Route = "local", "local"
	stoodIn.Attempt = &proxy.Attempt{Backend: "upstream", Fault: true, Why: "upstream answered HTTP 529"}
	if a := FromExchange(stoodIn).Attempt; a.Backend != "upstream" || a.Verdict != VerdictFailed {
		t.Errorf("stand-in attempt = %+v", a)
	}
	if FromExchange(accepted).Attempt.Backend != "local" {
		t.Error("a local-first attempt should name the local backend")
	}

	if FromExchange(exchange()).Attempt != nil {
		t.Error("an ordinary exchange has an attempt")
	}
}

func TestRecordCarriesTransforms(t *testing.T) {
	ex := exchange()
	ex.Transforms = []transform.Effect{{Name: "digest", Blocks: 2, Rewritten: 1, Declined: 1, Tokens: 900, SavedUSD: 0.0072, ExpectedUSD: 0.009}}
	rec := FromExchange(ex)
	want := []Transform{{Name: "digest", Blocks: 2, Rewritten: 1, Declined: 1, Tokens: 900, SavedUSD: 0.0072, ExpectedUSD: 0.009}}
	if len(rec.Transforms) != 1 || rec.Transforms[0] != want[0] {
		t.Errorf("transforms = %+v, want %+v", rec.Transforms, want)
	}
	line, _ := json.Marshal(rec)
	if !strings.Contains(string(line), `"transforms":[{"name":"digest","blocks":2,"rewritten":1,"declined":1,"tokens":900,"saved_usd":0.0072,"expected_usd":0.009}]`) {
		t.Errorf("ledger line = %s", line)
	}
	if line, _ := json.Marshal(FromExchange(exchange())); strings.Contains(string(line), "transforms") {
		t.Error("a request no transform touched has a transforms field")
	}
}
