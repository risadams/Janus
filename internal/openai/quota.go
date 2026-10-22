package openai

import (
	"net/http"
	"strconv"
	"time"

	"github.com/risadams/janus/internal/anthropic"
)

// QuotaWindows are the names OpenAI's limits are recorded under, beside
// Claude's "5h" and "7d" (#382): per-minute limits on requests and on
// tokens, from its x-ratelimit-* headers. The prefix keeps them apart
// from Claude's in the ledger and in a rule's ifUsageAbove.
const (
	QuotaRequests = "openai.requests"
	QuotaTokens   = "openai.tokens"
)

// QuotaFrom reads OpenAI's rate limits from a response's headers, as now:
// each limit's share used and when it resets. The resets are durations
// from the answer, such as "6m0s" (docs/research/openai.md, Errors). It is
// nil when there are none. It reads only the x-ratelimit-* headers, none
// of which names the account.
func QuotaFrom(h http.Header, now time.Time) *anthropic.Quota {
	var q *anthropic.Quota
	for window, limit := range map[string]string{QuotaRequests: "Requests", QuotaTokens: "Tokens"} {
		total, err1 := strconv.ParseFloat(h.Get("X-Ratelimit-Limit-"+limit), 64)
		left, err2 := strconv.ParseFloat(h.Get("X-Ratelimit-Remaining-"+limit), 64)
		if err1 != nil || err2 != nil || total <= 0 {
			continue
		}
		w := anthropic.QuotaWindow{Utilization: max(0, 1-left/total)}
		if left <= 0 {
			w.Status = "rejected"
		}
		if d, err := time.ParseDuration(h.Get("X-Ratelimit-Reset-" + limit)); err == nil {
			w.Resets = now.Add(d).UTC().Truncate(time.Second)
		}
		if q == nil {
			q = &anthropic.Quota{Windows: map[string]anthropic.QuotaWindow{}}
		}
		q.Windows[window] = w
	}
	return q
}
