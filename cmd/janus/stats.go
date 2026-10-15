// stats.go is `janus stats`: where the tokens and the money went, read
// from the ledger.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/ledger"
)

func runStats(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("janus stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", config.DefaultPath, "path to the config file")
	dir := fs.String("dir", "", "ledger directory (default: <logging.dir>/ledger from the config)")
	since := fs.String("since", "", "only requests from this point: a date (2026-10-01), a time (RFC 3339), or an age (24h, 7d)")
	until := fs.String("until", "", "only requests before this point; same forms as -since")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	daily := fs.Bool("daily", false, "add a breakdown by day")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "janus stats: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	// Read, not Load: looking at the ledger must not write a config file.
	cfg, err := config.Read(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "janus stats: %v\n", err)
		return 1
	}
	if *dir == "" {
		*dir = filepath.Join(cfg.Logging.Dir, "ledger")
		// Without a config file the ledger folder is only a guess, so say so
		// rather than report an empty ledger as if nothing had happened.
		if _, err := os.Stat(*configPath); os.IsNotExist(err) {
			fmt.Fprintf(stderr, "janus stats: no config file at %s, so reading the default ledger folder; pass -config with the file janus runs with\n", *configPath)
		}
	}

	now := time.Now()
	from, err := parseWhen(*since, now)
	if err != nil {
		fmt.Fprintf(stderr, "janus stats: -since: %v\n", err)
		return 2
	}
	to, err := parseWhen(*until, now)
	if err != nil {
		fmt.Fprintf(stderr, "janus stats: -until: %v\n", err)
		return 2
	}

	records, malformed, err := ledger.Read(*dir, from, to)
	if err != nil {
		fmt.Fprintf(stderr, "janus stats: %v\n", err)
		return 1
	}

	report := ledger.Summarise(records, prices(cfg))
	report.Malformed = malformed
	if *daily {
		report.ByDay = ledger.ByDay(records, prices(cfg), time.Local)
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(report)
		return 0
	}
	printReport(stdout, *dir, report)
	return 0
}

