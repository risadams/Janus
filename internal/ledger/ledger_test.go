package ledger

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/proxy"
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
		if info.Mode().Perm() != want {
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
