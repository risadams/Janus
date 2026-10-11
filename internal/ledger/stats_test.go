package ledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func at(day, hour int) time.Time { return time.Date(2026, 10, day, hour, 0, 0, 0, time.UTC) }

// rec builds a Messages record on claude-opus-5-5.
func rec(id string, when time.Time, class string, u *Usage) Record {
	r := Record{V: 1, ID: id, Time: when, Method: "POST", Path: "/v1/messages", Class: class,
		Route: "upstream", Backend: "upstream", Status: 200, Outcome: OutcomeOK, Usage: u}
	if u != nil {
		r.Response = &Response{Model: "claude-opus-5-5"}
	}
	return r
}

func write(t *testing.T, dir string, recs ...Record) {
	t.Helper()
	l := Open(dir)
	for _, r := range recs {
		if err := l.Append(r); err != nil {
			t.Fatal(err)
		}
	}
}

func ids(recs []Record) string {
	var out []string
	for _, r := range recs {
		out = append(out, r.ID)
	}
	return strings.Join(out, ",")
}

func TestReadFiltersByTime(t *testing.T) {
	dir := t.TempDir()
	write(t, dir,
		rec("d9-late", at(9, 23), "main", nil),
		rec("d10-early", at(10, 1), "main", nil),
		rec("d10-late", at(10, 22), "main", nil),
		rec("d11-early", at(11, 3), "main", nil),
		rec("d12", at(12, 12), "main", nil),
	)

	cases := []struct {
		name         string
		since, until time.Time
		want         string
	}{
		{"everything", time.Time{}, time.Time{}, "d9-late,d10-early,d10-late,d11-early,d12"},
		{"since mid-day", at(10, 12), time.Time{}, "d10-late,d11-early,d12"},
		{"until is exclusive", time.Time{}, at(10, 22), "d9-late,d10-early"},
		{"since is inclusive", at(10, 22), at(11, 3), "d10-late"},
		{"a window inside one day", at(10, 0), at(10, 12), "d10-early"},
		{"nothing in range", at(20, 0), time.Time{}, ""},
	}
	for _, c := range cases {
		got, malformed, err := Read(dir, c.since, c.until)
		if err != nil || malformed != 0 || ids(got) != c.want {
			t.Errorf("%s: got %q (malformed %d, err %v), want %q", c.name, ids(got), malformed, err, c.want)
		}
	}
}

