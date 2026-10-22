package route

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/openai"
)

// headroom keeps Claude's latest word on the subscription's usage limits,
// from the headers of each answer, so that a rule can send work local
// before a limit is reached rather than after (#240).
//
// It keeps each provider's latest apart, Claude's and OpenAI's (#382), so
// a rule can name either: OpenAI's windows are "openai.requests" and
// "openai.tokens".
type headroom struct {
	mu     sync.Mutex
	latest map[string]*anthropic.Quota
}

func (h *headroom) observe(provider string, q *anthropic.Quota) {
	if q == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.latest == nil {
		h.latest = map[string]*anthropic.Quota{}
	}
	if old := h.latest[provider]; old != nil && provider == RouteOpenAI {
		// Each OpenAI answer reports one model's limits; another model's
		// stay as they last were (#451).
		merged := &anthropic.Quota{Windows: map[string]anthropic.QuotaWindow{}}
		for name, w := range old.Windows {
			merged.Windows[name] = w
		}
		for name, w := range q.Windows {
			merged.Windows[name] = w
		}
		q = merged
	}
	h.latest[provider] = q
}

// used is each limit's utilization as last reported, leaving out any
// whose reset time has passed: its use has started again from nothing,
// and until Claude next answers, how much is used is unknown.
func (h *headroom) used(now time.Time) map[string]float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.latest == nil {
		return nil
	}
	out := map[string]float64{}
	for _, q := range h.latest {
		for name, w := range q.Windows {
			if !w.Resets.IsZero() && !now.Before(w.Resets) {
				continue
			}
			out[name] = w.Utilization
			// "openai.tokens" is any OpenAI model's: the fullest of them.
			if kind := openai.QuotaKind(name); kind != "" && name != kind {
				out[kind] = max(out[kind], w.Utilization)
			}
		}
	}
	return out
}

// usageMismatch says why a rule's ifUsageAbove does not match, or, when
// it does, which limit made it: one is enough.
func (rule Rule) usageMismatch(used map[string]float64) (mismatch, matched string) {
	windows := make([]string, 0, len(rule.IfUsageAbove))
	for w := range rule.IfUsageAbove {
		windows = append(windows, w)
	}
	sort.Strings(windows)
	var under []string
	for _, w := range windows {
		limit := rule.IfUsageAbove[w]
		got, known := used[w]
		switch {
		case !known:
			under = append(under, fmt.Sprintf("%s limit use not known yet", w))
		case got > limit:
			return "", fmt.Sprintf("%s limit %.0f%% used, over %.0f%%", w, got*100, limit*100)
		default:
			under = append(under, fmt.Sprintf("%s limit %.0f%% used, not over %.0f%%", w, got*100, limit*100))
		}
	}
	return strings.Join(under, "; "), ""
}