// parseWhen reads a point in time given as a date, an RFC 3339 time, or an
// age such as 24h or 7d counted back from now. Empty means no limit. Dates
// are local, because that is what "since Monday" means to the person
// typing it.
func parseWhen(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.ParseFloat(days, 64); err == nil && n >= 0 {
			return now.Add(-time.Duration(n * 24 * float64(time.Hour))), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("%q is not a date (2026-10-01), a time (2026-10-01T09:00:00Z), or an age (24h, 7d)", s)
}

func printReport(w io.Writer, dir string, rep ledger.Report) {
	fmt.Fprintf(w, "Ledger %s\n", dir)
	if rep.Total.Requests == 0 && len(rep.Delegated) == 0 && len(rep.Shadow) == 0 {
		fmt.Fprintln(w, "No requests recorded in this range.")
		if rep.Malformed > 0 {
			fmt.Fprintf(w, "%d ledger lines could not be read and were skipped.\n", rep.Malformed)
		}
		return
	}
	const stamp = "2006-01-02 15:04"
	fmt.Fprintf(w, "%s to %s (local time), %s requests\n",
		rep.From.Local().Format(stamp), rep.To.Local().Format(stamp), commas(rep.Total.Requests))

	if rep.Total.Requests > 0 {
		printTable(w, "class", rep.ByClass, &rep.Total)
		printTable(w, "model", rep.ByModel, nil)
		printTable(w, "backend", rep.ByBackend, nil)
	}
	printDays(w, rep.ByDay)
	printQuota(w, rep.Quota, time.Now())

	printLocalSaving(w, rep.LocalSaving)
	printLocalFirst(w, rep.LocalFirst)
	printLocalFirstCause(w, rep.LocalFirstCause)
	printDelegated(w, rep.Delegated)
	printShadow(w, rep.Shadow)
	printTransforms(w, rep.Transforms)

	outcomes := make([]string, 0, len(rep.Outcomes))
	for outcome, count := range rep.Outcomes {
		outcomes = append(outcomes, fmt.Sprintf("%s %s", outcome, commas(count)))
	}
	sort.Strings(outcomes)
	if len(outcomes) > 0 {
		fmt.Fprintf(w, "\nOutcomes: %s\n", strings.Join(outcomes, ", "))
	}

	fmt.Fprintf(w, "\nCosts are estimates at Anthropic's list prices as of %s. A subscription\n"+
		"is not billed per token, so read them as a measure of usage, not a bill.\n", rep.PricesChecked)
	if rep.Total.Unpriced > 0 {
		reasons := make([]string, 0, len(rep.Unpriced))
		for reason, count := range rep.Unpriced {
			reasons = append(reasons, fmt.Sprintf("  %s: %s", commas(count), reason))
		}
		sort.Strings(reasons)
		fmt.Fprintf(w, "\n%s requests could not be priced and are in no cost above:\n%s\n",
			commas(rep.Total.Unpriced), strings.Join(reasons, "\n"))
	}
	if g := rep.Guard; g.Requests > 0 || g.Blocked > 0 {
		kinds := make([]string, 0, len(g.Kinds))
		for kind, n := range g.Kinds {
			kinds = append(kinds, fmt.Sprintf("%s %s", kind, commas(n)))
		}
		sort.Strings(kinds)
		fmt.Fprintf(w, "\nGUARD: %s requests bound for upstream carried something that looks like a\nsecret (%s).\n", commas(g.Requests), strings.Join(kinds, ", "))
		if exposed := g.Requests - g.Redacted; exposed > 0 {
			fmt.Fprintf(w, "  %s of them were sent with the secrets in them: the guard was only detecting.\n"+
				"  Set guard.redact to replace secrets before they leave.\n", commas(exposed))
		}
		if g.Redacted > 0 {
			fmt.Fprintf(w, "  %s had their secrets replaced with placeholders before being sent, and %s\n"+
				"  placeholders were turned back into secrets in the answers.\n", commas(g.Redacted), commas(g.Restored))
		}
		if g.Blocked > 0 {
			fmt.Fprintf(w, "  %s exchanges were stopped because they could not be made safe. Search the\n  ledger for \"blocked\" to see why.\n", commas(g.Blocked))
		}
		fmt.Fprintf(w, "  A secret in a conversation is resent with every turn and counted each time.\n")
	}
	if lo := rep.LocalOnly; lo.Requests > 0 {
		fmt.Fprintf(w, "\nLOCAL-ONLY: %s requests held content the local-only policy keeps on this\nmachine.\n", commas(lo.Requests))
		if lo.Withheld > 0 {
			fmt.Fprintf(w, "  %s were sent upstream with it replaced by a notice (%s pieces withheld).\n", commas(lo.Withheld), commas(lo.Pieces))
		}
		if lo.KeptLocal > 0 {
			fmt.Fprintf(w, "  %s were answered by the local model instead.\n", commas(lo.KeptLocal))
		}
		if lo.Refused > 0 {
			fmt.Fprintf(w, "  %s were refused: they could not be answered without sending it.\n", commas(lo.Refused))
		}
		type reason struct {
			text string
			n    int
		}
		var reasons []reason
		for text, n := range lo.Reasons {
			reasons = append(reasons, reason{text, n})
		}
		sort.Slice(reasons, func(i, j int) bool {
			if reasons[i].n != reasons[j].n {
				return reasons[i].n > reasons[j].n
			}
			return reasons[i].text < reasons[j].text
		})
		if len(reasons) > 3 {
			reasons = reasons[:3]
		}
		for _, r := range reasons {
			fmt.Fprintf(w, "  %s: %s\n", commas(r.n), r.text)
		}
		fmt.Fprintf(w, "  Content in a conversation is withheld again on every turn and counted each time.\n")
	}
	printOutages(w, rep.Outages)
	if rep.Unread > 0 {
		fmt.Fprintf(w, "\nWARNING: %s responses were relayed but could not be read, so their tokens\n"+
			"are missing from every figure above. Run janus stats -json, or search the\n"+
			"ledger for \"unread\", to see why.\n", commas(rep.Unread))
	}
	if rep.Malformed > 0 {
		fmt.Fprintf(w, "\n%d ledger lines could not be read and were skipped.\n", rep.Malformed)
	}
}

// printOutages lists the runs of requests Claude could not answer, and how
// many the local model answered instead.
func printOutages(w io.Writer, outages []ledger.Outage) {
	if len(outages) == 0 {
		return
	}
	const stamp, clock = "2006-01-02 15:04", "15:04"
	when := func(t, since time.Time) string {
		if !since.IsZero() && t.Local().Format(time.DateOnly) == since.Local().Format(time.DateOnly) {
			return t.Local().Format(clock)
		}
		return t.Local().Format(stamp)
	}
	fmt.Fprintf(w, "\nWhen Claude could not answer\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  from\tto\tcause\trequests\tstood in\tlimit\tanswering again\n")
	stoodIn := 0
	for _, o := range outages {
		limit, lifts := o.Limit, "-"
		if limit == "" {
			limit = "-"
		}
		if !o.Lifts.IsZero() {
			lifts = when(o.Lifts, o.End)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n", when(o.Start, time.Time{}), when(o.End, o.Start),
			o.Cause, commas(o.Requests), commas(o.StoodIn), limit, lifts)
		stoodIn += o.StoodIn
	}
	tw.Flush()
	fmt.Fprintf(w, "  Requests no more than %d minutes apart are one row; times are local. \"Stood\n"+
		"  in\" were answered by the local model, %s in all; the rest reached the client\n"+
		"  as errors. \"Limit\" is the subscription limit reached, and \"answering\n"+
		"  again\" when Claude said it would. Search the ledger for \"sent local\" to see\n"+
		"  the stand-ins.\n", int(ledger.OutageGap.Minutes()), commas(stoodIn))
}

// printTable writes one breakdown. "cache hit" is the share of the prompt
// that was read from cache.
func printTable(w io.Writer, by string, rows []ledger.Row, total *ledger.Row) {
	fmt.Fprintf(w, "\nBy %s\n", by)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "  %s\trequests\tinput\tcache write\tcache read\toutput\tcache hit\test. cost\t\n", by)
	line := func(r ledger.Row) {
		hit := "-"
		if ratio := r.CacheHitRatio(); ratio >= 0 {
			hit = fmt.Sprintf("%.1f%%", ratio*100)
		}
		cost := fmt.Sprintf("$%.4f", r.CostUSD)
		if r.Unpriced > 0 {
			cost += "*"
		}
		key := r.Key
		if key == "" {
			key = "(none)"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t\n", key, commas(r.Requests),
			commas(r.Input), commas(r.CacheWrite), commas(r.CacheRead), commas(r.Output), hit, cost)
	}
	for _, r := range rows {
		line(r)
	}
	if total != nil {
		line(*total)
	}
	tw.Flush()
}

