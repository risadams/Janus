package route

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/openai/openaitest"
)

// clock is a fake clock a test moves by hand.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// A session moves off Claude at the 429 that says when the limit resets,
// stays off until then without asking Claude again, and goes back on the
// first turn after, including across a restart of janus (#378).
func TestASessionReturnsToClaudeWhenItsLimitResets(t *testing.T) {
	c := &clock{t: time.Now()}
	limited := true
	var sent []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limited {
			sent = append(sent, "429")
			w.Header().Set("Retry-After", "3600")
			http.Error(w, `{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`, http.StatusTooManyRequests)
			return
		}
		claudeAnswers(&sent)(w, r)
	}))
	defer up.Close()
	oa, srv := fakeOpenAI(t,
		openaitest.Answer(openaitest.Text{Text: "1"}), openaitest.Answer(openaitest.Text{Text: "2"}), openaitest.Answer(openaitest.Text{Text: "3"}))
	file := filepath.Join(t.TempDir(), LimitsFile)
	start := func() *listJanus {
		j := listRouter(t, up, oa, "upstream", "openai")
		j.resets.now = c.now
		j.down.now = c.now
		if err := j.Remember(file); err != nil {
			t.Fatal(err)
		}
		return j
	}
	j := start()

	if res := j.send(t, opusRequest); res.Exchange.Backend != "openai" {
		t.Fatalf("at the limit: answered by %s", res.Exchange.Backend)
	}
	c.t = c.t.Add(30 * time.Minute)
	res := j.send(t, opusRequest)
	if res.Exchange.Backend != "openai" || len(sent) != 1 || !strings.Contains(res.Exchange.Reason, "Claude is out: Claude's usage limit was reached; it resets at") {
		t.Errorf("before the reset: %s answered, Claude was asked %d times, reason %q", res.Exchange.Backend, len(sent), res.Exchange.Reason)
	}

	// janus restarts, and still knows.
	j = start()
	c.t = c.t.Add(20 * time.Minute)
	if res := j.send(t, opusRequest); res.Exchange.Backend != "openai" || len(sent) != 1 {
		t.Errorf("after a restart, before the reset: %s answered, Claude asked %d times", res.Exchange.Backend, len(sent))
	}

	limited = false
	c.t = c.t.Add(11 * time.Minute)
	if res := j.send(t, opusRequest); res.Exchange.Backend != "upstream" {
		t.Errorf("after the reset: answered by %s", res.Exchange.Backend)
	}
	if len(srv.Requests()) != 3 {
		t.Errorf("OpenAI was asked %d times, want 3", len(srv.Requests()))
	}
}

// An outage has no reset time: the provider is left alone for a backoff
// that doubles while the failures go on, and its first answer ends it.
func TestAnOutageBacksOffAndEndsWithAnAnswer(t *testing.T) {
	c := &clock{t: time.Now()}
	r := resets{now: c.now}
	r.outage("upstream")
	if r.out("upstream") == "" {
		t.Fatal("not left alone after an outage")
	}
	c.t = c.t.Add(outageBackoff + time.Second)
	if r.out("upstream") != "" {
		t.Fatal("still left alone after the backoff")
	}
	r.outage("upstream")
	c.t = c.t.Add(outageBackoff + time.Second)
	if r.out("upstream") == "" {
		t.Error("the second backoff is no longer than the first")
	}
	for range 10 {
		r.outage("upstream")
	}
	if b := r.outages["upstream"]; b.wait != maxOutageBackoff {
		t.Errorf("backoff grew to %s, want at most %s", b.wait, maxOutageBackoff)
	}
	r.answered("upstream")
	if r.out("upstream") != "" {
		t.Error("an answer did not end the outage")
	}
}

func TestOpenAIsWaitIsTheResetOfTheLimitUsedUp(t *testing.T) {
	h := http.Header{"X-Ratelimit-Remaining-Requests": {"12"}, "X-Ratelimit-Reset-Requests": {"30s"},
		"X-Ratelimit-Remaining-Tokens": {"0"}, "X-Ratelimit-Reset-Tokens": {"6m0s"}}
	if got := openaiWait(h); got != 6*time.Minute {
		t.Errorf("wait = %s, want the used-up token limit's 6m", got)
	}
	h.Set("X-Ratelimit-Remaining-Tokens", "500")
	if got := openaiWait(h); got != 0 {
		t.Errorf("wait = %s with nothing used up", got)
	}
}

// OpenAI's limits vary by model (#451): a rate limit on the model Haiku
// requests go to leaves the model Opus requests go to answering, and a
// used-up quota, which is the account's, takes both out.
func TestAnOpenAIRateLimitIsOnlyItsModels(t *testing.T) {
	c := &clock{t: time.Now()}
	long := openaitest.RateLimited("rate_limit_exceeded", "3600")
	ok := openaitest.Answer(openaitest.Text{Text: "ok"})
	oa, srv := fakeOpenAI(t, long, ok, openaitest.RateLimited("insufficient_quota", ""), ok)
	var sent []string
	up := httptest.NewServer(claudeAnswers(&sent))
	defer up.Close()
	j := listRouter(t, up, oa, "openai", "upstream")
	j.resets.now = c.now
	haiku := strings.Replace(opusRequest, "claude-opus-5-5", "claude-haiku-5-5", 1)

	// Haiku's model reaches its limit: that turn moves on to Claude.
	if res := j.send(t, haiku); res.Exchange.Backend != "upstream" {
		t.Fatalf("at the limit: answered by %s", res.Exchange.Backend)
	}
	// Until the reset, Haiku's model is passed over without being asked...
	res := j.send(t, haiku)
	if res.Exchange.Backend != "upstream" || len(srv.Requests()) != 1 || !strings.Contains(res.Exchange.Reason, "OpenAI (gpt-6-luna) is out: OpenAI's rate limit for gpt-6-luna was reached") {
		t.Errorf("Haiku before the reset: %s, OpenAI asked %d times, reason %q", res.Exchange.Backend, len(srv.Requests()), res.Exchange.Reason)
	}
	// ...and Opus's model is still asked.
	if res := j.send(t, opusRequest); res.Exchange.Backend != "openai" {
		t.Errorf("Opus's model was taken out by Haiku's limit: answered by %s", res.Exchange.Backend)
	}
	// The account's quota runs out on Opus's model: both are out.
	j.send(t, opusRequest)
	res = j.send(t, opusRequest)
	if res.Exchange.Backend != "upstream" || !strings.Contains(res.Exchange.Reason, "OpenAI is out until janus restarts") {
		t.Errorf("after the quota: %s, reason %q", res.Exchange.Backend, res.Exchange.Reason)
	}
}

// ifUsageAbove names one model's limit, or "openai.tokens" for any
// model's: the fullest of them.
func TestARuleCanNameOneOpenAIModelsLimitOrAnys(t *testing.T) {
	var h headroom
	now := time.Now()
	h.observe(RouteOpenAI, &anthropic.Quota{Windows: map[string]anthropic.QuotaWindow{"openai.gpt-6.1-sol.tokens": {Utilization: 0.2}}})
	h.observe(RouteOpenAI, &anthropic.Quota{Windows: map[string]anthropic.QuotaWindow{"openai.gpt-6-luna.tokens": {Utilization: 0.9}}})
	used := h.used(now)
	if used["openai.gpt-6.1-sol.tokens"] != 0.2 || used["openai.gpt-6-luna.tokens"] != 0.9 || used["openai.tokens"] != 0.9 {
		t.Errorf("used = %v", used)
	}
}
