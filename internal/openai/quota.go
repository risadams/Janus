package openai

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/risadams/janus/internal/anthropic"
)

// OpenAI's limits are recorded under names beside Claude's "5h" and "7d"
// (#382): per-minute limits on requests and on tokens, from its
// x-ratelimit-* headers. OpenAI's limits vary by model, and some families
// share one (#451), so each is named for the model it came from:
// "openai.gpt-6.1-sol.tokens". A rule's ifUsageAbove names one of those,
// or "openai.tokens" for any OpenAI model's. The prefix keeps them apart
// from Claude's in the ledger.
const (
	QuotaRequests = "openai.requests"
	QuotaTokens   = "openai.tokens"
)

// QuotaName is the name of model's limit, kind QuotaRequests or
// QuotaTokens: "openai.<model>.requests".
func QuotaName(model, kind string) string {
	if model == "" {
		return kind
	}
	return "openai." + model + strings.TrimPrefix(kind, "openai")
}

// QuotaKind is which of the two limits name is, whatever model it names:
// QuotaRequests, QuotaTokens, or "" for a name that is neither.
func QuotaKind(name string) string {
	switch {
	case name == QuotaRequests || strings.HasSuffix(name, ".requests") && strings.HasPrefix(name, "openai."):
		return QuotaRequests
	case name == QuotaTokens || strings.HasSuffix(name, ".tokens") && strings.HasPrefix(name, "openai."):
		return QuotaTokens
	}
	return ""
}

// QuotaFrom reads OpenAI's rate limits from a response's headers, as now:
// each limit's share used and when it resets. The resets are durations
// from the answer, such as "6m0s" (docs/research/openai.md, Errors). It is
// nil when there are none. It reads only the x-ratelimit-* headers, none
// of which names the account.
func QuotaFrom(h http.Header, now time.Time, model string) *anthropic.Quota {
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
		q.Windows[QuotaName(model, window)] = w
	}
	return q
}
