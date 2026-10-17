package ledger

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/risadams/janus/internal/proxy"
)

// Newest is the time of the latest record in dir, or zero when there is
// none. It reads only the newest day's file.
func Newest(dir string) (time.Time, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, nil
		}
		return time.Time{}, fmt.Errorf("ledger: %w", err)
	}
	// ReadDir sorts by name, and the names are dates, so search from the end.
	for i := len(entries) - 1; i >= 0; i-- {
		name := entries[i].Name()
		day, err := time.Parse("2006-01-02", strings.TrimSuffix(name, ".jsonl"))
		if err != nil || entries[i].IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		records, _, err := Read(dir, day, day.Add(24*time.Hour))
		if err != nil {
			return time.Time{}, err
		}
		if len(records) > 0 {
			return records[len(records)-1].Time, nil
		}
	}
	return time.Time{}, nil
}

// Skipped counts the ledger lines Read left out.
type Skipped struct {
	// Malformed lines are not records at all.
	Malformed int
	// Newer records were written by a newer janus, in a schema this one
	// does not know: one where a field may mean something else. Reading
	// them as if they were this schema could give wrong figures, so they
	// are left out and counted instead (#269).
	Newer int
}

// Read returns the records whose time falls in [since, until), oldest
// first. A zero since or until leaves that end open. Lines it cannot read
// are skipped and counted: one bad line must not hide a day.
//
// Every release so far has written schema 1, adding fields as it went, and
// testdata holds a ledger in the shape of each; a later schema that
// changes a field's meaning brings the records of earlier ones up to date
// here, as it reads them.
func Read(dir string, since, until time.Time) (records []Record, skipped Skipped, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, skipped, nil
		}
		return nil, skipped, fmt.Errorf("ledger: %w", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		day, err := time.Parse("2006-01-02", strings.TrimSuffix(name, ".jsonl"))
		if err != nil || entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		// Skip whole files that cannot hold anything in range.
		if (!until.IsZero() && !day.Before(until)) || (!since.IsZero() && day.Add(24*time.Hour).Before(since)) {
			continue
		}

		f, err := os.Open(filepath.Join(dir, name)) // #nosec G304 -- a day file listed from the ledger directory
		if err != nil {
			return nil, skipped, fmt.Errorf("ledger: %w", err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			// The schema version is read first: a newer record's fields
			// may not even fit this one's types.
			var head struct {
				V int `json:"v"`
			}
			if json.Unmarshal(sc.Bytes(), &head) == nil && head.V > SchemaVersion {
				skipped.Newer++
				continue
			}
			var rec Record
			if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.ID == "" {
				skipped.Malformed++
				continue
			}
			if (!since.IsZero() && rec.Time.Before(since)) || (!until.IsZero() && !rec.Time.Before(until)) {
				continue
			}
			records = append(records, rec)
		}
		err = sc.Err()
		_ = f.Close() // read only: closing cannot lose anything
		if err != nil {
			return nil, skipped, fmt.Errorf("ledger: %s: %w", name, err)
		}
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].Time.Before(records[j].Time) })
	return records, skipped, nil
}

// Row is the totals for one group of requests.
type Row struct {
	Key      string `json:"key"`
	Requests int    `json:"requests"`
	// Input is uncached input; CacheWrite and CacheRead are the rest of
	// the prompt, so the three together are everything the model was sent.
	Input      int `json:"input"`
	CacheWrite int `json:"cache_write"`
	CacheRead  int `json:"cache_read"`
	Output     int `json:"output"`
	// CostUSD covers only the requests that could be priced; Unpriced is
	// how many with usage could not, and are therefore missing from it.
	CostUSD  float64 `json:"cost_usd"`
	Unpriced int     `json:"unpriced"`
}

// CacheHitRatio is the share of the prompt that was read from cache, from
// 0 to 1, or -1 when there was no prompt at all.
func (r Row) CacheHitRatio() float64 {
	prompt := r.Input + r.CacheWrite + r.CacheRead
	if prompt == 0 {
		return -1
	}
	return float64(r.CacheRead) / float64(prompt)
}

