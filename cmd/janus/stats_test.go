package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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
		"logging": map[string]any{"dir": "logs"},
		"pricing": map[string]any{"my-gateway-model": map[string]any{"input": 1, "output": 7.5}},
	})
	os.WriteFile(cfgPath, cfg, 0o600)
	// Run from another folder: the relative logging.dir is the config
	// file's, so the ledger the proxy wrote is the one read.
	t.Chdir(t.TempDir())

	var out, errOut bytes.Buffer
	if code := run([]string{"stats", "-config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "$7.5000") || strings.Contains(out.String(), "could not be priced") {
		t.Errorf("the configured price was not used:\n%s", out.String())
	}
}

func TestStatsSaysWhenThereIsNoConfig(t *testing.T) {
	root := t.TempDir()
	var out, errOut bytes.Buffer
	run([]string{"stats", "-config", filepath.Join(root, "janus.config.json")}, &out, &errOut)
	if !strings.Contains(errOut.String(), "no config file at") || !strings.Contains(out.String(), "Ledger "+filepath.Join(root, "logs", "ledger")) {
		t.Errorf("stdout %q, stderr %q", out.String(), errOut.String())
	}

	// Given -dir, the config does not matter and nothing is said about it.
	out.Reset()
	errOut.Reset()
	run([]string{"stats", "-config", filepath.Join(root, "janus.config.json"), "-dir", root}, &out, &errOut)
	if errOut.Len() != 0 {
		t.Errorf("stderr %q", errOut.String())
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

func TestStatsWarnsAboutUnreadResponses(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	ledger.Open(dir).Append(ledger.Record{V: 1, ID: "u", Time: time.Now(), Class: "main", Backend: "upstream",
		Outcome: ledger.OutcomeOK, Unread: "content-encoding br"})

	out, _, _ := stats(t, "-dir", dir)
	if !strings.Contains(out, "WARNING: 1 responses were relayed but could not be read") {
		t.Errorf("no warning about the unread response:\n%s", out)
	}
	if clean, _, _ := stats(t, "-dir", seedLedger(t)); strings.Contains(clean, "WARNING") {
		t.Errorf("a warning was printed with nothing unread:\n%s", clean)
	}
}

func TestStatsSaysWhatLocalAnswersSaved(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l := ledger.Open(dir)
	when := time.Date(2026, 10, 10, 15, 0, 0, 0, time.UTC)
	for _, asked := range []string{"claude-opus-5-5", "janus-local"} {
		l.Append(ledger.Record{V: 1, ID: asked, Time: when, Class: "subagent", Route: "local", Backend: "local",
			Outcome: ledger.OutcomeOK, Request: &ledger.Request{Model: asked},
			Response: &ledger.Response{Model: "qwen/qwen3.8-27b"}, Usage: &ledger.Usage{Input: 10_000, Output: 1000}})
	}

	out, _, _ := stats(t, "-dir", dir)
	squeezed := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		// 10,000 x 4 + 1,000 x 20 = 60,000 millionths
		"Saved by answering locally 1 requests the local model answered would have cost about $0.0600 had Claude answered them",
		"1 local answers are not in it",
	} {
		if !strings.Contains(squeezed, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
	if clean, _, _ := stats(t, "-dir", seedLedger(t)); strings.Contains(clean, "Saved by answering locally") {
		t.Errorf("a saving was reported with nothing answered locally:\n%s", clean)
	}
}

func TestStatsShadowSection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l := ledger.Open(dir)
	when := time.Date(2026, 10, 10, 15, 0, 0, 0, time.UTC)
	for i, c := range []struct{ stop, tools bool }{{true, true}, {true, true}, {true, false}, {false, false}} {
		id := string(rune('a' + i))
		l.Append(ledger.Record{V: 1, ID: id, Time: when, Class: "subagent", Backend: "upstream", Outcome: ledger.OutcomeOK,
			Response: &ledger.Response{Model: "claude-opus-5-5"}, Usage: &ledger.Usage{Output: 100}})
		l.Append(ledger.Record{V: 1, ID: id + "-shadow", ShadowOf: id, Time: when, Class: "subagent", Route: "shadow", Backend: "local",
			Outcome: ledger.OutcomeOK, DurationMs: 12300, Response: &ledger.Response{Blocks: []string{"text"}},
			Usage: &ledger.Usage{Output: 5000}, Compare: &ledger.Compare{StopMatch: c.stop, ToolsMatch: c.tools, UpstreamMs: 2400}})
	}
	l.Append(ledger.Record{V: 1, ID: "e-shadow", ShadowOf: "e", Time: when, Class: "subagent", Route: "shadow", Backend: "local", Outcome: ledger.OutcomeSkipped})

	out, _, code := stats(t, "-dir", dir)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	squeezed := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"4 requests", // the four real ones; shadows are not requests
		"total 4 0 0 0 400",
		"Shadow runs (the local model answering alongside the real answer)",
		"class runs skipped failed compared same stop same tools both local upstream",
		"subagent 5 1 0 4 75% 50% 50% 12.3s 2.4s",
		"Shadow runs are in no other figure in this report.",
	} {
		if !strings.Contains(squeezed, want) {
			t.Errorf("report is missing %q", want)
		}
	}
	if strings.Contains(squeezed, "local 5") || strings.Contains(squeezed, "5,000") {
		t.Errorf("shadow runs leaked into the backend table or the token totals")
	}
	if t.Failed() {
		t.Logf("report was:\n%s", out)
	}
}

func TestStatsLocalFirstSection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l := ledger.Open(dir)
	when := time.Date(2026, 10, 10, 15, 0, 0, 0, time.UTC)
	for i, verdict := range []string{"accepted", "accepted", "accepted", "rejected", "failed"} {
		l.Append(ledger.Record{V: 1, ID: string(rune('a' + i)), Time: when, Class: "subagent", Backend: "upstream", Outcome: ledger.OutcomeOK,
			Attempt: &ledger.Attempt{Model: "qwen/qwen3.8-27b", Verdict: verdict, DurationMs: 10500}})
	}
	out, _, _ := stats(t, "-dir", dir)
	squeezed := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"Local first (the local model tried, with Claude as the fallback)",
		"class tried accepted rejected failed accepted % attempt time",
		"subagent 5 3 1 1 60% 10.5s",
	} {
		if !strings.Contains(squeezed, want) {
			t.Errorf("report is missing %q\n%s", want, out)
		}
	}
	if clean, _, _ := stats(t, "-dir", seedLedger(t)); strings.Contains(clean, "Local first") {
		t.Error("a ledger with no attempts shows a local-first section")
	}
}

