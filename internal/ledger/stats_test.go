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

// shadowOf builds the shadow record of a real one.
func shadowOf(real Record, outcome string, stop, tools bool, localMs, upstreamMs int64) Record {
	s := real
	s.ID, s.ShadowOf = real.ID+"-shadow", real.ID
	s.Route, s.Backend, s.Outcome, s.DurationMs = "shadow", "local", outcome, localMs
	s.Usage = &Usage{Input: 5000, Output: 300}
	s.Response = &Response{Model: "qwen/qwen3.8-27b", Blocks: []string{"tool_use"}}
	if outcome == OutcomeOK {
		s.Compare = &Compare{StopMatch: stop, ToolsMatch: tools, UpstreamMs: upstreamMs}
	}
	return s
}

func TestShadowRunsAreSummarisedApartFromEverythingElse(t *testing.T) {
	u := &Usage{Input: 2, Output: 100, CacheRead: n(9000), CacheWrite: n(0)}
	sub1, sub2, sub3, sub4, sub5 := rec("s1", at(10, 1), "subagent", u), rec("s2", at(10, 2), "subagent", u),
		rec("s3", at(10, 3), "subagent", u), rec("s4", at(10, 4), "subagent", u), rec("s5", at(10, 5), "subagent", u)
	main1 := rec("m1", at(10, 6), "main", u)

	empty := shadowOf(sub4, OutcomeOK, true, true, 9000, 2000)
	empty.Response.Blocks = []string{} // "successful" and empty
	skipped := shadowOf(sub5, OutcomeSkipped, false, false, 0, 0)
	skipped.Usage, skipped.Response = nil, nil

	records := []Record{
		sub1, shadowOf(sub1, OutcomeOK, true, true, 12000, 2000),
		sub2, shadowOf(sub2, OutcomeOK, true, false, 20000, 3000),
		sub3, shadowOf(sub3, OutcomeIncomplete, false, false, 4000, 0),
		sub4, empty,
		sub5, skipped,
		main1, shadowOf(main1, OutcomeOK, false, false, 30000, 5000),
	}
	withShadows := Summarise(records, DefaultPrices)
	without := Summarise([]Record{sub1, sub2, sub3, sub4, sub5, main1}, DefaultPrices)

	// Shadows change no other figure: not the request count, the tokens,
	// the cost, the outcomes, or the time range.
	if withShadows.Total != without.Total || len(withShadows.ByBackend) != 1 || withShadows.Outcomes[OutcomeOK] != 6 || len(withShadows.Outcomes) != 1 {
		t.Errorf("shadow runs leaked into the totals:\n with    %+v %v\n without %+v", withShadows.Total, withShadows.Outcomes, without.Total)
	}
	if !withShadows.To.Equal(at(10, 6)) {
		t.Errorf("To = %v", withShadows.To)
	}
	if len(without.Shadow) != 0 {
		t.Errorf("a report with no shadows has a shadow section: %+v", without.Shadow)
	}

	if len(withShadows.Shadow) != 2 || withShadows.Shadow[0].Class != "main" || withShadows.Shadow[1].Class != "subagent" {
		t.Fatalf("shadow rows = %+v", withShadows.Shadow)
	}
	want := ShadowRow{Class: "subagent", Runs: 5, Skipped: 1, Failed: 2, Compared: 2, StopMatch: 2, ToolsMatch: 1, BothMatch: 1,
		LocalMedianMs: 20000, UpstreamMedianMs: 3000}
	if got := withShadows.Shadow[1]; got != want {
		t.Errorf("subagent shadow row:\n got %+v\nwant %+v", got, want)
	}
	if got := withShadows.Shadow[0]; got.Compared != 1 || got.BothMatch != 0 || got.LocalMedianMs != 30000 {
		t.Errorf("main shadow row = %+v", got)
	}
}

func TestLocalFirstSummary(t *testing.T) {
	u := &Usage{Input: 2, Output: 100}
	with := func(id, class, verdict string, ms int64) Record {
		r := rec(id, at(10, 1), class, u)
		r.Attempt = &Attempt{Model: "qwen/qwen3.8-27b", Verdict: verdict, DurationMs: ms}
		if verdict == VerdictAccepted {
			r.Backend, r.Response.Model = "local", "qwen/qwen3.8-27b"
		}
		return r
	}
	rep := Summarise([]Record{
		with("a", "subagent", VerdictAccepted, 9000),
		with("b", "subagent", VerdictAccepted, 12000),
		with("c", "subagent", VerdictRejected, 30000),
		with("d", "subagent", VerdictFailed, 5),
		with("e", "main", VerdictRejected, 40000),
		rec("f", at(10, 2), "main", u), // never tried locally
	}, DefaultPrices)

	if len(rep.LocalFirst) != 2 || rep.LocalFirst[0].Class != "main" {
		t.Fatalf("rows = %+v", rep.LocalFirst)
	}
	want := LocalFirstRow{Class: "subagent", Tried: 4, Accepted: 2, Rejected: 1, Failed: 1, MedianMs: 12000}
	if rep.LocalFirst[1] != want {
		t.Errorf("subagent row = %+v, want %+v", rep.LocalFirst[1], want)
	}
	if rep.LocalFirst[0].Tried != 1 || rep.LocalFirst[0].Rejected != 1 {
		t.Errorf("main row = %+v", rep.LocalFirst[0])
	}
	// The requests themselves are counted as usual: accepted ones under
	// local at no cost, the rest under upstream.
	if rep.Total.Requests != 6 || len(rep.ByBackend) != 2 {
		t.Errorf("total = %+v, backends = %+v", rep.Total, rep.ByBackend)
	}
	if len(Summarise([]Record{rec("x", at(10, 1), "main", u)}, DefaultPrices).LocalFirst) != 0 {
		t.Error("a report with no attempts has a local-first section")
	}
}