func (r *Row) add(rec Record, prices Prices) (unpriced string) {
	r.Requests++
	u := rec.Usage
	if u == nil {
		return ""
	}
	r.Input += u.Input
	r.Output += u.Output
	r.CacheWrite += deref(u.CacheWrite)
	r.CacheRead += deref(u.CacheRead)

	// Requests answered locally cost nothing upstream, whatever the model.
	if rec.Backend == "local" {
		return ""
	}
	usd, unpriced := prices.Cost(model(rec), *u)
	if unpriced != "" {
		r.Unpriced++
		return unpriced
	}
	r.CostUSD += usd
	return ""
}

// model is the model that answered, falling back to the one asked for.
func model(rec Record) string {
	if rec.Response != nil && rec.Response.Model != "" {
		return rec.Response.Model
	}
	if rec.Request != nil {
		return rec.Request.Model
	}
	return ""
}

// Report is what janus stats shows.
type Report struct {
	// From and To are the times of the first and last record included.
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Total     Row       `json:"total"`
	ByClass   []Row     `json:"by_class"`
	ByModel   []Row     `json:"by_model"`
	ByBackend []Row     `json:"by_backend"`
	// Outcomes counts requests by outcome.
	Outcomes map[string]int `json:"outcomes"`
	// Guard totals what the search for secrets found in requests that
	// went upstream.
	Guard GuardTotals `json:"guard,omitzero"`
	// LocalOnly totals what the local-only policy did.
	LocalOnly LocalOnlyTotals `json:"local_only,omitzero"`
	// Delegated summarises, by tool, work Claude handed to the local model.
	// It is in no other figure.
	Delegated []DelegatedRow `json:"delegated,omitempty"`
	// StoodIn counts requests the local model answered because Claude
	// could not: unreachable, rate limited, overloaded, or failing.
	StoodIn int `json:"stood_in,omitempty"`
	// LocalSaving is what the requests the local model answered by choice
	// would have cost had Claude answered them (#227).
	LocalSaving LocalSaving `json:"local_saving,omitzero"`
	// ByDay is the range broken down by local date, when asked for.
	ByDay []DayRow `json:"by_day,omitempty"`
	// LocalFirst summarises, by class, requests where the local model was
	// tried with Claude as the fallback.
	LocalFirst []LocalFirstRow `json:"local_first,omitempty"`
	// LocalFirstCause counts, by cause, why the local model did not
	// answer: attempts that ended that way, and requests a rule or
	// stand-in passed over without trying it (#202).
	LocalFirstCause []CauseRow `json:"local_first_cause,omitempty"`
	// Shadow summarises shadow runs by class. They are in no other figure.
	Shadow []ShadowRow `json:"shadow,omitempty"`
	// Transforms is what each transform saved, set beside what the guard
	// expected when it decided.
	Transforms []TransformRow `json:"transforms,omitempty"`
	// Quota is where the subscription's limits stood, and which kinds of
	// request used them.
	Quota *QuotaSummary `json:"quota,omitempty"`
	// Handbacks counts the times Claude took a conversation back after the
	// local model stood in for it, and HandedBackTurns the stand-in turns
	// before them.
	Handbacks       int `json:"handbacks,omitempty"`
	HandedBackTurns int `json:"handed_back_turns,omitempty"`
	// Outages are the runs of requests Claude could not answer, and how
	// many of them the local model answered instead.
	Outages []Outage `json:"outages,omitempty"`
	// Unread counts Messages responses janus relayed but could not read.
	// Their tokens are missing from every figure in the report.
	Unread int `json:"unread,omitempty"`
	// Unpriced counts, by reason, the requests left out of every cost.
	Unpriced  map[string]int `json:"unpriced,omitempty"`
	Malformed int            `json:"malformed_lines,omitempty"`
	// Newer counts records written by a newer janus, in a schema this one
	// does not know, which were left out.
	Newer         int    `json:"newer_records,omitempty"`
	PricesChecked string `json:"prices_checked"`
}