// printQuota says where the subscription's limits stood at the end of the
// range, and which kinds of request used the five-hour limit.
func printQuota(w io.Writer, q *ledger.QuotaSummary, now time.Time) {
	if q == nil || q.Latest == nil {
		return
	}
	fmt.Fprintf(w, "\nUsage limits, as of %s\n", q.At.Local().Format("2006-01-02 15:04"))
	windows := make([]string, 0, len(q.Latest.Windows))
	for name := range q.Latest.Windows {
		windows = append(windows, name)
	}
	sort.Strings(windows)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, name := range windows {
		win := q.Latest.Windows[name]
		label := limitNames[name]
		if label == "" {
			label = name
		}
		resets := ""
		switch {
		case win.Resets.IsZero():
		case win.Resets.Before(now):
			resets = "reset since, at " + win.Resets.Local().Format("2006-01-02 15:04")
		default:
			resets = "resets " + resetTime(win.Resets, now)
		}
		fmt.Fprintf(tw, "  %s\t%.0f%% used\t%s\n", label, win.Utilization*100, resets)
	}
	tw.Flush()
	if len(q.ByClass) > 0 {
		parts := make([]string, 0, len(q.ByClass))
		for _, c := range q.ByClass {
			parts = append(parts, fmt.Sprintf("%s %.0f%%", c.Class, c.Share*100))
		}
		fmt.Fprintf(w, "  The five-hour limit used in this range, by class: %s.\n"+
			"  Each rise in use is put down to the request that reported it, and the API\n"+
			"  reports use in whole percents, so read the shares as rough.\n", strings.Join(parts, ", "))
	}
}

