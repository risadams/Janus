package anthropic

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Quota is what one response said about the subscription's usage limits,
// read from its Anthropic-Ratelimit-Unified-* headers. A subscription has
// several limits at once, each over its own window; the API reports each
// one's share used and names the one that binds.
type Quota struct {
	// Windows is each limit by the API's own name for its window, such as
	// "5h" and "7d".
	Windows map[string]QuotaWindow `json:"windows"`
	// Status is the account's overall status: allowed, allowed_warning,
	// or rejected.
	Status string `json:"status,omitempty"`
	// Binding is the limit the API named as the one that counts now,
	// such as five_hour or seven_day.
	Binding string `json:"binding,omitempty"`
}

// QuotaWindow is one limit's state.
type QuotaWindow struct {
	// Utilization is the share of the limit used, from 0; above 1 means
	// it was exceeded.
	Utilization float64   `json:"utilization"`
	Status      string    `json:"status,omitempty"`
	Resets      time.Time `json:"resets,omitzero"`
}

const unifiedPrefix = "anthropic-ratelimit-unified-"

// QuotaFrom reads the subscription's limits from a response's headers. It
// is nil when there are none, as for an API key, which has rate limits of
// another kind, or for any backend but Anthropic's.
func QuotaFrom(h http.Header) *Quota {
	var q *Quota
	get := func(name string) string { return h.Get(unifiedPrefix + name) }
	for k := range h {
		name := strings.ToLower(k)
		window, ok := strings.CutSuffix(strings.TrimPrefix(name, unifiedPrefix), "-utilization")
		if !ok || !strings.HasPrefix(name, unifiedPrefix) || window == "" {
			continue
		}
		used, err := strconv.ParseFloat(h.Get(k), 64)
		if err != nil {
			continue
		}
		w := QuotaWindow{Utilization: used, Status: get(window + "-status")}
		if epoch, err := strconv.ParseInt(get(window+"-reset"), 10, 64); err == nil {
			w.Resets = time.Unix(epoch, 0).UTC()
		}
		if q == nil {
			q = &Quota{Windows: map[string]QuotaWindow{}}
		}
		q.Windows[window] = w
	}
	if q != nil {
		q.Status, q.Binding = get("status"), get("representative-claim")
	}
	return q
}
