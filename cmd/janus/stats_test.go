package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/ledger"
)

func p(i int) *int { return &i }

// seedLedger writes a small ledger: a connectivity check, two main turns,
// one subagent turn, and one request on a model with no price.
func seedLedger(t *testing.T) (dir string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "ledger")
	l := ledger.Open(dir)
	when := time.Date(2026, 10, 10, 15, 0, 0, 0, time.UTC)
	add := func(id, class, model string, u *ledger.Usage) {
		rec := ledger.Record{V: 1, ID: id, Time: when, Class: class, Route: "upstream", Backend: "upstream",
			Status: 200, Outcome: ledger.OutcomeOK, Usage: u}
		if u != nil {
			rec.Response = &ledger.Response{Model: model}
		}
		if err := l.Append(rec); err != nil {
			t.Fatal(err)
		}
		when = when.Add(time.Minute)
	}
	add("h", "connectivity", "", nil)
	add("m1", "main", "claude-opus-5-5", &ledger.Usage{Input: 2, Output: 100, CacheRead: p(9000), CacheWrite: p(998), CacheWrite5m: p(0), CacheWrite1h: p(998)})
	add("m2", "main", "claude-opus-5-5", &ledger.Usage{Input: 2, Output: 50, CacheRead: p(9998), CacheWrite: p(0)})
	add("s1", "subagent", "claude-opus-5-5", &ledger.Usage{Input: 2, Output: 400, CacheRead: p(0), CacheWrite: p(1234567), CacheWrite5m: p(1234567), CacheWrite1h: p(0)})
	add("x1", "main", "some-new-model", &ledger.Usage{Input: 1, Output: 1})
	return dir
}

func stats(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	// Point -config somewhere empty so the test never reads a real config.
	args = append([]string{"stats", "-config", filepath.Join(t.TempDir(), "none.json")}, args...)
	code = run(args, &out, &errOut)
	return out.String(), errOut.String(), code
}

func TestStatsReport(t *testing.T) {
	dir := seedLedger(t)
	out, errOut, code := stats(t, "-dir", dir)
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}

	// Rows are checked with their spacing squeezed out, so the test does
	// not depend on column widths.
	squeezed := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"5 requests",
		"By class class requests input cache write cache read output cache hit est. cost",
		// 2x4 + 400x20 + 1,234,567x5 = 6,180,843 millionths
		"subagent 1 2 1,234,567 0 400 0.0% $6.1808",
		// 4x4 + 150x20 + 18,998x0.2 + 998x8, plus one request with no price
		"main 3 5 998 18,998 151 95.0% $0.0148*",
		"connectivity 1 0 0 0 0 - $0.0000",
		"total 5 7 1,235,565 18,998 551 1.5% $6.1956*",
		"By model",
		"claude-opus-5-5 3 6 1,235,565 18,998 550",
		"some-new-model 1 1 0 0 1 0.0% $0.0000*",
		"By backend",
		"upstream 5",
		"Outcomes: ok 5",
		"list prices as of " + ledger.PricesChecked,
		"1 requests could not be priced and are in no cost above: 1: no price for some-new-model",
	} {
		if !strings.Contains(squeezed, want) {
			t.Errorf("report is missing %q", want)
		}
	}
	if t.Failed() {
		t.Logf("report was:\n%s", out)
	}
}

func TestStatsJSON(t *testing.T) {
	out, _, code := stats(t, "-dir", seedLedger(t), "-json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var rep ledger.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("output is not a report: %v\n%s", err, out)
	}
	if rep.Total.Requests != 5 || rep.Total.Unpriced != 1 || len(rep.ByClass) != 3 || rep.ByClass[0].Key != "subagent" {
		t.Errorf("report = %+v", rep)
	}
}

func TestStatsTimeRange(t *testing.T) {
	dir := seedLedger(t) // records at 15:00 to 15:04 UTC on 2026-10-10
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-since", "2026-10-10T15:03:00Z"}, "2 requests"},
		{[]string{"-until", "2026-10-10T15:01:00Z"}, "1 requests"},
		{[]string{"-since", "2026-10-10T15:01:00Z", "-until", "2026-10-10T15:03:00Z"}, "2 requests"},
		{[]string{"-since", "2030-01-01"}, "No requests recorded in this range."},
		{[]string{"-since", "1h"}, "No requests recorded in this range."},
	} {
		out, _, code := stats(t, append([]string{"-dir", dir}, c.args...)...)
		if code != 0 || !strings.Contains(out, c.want) {
			t.Errorf("stats %v: exit %d, want %q in:\n%s", c.args, code, c.want, out)
		}
	}
}

func TestStatsUsesConfigForDirectoryAndPrices(t *testing.T) {
	root := t.TempDir()
	logs := filepath.Join(root, "logs")
	l := ledger.Open(filepath.Join(logs, "ledger"))
	l.Append(ledger.Record{V: 1, ID: "a", Time: time.Now(), Class: "main", Backend: "upstream", Outcome: ledger.OutcomeOK,
		Response: &ledger.Response{Model: "my-gateway-model"}, Usage: &ledger.Usage{Output: 1_000_000}})
	cfgPath := filepath.Join(root, "janus.config.json")
	cfg, _ := json.Marshal(map[string]any{
		"logging": map[string]any{"dir": logs},
		"pricing": map[string]any{"my-gateway-model": map[string]any{"input": 1, "output": 7.5}},
	})
	os.WriteFile(cfgPath, cfg, 0o600)

	var out, errOut bytes.Buffer
	if code := run([]string{"stats", "-config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "$7.5000") || strings.Contains(out.String(), "could not be priced") {
		t.Errorf("the configured price was not used:\n%s", out.String())
	}
}

func TestStatsDoesNotCreateAConfigOrALedger(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "janus.config.json")
	var out, errOut bytes.Buffer
	code := run([]string{"stats", "-config", cfgPath, "-dir", filepath.Join(root, "ledger")}, &out, &errOut)

	if code != 0 || !strings.Contains(out.String(), "No requests recorded") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("stats created files: %v", entries)
	}
}

func TestStatsBadArguments(t *testing.T) {
	for _, args := range [][]string{
		{"-since", "yesterday"},
		{"-until", "-5h"},
		{"extra"},
		{"-no-such-flag"},
	} {
		_, errOut, code := stats(t, args...)
		if code != 2 || errOut == "" {
			t.Errorf("stats %v: exit %d, stderr %q; want exit 2 and an explanation", args, code, errOut)
		}
	}
}

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"":                     {},
		"24h":                  now.Add(-24 * time.Hour),
		"7d":                   now.Add(-7 * 24 * time.Hour),
		"1.5d":                 now.Add(-36 * time.Hour),
		"90m":                  now.Add(-90 * time.Minute),
		"2026-10-01T09:00:00Z": time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
		"2026-10-01":           time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local),
	} {
		got, err := parseWhen(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseWhen(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

func TestCommas(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -1234: "-1,234", 100000: "100,000"} {
		if got := commas(n); got != want {
			t.Errorf("commas(%d) = %q, want %q", n, got, want)
		}
	}
}
