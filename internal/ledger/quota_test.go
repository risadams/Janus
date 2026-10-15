package ledger

import (
	"math"
	"testing"

	"github.com/risadams/janus/internal/anthropic"
)

func TestQuotaIsSharedOutByTheClassThatRaisedIt(t *testing.T) {
	with := func(r Record, fiveHour float64) Record {
		r.Quota = &anthropic.Quota{Windows: map[string]anthropic.QuotaWindow{"5h": {Utilization: fiveHour}, "7d": {Utilization: 0.5}}}
		return r
	}
	records := []Record{
		with(rec("a", at(10, 1), "main", nil), 0.10), // the starting point: nothing is put down to it
		with(rec("b", at(10, 2), "main", nil), 0.16),
		with(rec("c", at(10, 3), "safety", nil), 0.18),
		rec("d", at(10, 4), "subagent", nil), // no quota: a local answer
		with(rec("e", at(10, 5), "main", nil), 0.18),
		with(rec("f", at(10, 6), "main", nil), 0.02), // a reset
		with(rec("g", at(10, 7), "safety", nil), 0.04),
	}

	q := summariseQuota(records)
	if q == nil || !q.At.Equal(at(10, 7)) || q.Latest.Windows["5h"].Utilization != 0.04 {
		t.Fatalf("summary = %+v, want the latest from g", q)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if len(q.ByClass) != 2 || q.ByClass[0].Class != "main" || !near(q.ByClass[0].Added, 0.06) || !near(q.ByClass[0].Share, 0.6) ||
		q.ByClass[1].Class != "safety" || !near(q.ByClass[1].Added, 0.04) {
		t.Errorf("by class = %+v, want main 0.06 (60%%) then safety 0.04; the reset is not a share", q.ByClass)
	}
	if summariseQuota([]Record{rec("x", at(10, 1), "main", nil)}) != nil {
		t.Error("a summary from records with no quota")
	}
}