func TestReadSkipsWhatIsNotARecord(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, rec("good-1", at(10, 1), "main", nil))
	f, _ := os.OpenFile(filepath.Join(dir, "2026-10-10.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("not json at all\n{\"some\":\"other object\"}\n\n")
	f.Close()
	write(t, dir, rec("good-2", at(10, 2), "main", nil))
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine"), 0o600)
	os.WriteFile(filepath.Join(dir, "not-a-date.jsonl"), []byte("{}\n"), 0o600)
	os.Mkdir(filepath.Join(dir, "2026-10-11.jsonl"), 0o700)

	got, malformed, err := Read(dir, time.Time{}, time.Time{})
	if err != nil || ids(got) != "good-1,good-2" || malformed != 3 {
		t.Errorf("got %q, malformed %d, err %v; want both good records and 3 skipped lines", ids(got), malformed, err)
	}
}

func TestReadOfMissingLedgerIsEmpty(t *testing.T) {
	got, malformed, err := Read(filepath.Join(t.TempDir(), "nope"), time.Time{}, time.Time{})
	if err != nil || len(got) != 0 || malformed != 0 {
		t.Errorf("got %d records, malformed %d, err %v", len(got), malformed, err)
	}
}

func TestSummarise(t *testing.T) {
	main1 := rec("m1", at(10, 1), "main", &Usage{Input: 2, Output: 100, CacheRead: n(9000), CacheWrite: n(998), CacheWrite1h: n(998), CacheWrite5m: n(0)})
	main2 := rec("m2", at(10, 2), "main", &Usage{Input: 2, Output: 50, CacheRead: n(9998), CacheWrite: n(0)})
	sub := rec("s1", at(10, 3), "subagent", &Usage{Input: 2, Output: 400, CacheRead: n(0), CacheWrite: n(20000), CacheWrite5m: n(20000), CacheWrite1h: n(0)})
	hello := rec("h1", at(10, 0), "connectivity", nil)
	hello.Method, hello.Path = "HEAD", "/api/hello"
	failed := rec("f1", at(10, 4), "main", nil)
	failed.Status, failed.Outcome, failed.Error = 429, OutcomeAPIError, "rate_limit_error"

	rep := Summarise([]Record{hello, main1, main2, sub, failed}, DefaultPrices)

	if !rep.From.Equal(at(10, 0)) || !rep.To.Equal(at(10, 4)) {
		t.Errorf("range = %v to %v", rep.From, rep.To)
	}
	if rep.Outcomes[OutcomeOK] != 4 || rep.Outcomes[OutcomeAPIError] != 1 {
		t.Errorf("outcomes = %v", rep.Outcomes)
	}

	// main:     2+2 input, 150 output, 18,998 read, 998 written for an hour
	//           4x4 + 150x20 + 18,998x0.2 + 998x8 = 14,799.6 millionths
	// subagent: 2 input, 400 output, 20,000 written for five minutes
	//           2x4 + 400x20 + 20,000x5 = 108,008 millionths
	total := rep.Total
	if total.Requests != 5 || total.Input != 6 || total.Output != 550 || total.CacheRead != 18998 || total.CacheWrite != 20998 {
		t.Errorf("total = %+v", total)
	}
	if !near(total.CostUSD, 0.1228076) || total.Unpriced != 0 {
		t.Errorf("total cost = $%.7f (unpriced %d), want $0.1228076", total.CostUSD, total.Unpriced)
	}

	// The subagent turn is one request in five and most of the cost, so it
	// leads.
	if len(rep.ByClass) != 3 || rep.ByClass[0].Key != "subagent" || rep.ByClass[1].Key != "main" || rep.ByClass[2].Key != "connectivity" {
		t.Fatalf("by class = %+v", rep.ByClass)
	}
	subRow, mainRow := rep.ByClass[0], rep.ByClass[1]
	if !near(subRow.CostUSD, 0.108008) || !near(mainRow.CostUSD, 0.0147996) || mainRow.Requests != 3 {
		t.Errorf("subagent $%.6f, main $%.7f over %d requests", subRow.CostUSD, mainRow.CostUSD, mainRow.Requests)
	}
	if got := mainRow.CacheHitRatio(); !near(got, 18998.0/20000.0) {
		t.Errorf("main cache hit ratio = %f, want 0.9499", got)
	}
	if subRow.CacheHitRatio() != 0 || rep.ByClass[2].CacheHitRatio() != -1 {
		t.Errorf("cache hit ratios: subagent %f (want 0), connectivity %f (want -1 for no prompt)", subRow.CacheHitRatio(), rep.ByClass[2].CacheHitRatio())
	}

	if len(rep.ByModel) != 1 || rep.ByModel[0].Key != "claude-opus-5-5" || rep.ByModel[0].Requests != 3 {
		t.Errorf("by model = %+v, want one row covering the three requests with usage", rep.ByModel)
	}
	if len(rep.ByBackend) != 1 || rep.ByBackend[0].Key != "upstream" || rep.ByBackend[0].Requests != 5 {
		t.Errorf("by backend = %+v", rep.ByBackend)
	}
}

func TestSummariseCountsWhatItCannotPrice(t *testing.T) {
	priced := rec("a", at(10, 1), "main", &Usage{Output: 1000})
	mystery := rec("b", at(10, 2), "main", &Usage{Output: 1000})
	mystery.Response.Model = "some-new-model"
	fast := rec("c", at(10, 3), "main", &Usage{Output: 1000, Speed: "fast"})

	rep := Summarise([]Record{priced, mystery, fast}, DefaultPrices)

	if !near(rep.Total.CostUSD, 0.02) || rep.Total.Unpriced != 2 || rep.Total.Output != 3000 {
		t.Errorf("total = %+v; want the tokens of all three but the cost of one, with two unpriced", rep.Total)
	}
	if rep.Unpriced["no price for some-new-model"] != 1 || rep.Unpriced["fast mode is priced separately"] != 1 {
		t.Errorf("unpriced reasons = %v", rep.Unpriced)
	}
}

func TestLocalRequestsCostNothingUpstream(t *testing.T) {
	local := rec("l", at(10, 1), "subagent", &Usage{Input: 5000, Output: 1000})
	local.Backend, local.Route = "local", "local"
	local.Response.Model = "qwen/qwen3.8-27b"

	rep := Summarise([]Record{local}, DefaultPrices)
	if rep.Total.CostUSD != 0 || rep.Total.Unpriced != 0 || len(rep.Unpriced) != 0 {
		t.Errorf("total = %+v, unpriced = %v; a local answer is free, not unknown", rep.Total, rep.Unpriced)
	}
	if rep.ByBackend[0].Key != "local" || rep.ByBackend[0].Output != 1000 {
		t.Errorf("by backend = %+v", rep.ByBackend)
	}
}

func TestSummariseOfNothing(t *testing.T) {
	rep := Summarise(nil, DefaultPrices)
	if rep.Total.Requests != 0 || !rep.From.IsZero() || len(rep.ByClass) != 0 || rep.PricesChecked != PricesChecked {
		t.Errorf("report = %+v", rep)
	}
}

func TestSummariseCountsUnreadResponses(t *testing.T) {
	read := rec("a", at(10, 1), "main", &Usage{Output: 10})
	unread := rec("b", at(10, 2), "main", nil)
	unread.Unread = "content-encoding br"

	if rep := Summarise([]Record{read, unread}, DefaultPrices); rep.Unread != 1 {
		t.Errorf("Unread = %d, want 1", rep.Unread)
	}
}
