package ledger

import (
	"testing"
	"time"
)

func TestOutagesGroupWhatClaudeCouldNotAnswer(t *testing.T) {
	after := func(m int) time.Time { return at(10, 9).Add(time.Duration(m) * time.Minute) }
	limited := func(id string, when time.Time) Record {
		r := rec(id, when, "main", nil)
		r.Status, r.Outcome, r.Error = 429, OutcomeAPIError, "rate_limit_error"
		r.Limits = map[string]string{"anthropic-ratelimit-unified-representative-claim": "five_hour",
			"anthropic-ratelimit-unified-reset": "1791937800"}
		return r
	}
	stoodIn := func(id string, when time.Time, why string) Record {
		r := rec(id, when, "main", &Usage{Input: 10})
		r.Backend = "local"
		r.Attempt = &Attempt{Backend: "upstream", Verdict: VerdictFailed, Why: why,
			Limits: map[string]string{"retry-after": "600"}}
		return r
	}
	overloaded := rec("overloaded", after(100), "main", nil)
	overloaded.Status, overloaded.Error = 529, "overloaded_error"
	unreachable := rec("unreachable", after(102), "main", nil)
	unreachable.Status, unreachable.Outcome = 0, OutcomeNoResponse
	quota := rec("quota", after(5), "quota", nil)
	quota.Status = 429

	records := []Record{
		limited("a", after(0)),
		quota, // normally answered with a 429; not an outage
		stoodIn("b", after(10), "upstream answered HTTP 429"),
		stoodIn("c", after(25), "upstream answered HTTP 429"),
		rec("fine", after(30), "main", &Usage{Input: 1}),
		// 75 minutes after c: a new outage.
		overloaded,
		unreachable,
		stoodIn("d", after(103), "could not reach upstream: connection reset by peer"),
	}

	got := Outages(records)
	if len(got) != 2 {
		t.Fatalf("outages = %+v, want two", got)
	}
	first := got[0]
	if !first.Start.Equal(after(0)) || !first.End.Equal(after(25)) || first.Cause != OutageRateLimited ||
		first.Requests != 3 || first.StoodIn != 2 || first.Limit != "five_hour" ||
		!first.Lifts.Equal(time.Unix(1791937800, 0)) {
		t.Errorf("first outage = %+v, want 3 rate-limited requests over 25 minutes, 2 stood in, five_hour lifting at its reset", first)
	}
	second := got[1]
	if second.Cause != OutageUnreachable || second.Requests != 3 || second.StoodIn != 1 || second.Limit != "" ||
		!second.Lifts.Equal(after(103).Add(10*time.Minute)) {
		t.Errorf("second outage = %+v, want 3 requests, mostly unreachable, 1 stood in, answering again after retry-after", second)
	}
}
