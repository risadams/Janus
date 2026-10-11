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
)

// Read returns the records whose time falls in [since, until), oldest
// first. A zero since or until leaves that end open. Lines that are not
// records are skipped and counted: one bad line must not hide a day.
func Read(dir string, since, until time.Time) (records []Record, malformed int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("ledger: %w", err)
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

		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return nil, 0, fmt.Errorf("ledger: %w", err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			var rec Record
			if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.ID == "" {
				malformed++
				continue
			}
			if (!since.IsZero() && rec.Time.Before(since)) || (!until.IsZero() && !rec.Time.Before(until)) {
				continue
			}
			records = append(records, rec)
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("ledger: %s: %w", name, err)
		}
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].Time.Before(records[j].Time) })
	return records, malformed, nil
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
	// Delegated summarises, by tool, work Claude handed to the local model.
	// It is in no other figure.
	Delegated []DelegatedRow `json:"delegated,omitempty"`
	// StoodIn counts requests the local model answered because Claude
	// could not: unreachable, rate limited, overloaded, or failing.
	StoodIn int `json:"stood_in,omitempty"`
	// LocalFirst summarises, by class, requests where the local model was
	// tried with Claude as the fallback.
	LocalFirst []LocalFirstRow `json:"local_first,omitempty"`
	// Shadow summarises shadow runs by class. They are in no other figure.
	Shadow []ShadowRow `json:"shadow,omitempty"`
	// Unread counts Messages responses janus relayed but could not read.
	// Their tokens are missing from every figure in the report.
	Unread int `json:"unread,omitempty"`
	// Unpriced counts, by reason, the requests left out of every cost.
	Unpriced      map[string]int `json:"unpriced,omitempty"`
	Malformed     int            `json:"malformed_lines,omitempty"`
	PricesChecked string         `json:"prices_checked"`
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
	delegated := map[string]*delegatedTally{}
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
		if rec.Attempt != nil && rec.Attempt.Backend == "upstream" {
			rep.StoodIn++
		} else if rec.Attempt != nil {
			if attempts[rec.Class] == nil {
				attempts[rec.Class] = &attemptTally{}
			}
			attempts[rec.Class].add(*rec.Attempt)
		}
		if g := rec.Guard; g != nil && g.Findings > 0 {
			rep.Guard.Requests++
			rep.Guard.Findings += g.Findings
			if rep.Guard.Kinds == nil {
				rep.Guard.Kinds = map[string]int{}
			}
			for kind, n := range g.Kinds {
				rep.Guard.Kinds[kind] += n
			}
		}
		rep.Outcomes[rec.Outcome]++
		if rec.Unread != "" {
			rep.Unread++
		}

		if reason := rep.Total.add(rec, prices); reason != "" {
			rep.Unpriced[reason]++
		}
		group(byClass, rec.Class).add(rec, prices)
		group(byBackend, rec.Backend).add(rec, prices)
		if rec.Usage != nil {
			group(byModel, model(rec)).add(rec, prices)
		}
	}

	rep.ByClass, rep.ByModel, rep.ByBackend = sorted(byClass), sorted(byModel), sorted(byBackend)
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
	for tool, tally := range delegated {
		row := tally.row
		row.Tool, row.MedianMs = tool, medianOf(tally.ms)
		rep.Delegated = append(rep.Delegated, row)
	}
	sort.Slice(rep.Delegated, func(i, j int) bool { return rep.Delegated[i].Tool < rep.Delegated[j].Tool })
	return rep
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
	switch a.Verdict {
	case VerdictAccepted:
		t.row.Accepted++
	case VerdictRejected:
		t.row.Rejected++
	default:
		t.row.Failed++
	}
	t.ms = append(t.ms, a.DurationMs)
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

// GuardTotals is how often secrets were seen going upstream. A secret that
// sits in a conversation is sent again with every turn, and is counted
// each time: Findings is occurrences across requests, not distinct
// secrets.
type GuardTotals struct {
	Requests int            `json:"requests"`
	Findings int            `json:"findings"`
	Kinds    map[string]int `json:"kinds,omitempty"`
}