// Summarise totals the records, overall and by class, model, and backend.
// By-model rows cover only requests that reported usage, since a request
// with no response has no model to file it under.
func Summarise(records []Record, prices Prices) Report {
	rep := Report{
		Total:         Row{Key: "total"},
		Outcomes:      map[string]int{},
		Unpriced:      map[string]int{},
		PricesChecked: PricesChecked,
	}
	byClass, byModel, byBackend := map[string]*Row{}, map[string]*Row{}, map[string]*Row{}
	group := func(rows map[string]*Row, key string) *Row {
		if rows[key] == nil {
			rows[key] = &Row{Key: key}
		}
		return rows[key]
	}

	shadows := map[string]*shadowTally{}
	attempts := map[string]*attemptTally{}
	causes := map[string]*CauseRow{}
	delegated := map[string]*delegatedTally{}
	transforms := map[string]*TransformRow{}
	first := true
	for _, rec := range records {
		if rec.Worker != nil {
			// Delegated work is real activity, so it counts toward the
			// period the report covers, though not toward the requests.
			if first {
				rep.From, first = rec.Time, false
			}
			rep.To = rec.Time
			if delegated[rec.Worker.Tool] == nil {
				delegated[rec.Worker.Tool] = &delegatedTally{}
			}
			delegated[rec.Worker.Tool].add(rec)
			continue
		}
		if rec.ShadowOf != "" {
			if shadows[rec.Class] == nil {
				shadows[rec.Class] = &shadowTally{}
			}
			shadows[rec.Class].add(rec)
			continue
		}
		if first {
			rep.From, first = rec.Time, false
		}
		rep.To = rec.Time
		standIn := rec.Attempt != nil && rec.Attempt.Backend == "upstream"
		if standIn {
			rep.StoodIn++
		} else if rec.Attempt != nil {
			if attempts[rec.Class] == nil {
				attempts[rec.Class] = &attemptTally{}
			}
			attempts[rec.Class].add(*rec.Attempt)
		}
		if c := firstCause(rec); c != "" {
			row, ok := causes[c]
			if !ok {
				row = &CauseRow{Cause: c}
				causes[c] = row
			}
			if rec.Attempt != nil {
				row.Attempts++
			} else {
				row.NotTried++
			}
		}
		if g := rec.Guard; g != nil && g.Blocked != "" {
			rep.Guard.Blocked++
		}
		if g := rec.Guard; g != nil && len(g.LocalOnly) > 0 {
			rep.LocalOnly.add(rec)
		}
		if g := rec.Guard; g != nil && g.Findings > 0 {
			rep.Guard.Requests++
			if g.Redacted != nil {
				rep.Guard.Redacted++
			}
			rep.Guard.Restored += g.Restored
			rep.Guard.Findings += g.Findings
			if rep.Guard.Kinds == nil {
				rep.Guard.Kinds = map[string]int{}
			}
			for kind, n := range g.Kinds {
				rep.Guard.Kinds[kind] += n
			}
		}
		for _, t := range rec.Transforms {
			row := transforms[t.Name]
			if row == nil {
				row = &TransformRow{Name: t.Name}
				transforms[t.Name] = row
			}
			row.add(t)
		}
		if h := rec.Handback; h != nil {
			rep.Handbacks++
			rep.HandedBackTurns += h.LocalTurns
		}
		rep.Outcomes[rec.Outcome]++
		if rec.Unread != "" {
			rep.Unread++
		}

		if reason := rep.Total.add(rec, prices); reason != "" {
			rep.Unpriced[reason]++
		}
		if rec.Backend == "local" && !standIn {
			rep.LocalSaving.add(rec, prices)
		}
		group(byClass, rec.Class).add(rec, prices)
		group(byBackend, rec.Backend).add(rec, prices)
		if rec.Usage != nil {
			group(byModel, model(rec)).add(rec, prices)
		}
	}

	rep.ByClass, rep.ByModel, rep.ByBackend = sorted(byClass), sorted(byModel), sorted(byBackend)
	rep.Outages = Outages(records)
	rep.Quota = summariseQuota(records)
	for class, tally := range shadows {
		rep.Shadow = append(rep.Shadow, tally.row(class))
	}
	sort.Slice(rep.Shadow, func(i, j int) bool { return rep.Shadow[i].Class < rep.Shadow[j].Class })
	for class, tally := range attempts {
		row := tally.row
		row.Class, row.MedianMs = class, medianOf(tally.ms)
		rep.LocalFirst = append(rep.LocalFirst, row)
	}
	sort.Slice(rep.LocalFirst, func(i, j int) bool { return rep.LocalFirst[i].Class < rep.LocalFirst[j].Class })
	for _, row := range causes {
		rep.LocalFirstCause = append(rep.LocalFirstCause, *row)
	}
	sort.Slice(rep.LocalFirstCause, func(i, j int) bool { return rep.LocalFirstCause[i].Cause < rep.LocalFirstCause[j].Cause })
	for tool, tally := range delegated {
		row := tally.row
		row.Tool, row.MedianMs = tool, medianOf(tally.ms)
		rep.Delegated = append(rep.Delegated, row)
	}
	sort.Slice(rep.Delegated, func(i, j int) bool { return rep.Delegated[i].Tool < rep.Delegated[j].Tool })
	for _, row := range transforms {
		rep.Transforms = append(rep.Transforms, *row)
	}
	sort.Slice(rep.Transforms, func(i, j int) bool { return rep.Transforms[i].Name < rep.Transforms[j].Name })
	return rep
}

