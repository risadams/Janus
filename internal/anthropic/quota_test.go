package anthropic

import (
	"net/http"
	"testing"
)

func TestQuotaFromFindsEveryWindowAndSkipsTheRest(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.5")
	// A window the API might add later is picked up by its suffix.
	h.Set("Anthropic-Ratelimit-Unified-7d_opus-Utilization", "1.01")
	h.Set("Anthropic-Ratelimit-Unified-7d_opus-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Bad-Utilization", "lots")
	h.Set("Anthropic-Ratelimit-Unified-Fallback-Percentage", "0.5")

	q := QuotaFrom(h)
	if q == nil || len(q.Windows) != 2 || q.Windows["5h"].Utilization != 0.5 ||
		q.Windows["7d_opus"].Utilization != 1.01 || q.Windows["7d_opus"].Status != "rejected" {
		t.Errorf("quota = %+v, want the 5h and 7d_opus windows only", q)
	}
	if QuotaFrom(http.Header{"Content-Type": {"text/event-stream"}}) != nil {
		t.Error("a quota from headers that have none")
	}
}
