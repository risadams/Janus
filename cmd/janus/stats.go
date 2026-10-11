// stats.go — the `janus stats` command: where the tokens and the money
// went, read from the ledger.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
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

	overrides := ledger.Prices{}
	for model, p := range cfg.Pricing {
		overrides[model] = ledger.Price{
			Input: p.Input, Output: p.Output, CacheRead: p.CacheRead,
			CacheWrite5m: p.CacheWrite5m, CacheWrite1h: p.CacheWrite1h,
		}
	}
	report := ledger.Summarise(records, ledger.DefaultPrices.With(overrides))
	report.Malformed = malformed

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

	printLocalFirst(w, rep.LocalFirst)
	printDelegated(w, rep.Delegated)
	printShadow(w, rep.Shadow)

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
	if rep.StoodIn > 0 {
		fmt.Fprintf(w, "\nNOTE: %s requests were answered by the local model because Claude could not\n"+
			"answer them (unreachable, rate limited, overloaded, or failing). Search the\n"+
			"ledger for \"sent local\" to see which.\n", commas(rep.StoodIn))
	}
	if rep.Unread > 0 {
		fmt.Fprintf(w, "\nWARNING: %s responses were relayed but could not be read, so their tokens\n"+
			"are missing from every figure above. Run janus stats -json, or search the\n"+
			"ledger for \"unread\", to see why.\n", commas(rep.Unread))
	}
	if rep.Malformed > 0 {
		fmt.Fprintf(w, "\n%d ledger lines could not be read and were skipped.\n", rep.Malformed)
	}
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
	fmt.Fprintf(w, "  Accepted answers came from the local model. Rejected ones failed a check and\n"+
		"  failed ones found it unavailable; Claude answered both, after the attempt time\n"+
		"  (a median) had already been spent.\n")
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