// DayRow is one day's headline figures, so that a week of use can be read
// day by day and a day that went worse than the rest stands out.
type DayRow struct {
	// Day is the local date, as 2006-01-02.
	Day      string `json:"day"`
	Requests int    `json:"requests"`
	// Failed counts requests that did not end well, other than those the
	// client cancelled: errors, broken-off answers, and no answer at all.
	Failed   int     `json:"failed"`
	CostUSD  float64 `json:"cost_usd"`
	Unpriced int     `json:"unpriced"`
	// CacheHit is the share of the prompt read from cache, or -1 when
	// nothing was sent.
	CacheHit float64 `json:"cache_hit"`
	// LocalTried and LocalAccepted are local-first attempts and those
	// whose answer was used.
	LocalTried    int     `json:"local_tried"`
	LocalAccepted int     `json:"local_accepted"`
	LocalSavedUSD float64 `json:"local_saved_usd"`
	StoodIn       int     `json:"stood_in"`
}

// ByDay summarises the records one day at a time, by their date in loc,
// oldest first. The records must be in time order, as Read returns them.
// Days with no requests are left out.
func ByDay(records []Record, prices Prices, loc *time.Location) []DayRow {
	var days []DayRow
	start := 0
	for i := range records {
		day := records[i].Time.In(loc).Format(time.DateOnly)
		if i+1 < len(records) && records[i+1].Time.In(loc).Format(time.DateOnly) == day {
			continue
		}
		rep := Summarise(records[start:i+1], prices)
		start = i + 1
		if rep.Total.Requests == 0 {
			continue
		}
		row := DayRow{Day: day, Requests: rep.Total.Requests, CostUSD: rep.Total.CostUSD,
			Unpriced: rep.Total.Unpriced, CacheHit: rep.Total.CacheHitRatio(),
			LocalSavedUSD: rep.LocalSaving.USD, StoodIn: rep.StoodIn}
		for outcome, n := range rep.Outcomes {
			if outcome != OutcomeOK && outcome != OutcomeCancelled {
				row.Failed += n
			}
		}
		for _, lf := range rep.LocalFirst {
			row.LocalTried += lf.Tried
			row.LocalAccepted += lf.Accepted
		}
		days = append(days, row)
	}
	return days
}

// LocalSaving is what the requests the local model answered by choice, by
// a rule or an accepted local-first attempt, would have cost at the list
// price of the Claude model each asked for. Stand-ins are left out: Claude
// could not answer those, so nothing was saved by choosing local.
//
// It is an estimate. The tokens are the local model's count, not Claude's,
// and Claude would have cached a different share of each prompt, so it
// says how large the saving was, not what it was to the cent.
type LocalSaving struct {
	Requests int     `json:"requests"`
	USD      float64 `json:"usd"`
	// Unpriced counts local answers left out of USD: no usage was read,
	// or the model asked for has no price, as with a local alias.
	Unpriced int `json:"unpriced"`
}

func (s *LocalSaving) add(rec Record, prices Prices) {
	s.Requests++
	asked := ""
	if rec.Request != nil {
		asked = rec.Request.Model
	}
	if rec.Usage == nil {
		s.Unpriced++
		return
	}
	usd, unpriced := prices.Cost(asked, *rec.Usage)
	if unpriced != "" {
		s.Unpriced++
		return
	}
	s.USD += usd
}