func TestStatsNotesWhenLocalStoodInForClaude(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	ledger.Open(dir).Append(ledger.Record{V: 1, ID: "a", Time: time.Now(), Class: "main", Backend: "local", Outcome: ledger.OutcomeOK,
		Attempt: &ledger.Attempt{Backend: "upstream", Verdict: ledger.VerdictFailed}})
	out, _, _ := stats(t, "-dir", dir)
	if !strings.Contains(out, "NOTE: 1 requests were answered by the local model because Claude could not") || strings.Contains(out, "Local first") {
		t.Errorf("report:\n%s", out)
	}
}

func TestStatsDelegatedSection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l := ledger.Open(dir)
	for i := 0; i < 2; i++ {
		l.Append(ledger.Record{V: 1, ID: string(rune('a' + i)), Time: time.Now(), Class: "worker", Backend: "local", Outcome: ledger.OutcomeOK, DurationMs: 9500,
			Worker: &ledger.Worker{Tool: "ask_files", Files: 4, ReadBytes: 1 << 20, AvoidedTokens: 250000}})
	}
	out, _, _ := stats(t, "-dir", dir)
	squeezed := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"Delegated work (tools Claude called that the local model carried out)",
		"tool calls failed files read size read avoided at most time",
		"ask_files 2 0 8 2.0 MB 500,000 9.5s",
		"It is a ceiling",
	} {
		if !strings.Contains(squeezed, want) {
			t.Errorf("report is missing %q\n%s", want, out)
		}
	}
}

func TestStatsGuardNote(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l := ledger.Open(dir)
	for i := 0; i < 3; i++ {
		l.Append(ledger.Record{V: 1, ID: string(rune('a' + i)), Time: time.Now(), Class: "main", Backend: "upstream", Outcome: ledger.OutcomeOK,
			Guard: &ledger.Guard{Findings: 2, Kinds: map[string]int{"aws_access_key": 1, "url_password": 1}, Where: []string{"messages[1] user (tool_result)"}}})
	}
	l.Append(ledger.Record{V: 1, ID: "z", Time: time.Now(), Class: "main", Backend: "upstream", Outcome: ledger.OutcomeOK})

	out, _, _ := stats(t, "-dir", dir)
	squeezed := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"GUARD: 3 requests bound for upstream carried something that looks like a secret (aws_access_key 3, url_password 3).",
		"3 of them were sent with the secrets in them: the guard was only detecting.",
		"counted each time",
	} {
		if !strings.Contains(squeezed, want) {
			t.Errorf("report is missing %q\n%s", want, out)
		}
	}
	if clean, _, _ := stats(t, "-dir", seedLedger(t)); strings.Contains(clean, "GUARD") {
		t.Error("a ledger with no findings shows a guard note")
	}
}