// printDays writes the breakdown by day, so a day that went worse than the
// others stands out.
func printDays(w io.Writer, rows []ledger.DayRow) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(w, "\nBy day\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "  day\trequests\tfailed\tcache hit\test. cost\tlocal tried\taccepted\tsaved locally\tstood in\t\n")
	for _, r := range rows {
		hit := "-"
		if r.CacheHit >= 0 {
			hit = fmt.Sprintf("%.1f%%", r.CacheHit*100)
		}
		cost := fmt.Sprintf("$%.4f", r.CostUSD)
		if r.Unpriced > 0 {
			cost += "*"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t$%.4f\t%s\t\n", r.Day, commas(r.Requests), commas(r.Failed),
			hit, cost, commas(r.LocalTried), commas(r.LocalAccepted), r.LocalSavedUSD, commas(r.StoodIn))
	}
	tw.Flush()
	fmt.Fprintf(w, "  Days are local dates. \"failed\" leaves out requests the client cancelled.\n")
}

// printLocalSaving reports what the local model's answers would have cost
// had Claude given them, which is what routing to it saved.
func printLocalSaving(w io.Writer, s ledger.LocalSaving) {
	if s.Requests == 0 {
		return
	}
	priced := s.Requests - s.Unpriced
	fmt.Fprintf(w, "\nSaved by answering locally\n")
	if priced > 0 {
		fmt.Fprintf(w, "  %s requests the local model answered would have cost about $%.4f had\n"+
			"  Claude answered them, at the list price of the model each asked for. The\n"+
			"  local model counts and caches tokens differently from Claude, so read it as\n"+
			"  the size of the saving, not an exact figure.\n", commas(priced), s.USD)
	}
	if s.Unpriced > 0 {
		fmt.Fprintf(w, "  %s local answers are not in it: they reported no usage, or asked for a\n"+
			"  model with no price, such as a local alias.\n", commas(s.Unpriced))
	}
}

// printLocalFirst reports how often trying the local model first paid off.
func printLocalFirst(w io.Writer, rows []ledger.LocalFirstRow) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(w, "\nLocal first (the local model tried, with Claude as the fallback)\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "  class\ttried\taccepted\trejected\tfailed\taccepted %%\tattempt time\t\n")
	for _, r := range rows {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%.0f%%\t%.1fs\t\n", r.Class, commas(r.Tried), commas(r.Accepted),
			commas(r.Rejected), commas(r.Failed), 100*float64(r.Accepted)/float64(r.Tried), float64(r.MedianMs)/1000)
	}
	tw.Flush()
	fmt.Fprintf(w, "  Accepted answers came from the local model. Rejected ones came back whole and\n"+
		"  failed a check; failed ones never gave an answer to judge (why is below).\n"+
		"  Claude answered both, after the attempt time (a median) had been spent.\n")
}

// printLocalFirstCause reports why the local model did not answer:
// attempts that ended that way, and requests a rule or stand-in passed
// over without trying it. Each cause names its next step.
func printLocalFirstCause(w io.Writer, rows []ledger.CauseRow) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(w, "\nWhy the local model did not answer, and what to do about it\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  cause\ttried\tnever tried\tnext step\n")
	for _, r := range rows {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", r.Cause, commas(r.Attempts), commas(r.NotTried), r.Hint())
	}
	tw.Flush()
	fmt.Fprintf(w, "  \"Tried\" is a local attempt that ended that way; Claude answered.\n"+
		"  \"Never tried\" is a request a rule meant for the local model that was\n"+
		"  sent upstream (or left without a stand-in) for that reason.\n")
}