// TransformRow is one transform's totals over the requests it touched.
//
// ExpectedUSD is what the guard expected its decisions to save, over the
// conversations' remaining turns, net of the cost of revising. SavedUSD
// less CostUSD is what was saved in fact on the requests in the range.
// The two are measured differently, so they converge only over whole
// conversations: a decision is expected all at once, on the request that
// makes it, and saves a little on each request after.
type TransformRow struct {
	Name        string  `json:"name"`
	Requests    int     `json:"requests"`
	Rewritten   int     `json:"rewritten"`
	Revised     int     `json:"revised"`
	Declined    int     `json:"declined"`
	Tokens      int     `json:"tokens"`
	SavedUSD    float64 `json:"saved_usd"`
	CostUSD     float64 `json:"cost_usd"`
	ExpectedUSD float64 `json:"expected_usd"`
	// Unpriced counts requests whose model had no price, which are in no
	// dollar figure.
	Unpriced int `json:"unpriced"`
}

func (r *TransformRow) add(t Transform) {
	r.Requests++
	r.Rewritten += t.Rewritten
	r.Revised += t.Revised
	r.Declined += t.Declined
	r.Tokens += t.Tokens
	r.SavedUSD += t.SavedUSD
	r.CostUSD += t.CostUSD
	r.ExpectedUSD += t.ExpectedUSD
	if t.Unpriced {
		r.Unpriced++
	}
}

// sorted orders rows by cost, then by size, so the expensive groups lead.
func sorted(rows map[string]*Row) []Row {
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.CostUSD != b.CostUSD {
			return a.CostUSD > b.CostUSD
		}
		if a.Requests != b.Requests {
			return a.Requests > b.Requests
		}
		return a.Key < b.Key
	})
	return out
}

// ShadowRow is how the local model did on one class of request, judged
// against the real answers.
type ShadowRow struct {
	Class string `json:"class"`
	// Runs is every shadow asked for. Of those, Skipped were never sent
	// (too large, queue full), Failed were sent and came back unusable
	// (an error, cut off, or empty), and Compared came back whole.
	Runs     int `json:"runs"`
	Skipped  int `json:"skipped"`
	Failed   int `json:"failed"`
	Compared int `json:"compared"`
	// Of the compared runs: how many stopped for the same reason as the
	// real answer, how many called the same tools, and how many did both.
	StopMatch  int `json:"stop_match"`
	ToolsMatch int `json:"tools_match"`
	BothMatch  int `json:"both_match"`
	// Median durations of the compared runs and of the real answers they
	// were compared with.
	LocalMedianMs    int64 `json:"local_median_ms"`
	UpstreamMedianMs int64 `json:"upstream_median_ms"`
}

type shadowTally struct {
	counts              ShadowRow
	localMs, upstreamMs []int64
}

func (t *shadowTally) add(rec Record) {
	t.counts.Runs++
	switch {
	case rec.Outcome == OutcomeSkipped:
		t.counts.Skipped++
		return
	case rec.Outcome != OutcomeOK || rec.Compare == nil || rec.Response == nil || len(rec.Response.Blocks) == 0:
		// A well-formed answer with nothing in it is a failure too: it is
		// the "successful but empty" response small models produce.
		t.counts.Failed++
		return
	}
	t.counts.Compared++
	if rec.Compare.StopMatch {
		t.counts.StopMatch++
	}
	if rec.Compare.ToolsMatch {
		t.counts.ToolsMatch++
	}
	if rec.Compare.StopMatch && rec.Compare.ToolsMatch {
		t.counts.BothMatch++
	}
	t.localMs = append(t.localMs, rec.DurationMs)
	t.upstreamMs = append(t.upstreamMs, rec.Compare.UpstreamMs)
}

func (t *shadowTally) row(class string) ShadowRow {
	r := t.counts
	r.Class = class
	r.LocalMedianMs, r.UpstreamMedianMs = medianOf(t.localMs), medianOf(t.upstreamMs)
	return r
}