func TestStoodInIsCountedApartFromLocalFirst(t *testing.T) {
	u := &Usage{Input: 100, Output: 30}
	stoodIn := rec("a", at(10, 1), "main", u)
	stoodIn.Backend, stoodIn.Route = "local", "local"
	stoodIn.Response.Model = "qwen/qwen3.8-27b"
	stoodIn.Attempt = &Attempt{Backend: "upstream", Verdict: VerdictFailed, Why: "upstream answered HTTP 529"}
	localFirst := rec("b", at(10, 2), "subagent", u)
	localFirst.Attempt = &Attempt{Backend: "local", Verdict: VerdictRejected}

	rep := Summarise([]Record{stoodIn, localFirst, rec("c", at(10, 3), "main", u)}, DefaultPrices)
	if rep.StoodIn != 1 {
		t.Errorf("StoodIn = %d, want 1", rep.StoodIn)
	}
	if len(rep.LocalFirst) != 1 || rep.LocalFirst[0].Class != "subagent" || rep.LocalFirst[0].Tried != 1 {
		t.Errorf("local-first rows = %+v; a stand-in is not a local-first attempt", rep.LocalFirst)
	}
}

func TestAvoidedTokensErrsLow(t *testing.T) {
	for _, c := range []struct{ read, answer, want int }{
		{84000, 900, 20700},
		{400, 300, 0},  // 100 - 100
		{100, 3000, 0}, // an answer longer than the file saved nothing
		{0, 0, 0},
	} {
		if got := AvoidedTokens(c.read, c.answer); got != c.want {
			t.Errorf("AvoidedTokens(%d, %d) = %d, want %d", c.read, c.answer, got, c.want)
		}
	}
}

func TestDelegatedWorkIsSummarisedApart(t *testing.T) {
	work := func(id string, outcome string, files, read, avoided int, ms int64) Record {
		return Record{V: 1, ID: id, Time: at(10, 1), Class: "worker", Backend: "local", Outcome: outcome, DurationMs: ms,
			Usage:  &Usage{Input: 20000, Output: 200},
			Worker: &Worker{Tool: "ask_files", Files: files, ReadBytes: read, AvoidedTokens: avoided}}
	}
	real := rec("m", at(10, 2), "main", &Usage{Input: 2, Output: 100})
	rep := Summarise([]Record{
		real,
		work("a", OutcomeOK, 3, 84000, 20700, 12000),
		work("b", OutcomeOK, 1, 4000, 900, 6000),
		work("c", OutcomeToolError, 5, 99000, 0, 400),
	}, DefaultPrices)

	if len(rep.Delegated) != 1 {
		t.Fatalf("delegated rows = %+v", rep.Delegated)
	}
	want := DelegatedRow{Tool: "ask_files", Calls: 3, Failed: 1, Files: 4, ReadBytes: 88000, AvoidedTokens: 21600, MedianMs: 12000}
	if rep.Delegated[0] != want {
		t.Errorf("row = %+v, want %+v", rep.Delegated[0], want)
	}
	// Delegated work is not a request: the totals are the one real request.
	if only := Summarise([]Record{real}, DefaultPrices); rep.Total != only.Total || len(rep.ByClass) != 1 {
		t.Errorf("delegated work leaked into the totals: %+v", rep.Total)
	}
}

func TestSummariseTotalsTransforms(t *testing.T) {
	first := rec("m1", at(10, 1), "main", nil)
	first.Transforms = []Transform{{Name: "digest", Blocks: 1, Rewritten: 1, Tokens: 1000, SavedUSD: 0.008, ExpectedUSD: 0.0084}}
	later := rec("m2", at(10, 2), "main", nil)
	later.Transforms = []Transform{
		{Name: "digest", Blocks: 1, Tokens: 1000, SavedUSD: 0.0002},
		{Name: "prune", Blocks: 1, Revised: 1, Declined: 2, Tokens: 5000, SavedUSD: 0.001, CostUSD: 0.0004, ExpectedUSD: 0.002},
	}
	unpriced := rec("m3", at(10, 3), "main", nil)
	unpriced.Transforms = []Transform{{Name: "digest", Blocks: 1, Rewritten: 1, Tokens: 300, Unpriced: true}}

	rep := Summarise([]Record{first, later, unpriced}, DefaultPrices)
	if len(rep.Transforms) != 2 {
		t.Fatalf("transforms = %+v, want digest and prune", rep.Transforms)
	}
	digest, prune := rep.Transforms[0], rep.Transforms[1]
	if digest.Name != "digest" || digest.Requests != 3 || digest.Rewritten != 2 || digest.Tokens != 2300 ||
		!near(digest.SavedUSD, 0.0082) || !near(digest.ExpectedUSD, 0.0084) || digest.Unpriced != 1 {
		t.Errorf("digest = %+v", digest)
	}
	if prune.Name != "prune" || prune.Revised != 1 || prune.Declined != 2 || !near(prune.CostUSD, 0.0004) {
		t.Errorf("prune = %+v", prune)
	}
}