// printDelegated reports the work Claude handed to the local model.
func printDelegated(w io.Writer, rows []ledger.DelegatedRow) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(w, "\nDelegated work (tools Claude called that the local model carried out)\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "  tool\tcalls\tfailed\tfiles read\tsize read\tavoided at most\ttime\t\n")
	for _, r := range rows {
		read := fmt.Sprintf("%d kB", r.ReadBytes/1024)
		if r.ReadBytes >= 1<<20 {
			read = fmt.Sprintf("%.1f MB", float64(r.ReadBytes)/(1<<20))
		}
		took := "-"
		if r.Calls > r.Failed {
			took = fmt.Sprintf("%.1fs", float64(r.MedianMs)/1000)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t\n", r.Tool, commas(r.Calls), commas(r.Failed), commas(r.Files), read, commas(r.AvoidedTokens), took)
	}
	tw.Flush()
	fmt.Fprintf(w, "  \"avoided at most\" is the input tokens Claude would have spent reading every\n"+
		"  one of those files in full, less what the answers cost it. It is a ceiling: left\n"+
		"  to itself Claude often searches a file and reads a little of it. Compare cost\n"+
		"  per session with and without the tool to see what was really saved. Time is\n"+
		"  the median per call.\n")
}

// prices is the price table janus ships with, as the config corrects it.
func prices(cfg config.Config) ledger.Prices {
	overrides := ledger.Prices{}
	for model, p := range cfg.Pricing {
		overrides[model] = ledger.Price{
			Input: p.Input, Output: p.Output, CacheRead: p.CacheRead,
			CacheWrite5m: p.CacheWrite5m, CacheWrite1h: p.CacheWrite1h,
		}
	}
	return ledger.DefaultPrices.With(overrides)
}

// printTransforms reports what each transform saved, beside what the
// net-savings guard expected.
func printTransforms(w io.Writer, rows []ledger.TransformRow) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(w, "\nTransforms (requests rewritten on their way upstream)\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "  transform\trequests\trewritten\trevised\tdeclined\ttokens cut\tsaved\texpected\t\n")
	for _, r := range rows {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t$%.4f\t$%.4f\t\n", r.Name, commas(r.Requests), commas(r.Rewritten),
			commas(r.Revised), commas(r.Declined), commas(r.Tokens), r.SavedUSD-r.CostUSD, r.ExpectedUSD)
	}
	tw.Flush()
	fmt.Fprintf(w, "  \"saved\" is what the tokens cut would have cost on these requests, less the cache\n"+
		"  that revisions broke. \"expected\" is what the guard expected its decisions in\n"+
		"  this range to save over the rest of their conversations. Saved catches up with\n"+
		"  expected as conversations go on; if it stays well below, the guard is too hopeful.\n")
	for _, r := range rows {
		if r.Unpriced > 0 {
			fmt.Fprintf(w, "  %s: %s requests were for a model with no price and are in no dollar figure.\n", r.Name, commas(r.Unpriced))
		}
	}
}

// printShadow reports how the local model did when run alongside real
// answers. The percentages are of the runs that came back whole.
func printShadow(w io.Writer, rows []ledger.ShadowRow) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(w, "\nShadow runs (the local model answering alongside the real answer)\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "  class\truns\tskipped\tfailed\tcompared\tsame stop\tsame tools\tboth\tlocal\tupstream\t\n")
	share := func(n, of int) string {
		if of == 0 {
			return "-"
		}
		return fmt.Sprintf("%.0f%%", 100*float64(n)/float64(of))
	}
	seconds := func(ms int64, any bool) string {
		if !any {
			return "-"
		}
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	for _, r := range rows {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t\n", r.Class,
			commas(r.Runs), commas(r.Skipped), commas(r.Failed), commas(r.Compared),
			share(r.StopMatch, r.Compared), share(r.ToolsMatch, r.Compared), share(r.BothMatch, r.Compared),
			seconds(r.LocalMedianMs, r.Compared > 0), seconds(r.UpstreamMedianMs, r.Compared > 0 && r.UpstreamMedianMs > 0))
	}
	tw.Flush()
	fmt.Fprintf(w, "  \"same stop\" and \"same tools\" compare the shape of the two answers, not their\n"+
		"  content: whether both stopped for the same reason and called the same tools.\n"+
		"  Times are medians. Shadow runs are in no other figure in this report.\n")
}

// commas writes an integer with thousands separators.
func commas(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}