func medianOf(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

// LocalFirstRow is how trying the local model first went for one class.
type LocalFirstRow struct {
	Class string `json:"class"`
	// Tried is every request the local model was tried for. Accepted were
	// answered by it; Rejected failed a check and Failed found it
	// unavailable, and both of those were answered by Claude instead.
	Tried    int `json:"tried"`
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
	Failed   int `json:"failed"`
	// MedianMs is the median time the local attempts took, whatever became
	// of them. For a rejected or failed attempt that time was spent before
	// Claude was even asked.
	MedianMs int64 `json:"median_ms"`
}

type attemptTally struct {
	row LocalFirstRow
	ms  []int64
}

func (t *attemptTally) add(a Attempt) {
	t.row.Tried++
	switch {
	case a.Verdict == VerdictAccepted:
		t.row.Accepted++
	case a.Verdict == VerdictRejected && !failedOnTheWire(a):
		t.row.Rejected++
	default:
		t.row.Failed++
	}
	t.ms = append(t.ms, a.DurationMs)
}

// failedOnTheWire reports whether an attempt recorded as rejected, by a
// Janus older than causes (#202), in fact never produced an answer to
// judge: an error from LM Studio, or an answer cut short. Those are
// failures, and are counted as such.
func failedOnTheWire(a Attempt) bool {
	c := AttemptCause(a)
	return c != "" && c != proxy.CauseRejected
}

// DelegatedRow is the delegated work done through one tool.
type DelegatedRow struct {
	Tool   string `json:"tool"`
	Calls  int    `json:"calls"`
	Failed int    `json:"failed"`
	// Files and ReadBytes are what the local model read in the calls that
	// succeeded, and AvoidedTokens the estimate of what that spared Claude.
	Files         int   `json:"files"`
	ReadBytes     int   `json:"read_bytes"`
	AvoidedTokens int   `json:"avoided_tokens"`
	MedianMs      int64 `json:"median_ms"`
}

type delegatedTally struct {
	row DelegatedRow
	ms  []int64
}

func (t *delegatedTally) add(rec Record) {
	t.row.Calls++
	if rec.Outcome != OutcomeOK {
		t.row.Failed++
		return
	}
	t.row.Files += rec.Worker.Files
	t.row.ReadBytes += rec.Worker.ReadBytes
	t.row.AvoidedTokens += rec.Worker.AvoidedTokens
	t.ms = append(t.ms, rec.DurationMs)
}

// LocalOnlyTotals is what the local-only policy did with the requests
// that held content it protects. Like a secret, such content stays in its
// conversation and is counted again on every turn.
type LocalOnlyTotals struct {
	Requests int `json:"requests"`
	// Withheld were sent upstream with the content replaced by a notice;
	// Pieces is how many pieces that was in all. KeptLocal were answered
	// by the local model instead, and Refused were not sent anywhere.
	Withheld  int `json:"withheld"`
	Pieces    int `json:"pieces"`
	KeptLocal int `json:"kept_local"`
	Refused   int `json:"refused"`
	// Reasons counts why, by the policy's own words, which name paths and
	// markers and never content.
	Reasons map[string]int `json:"reasons,omitempty"`
}

func (t *LocalOnlyTotals) add(rec Record) {
	g := rec.Guard
	t.Requests++
	switch {
	case rec.Backend == "local":
		t.KeptLocal++
	case rec.Backend == "none":
		t.Refused++
	default:
		t.Withheld++
		t.Pieces += g.Withheld
	}
	if t.Reasons == nil {
		t.Reasons = map[string]int{}
	}
	for _, reason := range g.LocalOnly {
		t.Reasons[reason]++
	}
}

// GuardTotals is how often secrets were seen going upstream. A secret that
// sits in a conversation is sent again with every turn, and is counted
// each time: Findings is occurrences across requests, not distinct
// secrets.
type GuardTotals struct {
	Requests int            `json:"requests"`
	Findings int            `json:"findings"`
	Kinds    map[string]int `json:"kinds,omitempty"`
	// Redacted is how many of those requests had their secrets replaced
	// before they were sent; the rest went upstream with secrets in them.
	Redacted int `json:"redacted"`
	// Restored counts placeholders put back in answers, and Blocked the
	// exchanges that were stopped.
	Restored int `json:"restored"`
	Blocked  int `json:"blocked"`
}
