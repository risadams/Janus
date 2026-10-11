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

	for i, rec := range records {
		if i == 0 {
			rep.From = rec.Time
		}
		rep.To = rec.Time
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
