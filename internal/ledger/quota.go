package ledger

import (
	"sort"
	"time"

	"github.com/risadams/janus/internal/anthropic"
)

// QuotaSummary is what the ledger says about the subscription's limits
// over a range (#239): where they stood at the last response that said,
// and which kinds of request used up the five-hour limit.
type QuotaSummary struct {
	// At is when Latest was reported.
	At     time.Time        `json:"at"`
	Latest *anthropic.Quota `json:"latest"`
	// ByClass is each class's share of the rise in five-hour utilization
	// over the range, largest first.
	ByClass []QuotaShare `json:"by_class,omitempty"`
}

// QuotaShare is how much of the five-hour limit one class used.
type QuotaShare struct {
	Class string `json:"class"`
	// Added is the rise in utilization that followed this class's
	// requests, in the API's units: 0.01 is one percent of the limit.
	Added float64 `json:"added"`
	// Share is Added as a part of the whole rise, from 0 to 1.
	Share float64 `json:"share"`
}

// quotaWindow is the window whose use is shared out by class: the one
// that has run out in practice.
const quotaWindow = "5h"

// merged is the latest word on every provider's limits: q's windows over
// those of before, and the account's status from Claude's alone. Claude's
// and OpenAI's windows have different names (openai.QuotaFrom), so one
// provider's answer leaves the other's limits as they were (#382).
func merged(before, q *anthropic.Quota) *anthropic.Quota {
	if before == nil {
		return q
	}
	out := &anthropic.Quota{Windows: map[string]anthropic.QuotaWindow{}, Status: before.Status, Binding: before.Binding}
	for name, w := range before.Windows {
		out.Windows[name] = w
	}
	for name, w := range q.Windows {
		out.Windows[name] = w
	}
	if q.Status != "" || q.Binding != "" {
		out.Status, out.Binding = q.Status, q.Binding
	}
	return out
}

// summariseQuota works out the limits' summary from records in time
// order. Each rise in five-hour utilization is put down to the request
// whose response reported it. The API reports utilization in hundredths
// and for the whole account, so a single request's share is rough; over
// a day the shares say which kinds of request use the limit. A fall is a
// reset and is skipped. It is nil when no record has a quota.
func summariseQuota(records []Record) *QuotaSummary {
	var sum *QuotaSummary
	added := map[string]float64{}
	var total, prev float64
	havePrev := false
	for _, rec := range records {
		if rec.Quota == nil || rec.ShadowOf != "" {
			continue
		}
		if sum == nil {
			sum = &QuotaSummary{}
		}
		sum.At, sum.Latest = rec.Time, merged(sum.Latest, rec.Quota)
		w, ok := rec.Quota.Windows[quotaWindow]
		if !ok {
			continue
		}
		if havePrev && w.Utilization > prev {
			added[rec.Class] += w.Utilization - prev
			total += w.Utilization - prev
		}
		prev, havePrev = w.Utilization, true
	}
	if sum == nil {
		return nil
	}
	for class, a := range added {
		sum.ByClass = append(sum.ByClass, QuotaShare{Class: class, Added: a, Share: a / total})
	}
	sort.Slice(sum.ByClass, func(i, j int) bool {
		if sum.ByClass[i].Added != sum.ByClass[j].Added {
			return sum.ByClass[i].Added > sum.ByClass[j].Added
		}
		return sum.ByClass[i].Class < sum.ByClass[j].Class
	})
	return sum
}