func TestStatsGuardNoteWhenRedacting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l := ledger.Open(dir)
	one := 1
	for i := 0; i < 2; i++ {
		l.Append(ledger.Record{V: 1, ID: string(rune('a' + i)), Time: time.Now(), Class: "main", Backend: "upstream", Outcome: ledger.OutcomeOK,
			Guard: &ledger.Guard{Findings: 1, Kinds: map[string]int{"aws_access_key": 1}, Redacted: &one, Restored: 2}})
	}
	l.Append(ledger.Record{V: 1, ID: "x", Time: time.Now(), Class: "main", Backend: "none", Outcome: ledger.OutcomeHTTPError,
		Guard: &ledger.Guard{Blocked: "the request could not be read to check it for secrets"}})

	out, _, _ := stats(t, "-dir", dir)
	squeezed := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"GUARD: 2 requests bound for upstream carried something that looks like a secret (aws_access_key 2).",
		"2 had their secrets replaced with placeholders before being sent, and 4 placeholders were turned back into secrets in the answers.",
		"1 exchanges were stopped because they could not be made safe.",
	} {
		if !strings.Contains(squeezed, want) {
			t.Errorf("report is missing %q\n%s", want, out)
		}
	}
	if strings.Contains(squeezed, "only detecting") {
		t.Errorf("a fully redacted ledger is described as exposed:\n%s", out)
	}
}

func TestStatsReportsTransforms(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l := ledger.Open(dir)
	when := time.Date(2026, 10, 13, 15, 0, 0, 0, time.UTC)
	for i, tr := range [][]ledger.Transform{
		{{Name: "digest", Blocks: 1, Rewritten: 1, Tokens: 1000, SavedUSD: 0.008, ExpectedUSD: 0.0084}},
		{{Name: "digest", Blocks: 1, Tokens: 1000, SavedUSD: 0.0002}, {Name: "prune", Blocks: 1, Revised: 1, Declined: 2, Tokens: 5000, SavedUSD: 0.001, CostUSD: 0.0004, ExpectedUSD: 0.002}},
	} {
		rec := ledger.Record{V: 1, ID: fmt.Sprint(i), Time: when.Add(time.Duration(i) * time.Minute), Class: "main", Route: "upstream",
			Backend: "upstream", Status: 200, Outcome: ledger.OutcomeOK, Transforms: tr}
		if err := l.Append(rec); err != nil {
			t.Fatal(err)
		}
	}
	out, _, code := stats(t, "-dir", dir)
	squeezed := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"Transforms (requests rewritten on their way upstream)",
		"transform requests rewritten revised declined tokens cut saved expected",
		"digest 2 1 0 0 2,000 $0.0082 $0.0084",
		"prune 1 0 1 2 5,000 $0.0006 $0.0020",
	} {
		if !strings.Contains(squeezed, want) {
			t.Errorf("report is missing %q", want)
		}
	}
	if code != 0 || t.Failed() {
		t.Logf("exit %d, report was:\n%s", code, out)
	}
}

func TestStatsReportsLocalOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l := ledger.Open(dir)
	when := time.Date(2026, 10, 13, 15, 0, 0, 0, time.UTC)
	reason := `Read read secrets/db.yaml, which "secrets/**" makes local-only`
	for i, backend := range []string{"upstream", "upstream", "local", "none"} {
		rec := ledger.Record{V: 1, ID: fmt.Sprint(i), Time: when.Add(time.Duration(i) * time.Minute), Class: "main", Route: "upstream",
			Backend: backend, Status: 200, Outcome: ledger.OutcomeOK, Guard: &ledger.Guard{LocalOnly: []string{reason}}}
		if backend == "upstream" {
			rec.Guard.Withheld = 1
		}
		if err := l.Append(rec); err != nil {
			t.Fatal(err)
		}
	}
	out, _, code := stats(t, "-dir", dir)
	squeezed := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"LOCAL-ONLY: 4 requests held content the local-only policy keeps on this machine.",
		"2 were sent upstream with it replaced by a notice (2 pieces withheld).",
		"1 were answered by the local model instead.",
		"1 were refused: they could not be answered without sending it.",
		"4: " + reason,
	} {
		if !strings.Contains(squeezed, want) {
			t.Errorf("report is missing %q", want)
		}
	}
	if code != 0 || t.Failed() {
		t.Logf("exit %d, report was:\n%s", code, out)
	}
}
