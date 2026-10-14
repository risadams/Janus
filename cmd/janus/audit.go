// audit.go is `janus audit`: what left this machine, request by request.

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/guard"
	"github.com/risadams/janus/internal/ledger"
	"github.com/risadams/janus/internal/proxy"
)

func runAudit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("janus audit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: janus audit [flags] [request id]")
		fmt.Fprintln(stderr, "\nShows what went to Anthropic: how many requests, how many janus changed on the")
		fmt.Fprintln(stderr, "way, and what became of any secrets in them. With a request id, shows that one")
		fmt.Fprintln(stderr, "request, and with -body prints it exactly as it was sent.")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", config.DefaultPath, "path to the config file")
	dir := fs.String("dir", "", "log directory holding ledger/ and captures/ (default: logging.dir from the config)")
	since := fs.String("since", "", "only requests from this point: a date, a time, or an age (24h, 7d)")
	until := fs.String("until", "", "only requests before this point")
	verify := fs.Bool("verify", false, "re-read every saved request that was sent to Anthropic and look for secrets in it")
	body := fs.Bool("body", false, "with a request id: print the request exactly as it was sent, and nothing else")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 || (*body && fs.NArg() != 1) {
		fs.Usage()
		return 2
	}

	cfg, err := config.Read(*configPath)
	if err == nil {
		cfg = config.Apply(cfg, config.Overrides{}, os.Getenv)
		err = cfg.Validate()
	}
	if err != nil {
		fmt.Fprintf(stderr, "janus audit: %v\n", err)
		return 1
	}
	if *dir == "" {
		*dir = cfg.Logging.Dir
	}
	now := time.Now()
	from, err := parseWhen(*since, now)
	if err != nil {
		fmt.Fprintf(stderr, "janus audit: -since: %v\n", err)
		return 2
	}
	to, err := parseWhen(*until, now)
	if err != nil {
		fmt.Fprintf(stderr, "janus audit: -until: %v\n", err)
		return 2
	}
	records, _, err := ledger.Read(filepath.Join(*dir, "ledger"), from, to)
	if err != nil {
		fmt.Fprintf(stderr, "janus audit: %v\n", err)
		return 1
	}
	captures := filepath.Join(*dir, "captures")

	if fs.NArg() == 1 {
		return auditOne(stdout, stderr, records, captures, fs.Arg(0), *body)
	}

	a := tally(records)
	printAudit(stdout, *dir, a)
	if !*verify {
		return 0
	}
	detector, policy := newDetector(cfg), newPolicy(cfg)
	leaks := verifySent(stdout, a.sent, captures, func(req *anthropic.Request) guard.Report {
		report := detector.ScanRequest(req)
		if policy == nil {
			return report
		}
		// Local-only content that went upstream is a leak like any other.
		// Remaining, not Check: a request the content was withheld from
		// still names the file, and that is not a leak.
		if m := policy.Check(req); policy.Remaining(req) > 0 {
			if report.Kinds == nil {
				report.Kinds = map[string]int{}
			}
			report.Findings += len(m.Reasons)
			report.Kinds["local-only content"] += len(m.Reasons)
			report.Where = append(report.Where, m.Reasons...)
		}
		return report
	})
	if leaks > 0 {
		return 1
	}
	return 0
}

// audit is the tally of where requests went.
type audit struct {
	from, to time.Time
	requests int
	// Requests that reached Anthropic, as received and as changed.
	untouched, changed int
	redacted, repaired int
	withheld           int
	// keptLocal counts requests the local-only policy kept from Anthropic
	// altogether: answered locally, or refused.
	keptLocal int
	// Requests that did not.
	localOnly, stopped int
	// Secrets.
	carried, replaced int
	kinds             map[string]int
	exposed           []ledger.Record // sent to Anthropic with secrets in them
	blocked           []ledger.Record
	// sent is every record that reached Anthropic, for verification.
	sent []ledger.Record
}

// reachedAnthropic reports whether a request was sent to the upstream at
// any point, whether or not the upstream ended up answering it.
func reachedAnthropic(rec ledger.Record) bool {
	return rec.Backend == "upstream" || (rec.Attempt != nil && rec.Attempt.Backend == "upstream")
}

// carriesContent reports whether a record is of a request that carries the
// user's content: a Messages call, or the token count for one.
func carriesContent(rec ledger.Record) bool {
	return rec.Method == "POST" && (rec.Path == "/v1/messages" || rec.Path == "/v1/messages/count_tokens") &&
		rec.ShadowOf == "" && rec.Worker == nil
}

func tally(records []ledger.Record) audit {
	a := audit{kinds: map[string]int{}}
	for _, rec := range records {
		if !carriesContent(rec) {
			continue
		}
		if a.requests == 0 {
			a.from = rec.Time
		}
		a.to = rec.Time
		a.requests++

		g := rec.Guard
		switch {
		case reachedAnthropic(rec):
			a.sent = append(a.sent, rec)
			wasRedacted := g != nil && g.Redacted != nil && *g.Redacted > 0
			wasRepaired := false
			for _, stage := range rec.Stages {
				wasRepaired = wasRepaired || stage == backend.StageDropThinking
			}
			if wasRedacted {
				a.redacted++
			}
			if wasRepaired {
				a.repaired++
			}
			wasWithheld := g != nil && g.Withheld > 0
			if wasWithheld {
				a.withheld++
			}
			if wasRedacted || wasRepaired || wasWithheld {
				a.changed++
			} else {
				a.untouched++
			}
		case rec.Backend == "local":
			a.localOnly++
		default:
			a.stopped++
		}

		if g == nil {
			continue
		}
		if len(g.LocalOnly) > 0 && !reachedAnthropic(rec) {
			a.keptLocal++
		}
		if g.Blocked != "" {
			a.blocked = append(a.blocked, rec)
		}
		if g.Findings > 0 && reachedAnthropic(rec) {
			a.carried++
			for kind, n := range g.Kinds {
				a.kinds[kind] += n
			}
			if g.Redacted != nil {
				a.replaced++
			} else {
				a.exposed = append(a.exposed, rec)
			}
		}
	}
	return a
}

func printAudit(w io.Writer, dir string, a audit) {
	fmt.Fprintf(w, "Audit of %s\n", dir)
	if a.requests == 0 {
		fmt.Fprintln(w, "No requests carrying content were recorded in this range.")
		return
	}
	const stamp = "2006-01-02 15:04"
	fmt.Fprintf(w, "%s to %s (local time)\n\n", a.from.Local().Format(stamp), a.to.Local().Format(stamp))

	fmt.Fprintf(w, "Where %s requests carrying your content went\n", commas(a.requests))
	fmt.Fprintf(w, "  %6s  to Anthropic, exactly as your client sent them\n", commas(a.untouched))
	fmt.Fprintf(w, "  %6s  to Anthropic, changed by janus on the way", commas(a.changed))
	if a.changed > 0 {
		var how []string
		if a.redacted > 0 {
			how = append(how, fmt.Sprintf("%s with secrets replaced", commas(a.redacted)))
		}
		if a.withheld > 0 {
			how = append(how, fmt.Sprintf("%s with local-only content withheld", commas(a.withheld)))
		}
		if a.repaired > 0 {
			how = append(how, fmt.Sprintf("%s with a local model's thinking removed", commas(a.repaired)))
		}
		fmt.Fprintf(w, " (%s)", strings.Join(how, ", "))
	}
	fmt.Fprintf(w, "\n  %6s  to the local model only, never to Anthropic\n", commas(a.localOnly))
	fmt.Fprintf(w, "  %6s  refused or stopped by janus, and sent nowhere\n", commas(a.stopped))

	if a.withheld > 0 || a.keptLocal > 0 {
		fmt.Fprintf(w, "\nLocal-only content\n")
		fmt.Fprintf(w, "  %6s  requests went to Anthropic with it replaced by a notice\n", commas(a.withheld))
		fmt.Fprintf(w, "  %6s  requests were kept from Anthropic because of it\n", commas(a.keptLocal))
	}

	fmt.Fprintf(w, "\nSecrets\n")
	if a.carried == 0 {
		fmt.Fprintf(w, "  None were found in anything bound for Anthropic. That means the guard found\n"+
			"  none, or was off; it is not proof there were none. Run with -verify to look again.\n")
	} else {
		kinds := make([]string, 0, len(a.kinds))
		for kind, n := range a.kinds {
			kinds = append(kinds, fmt.Sprintf("%s %s", kind, commas(n)))
		}
		sort.Strings(kinds)
		fmt.Fprintf(w, "  %s requests bound for Anthropic carried something that looks like a secret\n  (%s).\n", commas(a.carried), strings.Join(kinds, ", "))
		fmt.Fprintf(w, "  %6s  had them replaced with placeholders before sending\n", commas(a.replaced))
		fmt.Fprintf(w, "  %6s  were SENT WITH THE SECRETS IN THEM\n", commas(len(a.exposed)))
		for _, rec := range a.exposed {
			fmt.Fprintf(w, "            %s  %s  in %s\n", rec.ID, kindList(rec.Guard.Kinds), strings.Join(rec.Guard.Where, "; "))
		}
		if len(a.exposed) > 0 {
			fmt.Fprintf(w, "  Treat those secrets as disclosed and rotate them. Set guard.redact to stop it\n  happening again.\n")
		}
	}
	if len(a.blocked) > 0 {
		fmt.Fprintf(w, "\nStopped by the guard\n")
		for _, rec := range a.blocked {
			fmt.Fprintf(w, "  %s  %s\n", rec.ID, rec.Guard.Blocked)
		}
	}
}

func kindList(kinds map[string]int) string {
	names := make([]string, 0, len(kinds))
	for k := range kinds {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// sentPath is the file holding a request as it was sent: the .sent.txt
// janus saves when it changed the request, and otherwise the request as
// the client sent it, since that is then what went.
func sentPath(captures string, rec ledger.Record) (path string, ok bool) {
	if rec.Capture == "" {
		return "", false
	}
	base := filepath.Join(captures, rec.Capture)
	for _, ext := range []string{".sent.txt", ".request.txt"} {
		if _, err := os.Stat(base + ext); err == nil {
			return base + ext, true
		}
	}
	return "", false
}

// verifySent re-reads every saved request that went to Anthropic and looks
// for secrets in it, with the detector as it is configured now. It returns
// how many still carry one.
//
// This is the check that does not take the ledger's word for it: it looks
// at the bytes that were sent.
func verifySent(w io.Writer, sent []ledger.Record, captures string, scan func(*anthropic.Request) guard.Report) (leaks int) {
	clean, unchecked := 0, 0
	var lines []string
	for _, rec := range sent {
		path, ok := sentPath(captures, rec)
		if !ok {
			unchecked++
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			unchecked++
			continue
		}
		req, err := anthropic.ParseRequest(data)
		if err != nil {
			unchecked++
			continue
		}
		report := scan(req)
		if report.Findings == 0 {
			clean++
			continue
		}
		leaks++
		lines = append(lines, fmt.Sprintf("            %s  %s  in %s", rec.ID, report.Summary(), strings.Join(report.Where, "; ")))
	}

	fmt.Fprintf(w, "\nVerification: re-read %s requests as they were sent to Anthropic\n", commas(len(sent)))
	fmt.Fprintf(w, "  %6s  contain nothing that looks like a secret\n", commas(clean))
	fmt.Fprintf(w, "  %6s  STILL CONTAIN SOMETHING THAT LOOKS LIKE A SECRET\n", commas(leaks))
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
	fmt.Fprintf(w, "  %6s  could not be checked: no capture was saved, or it has been pruned\n", commas(unchecked))
	if unchecked > 0 {
		fmt.Fprintf(w, "  Captures have to be on (logging.capture) for a request to be checked later.\n")
	}
	fmt.Fprintf(w, "  This uses the detector as configured now, so it also finds what a pattern\n  added since would have caught. It cannot find what no detector recognises.\n")
	return leaks
}

// auditOne shows one request, or with body set prints it as it was sent.
func auditOne(stdout, stderr io.Writer, records []ledger.Record, captures, id string, body bool) int {
	var rec *ledger.Record
	for i := range records {
		if records[i].ID == id {
			rec = &records[i]
			break
		}
	}
	if rec == nil {
		fmt.Fprintf(stderr, "janus audit: no request %q in the ledger for this range\n", id)
		return 1
	}
	path, saved := sentPath(captures, *rec)

	if body {
		if !reachedAnthropic(*rec) && rec.Backend != "local" {
			fmt.Fprintf(stderr, "janus audit: request %s was not sent anywhere: %s\n", id, rec.Reason)
			return 1
		}
		if !saved {
			fmt.Fprintf(stderr, "janus audit: no capture of request %s was saved, or it has been pruned\n", id)
			return 1
		}
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(stderr, "janus audit: %v\n", err)
			return 1
		}
		stdout.Write(data)
		return 0
	}

	fmt.Fprintf(stdout, "Request %s\n", rec.ID)
	fmt.Fprintf(stdout, "  When      %s\n", rec.Time.Local().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(stdout, "  What      %s %s, class %s\n", rec.Method, rec.Path, rec.Class)
	switch {
	case reachedAnthropic(*rec) && rec.Backend == "local":
		fmt.Fprintf(stdout, "  Went to   Anthropic, which could not answer; then the local model\n")
	case reachedAnthropic(*rec):
		fmt.Fprintf(stdout, "  Went to   Anthropic\n")
	case rec.Backend == "local":
		fmt.Fprintf(stdout, "  Went to   the local model only; it was not sent to Anthropic\n")
	default:
		fmt.Fprintf(stdout, "  Went to   nowhere: janus refused or stopped it\n")
	}
	fmt.Fprintf(stdout, "  Why       %s\n", rec.Reason)
	if len(rec.Stages) > 0 {
		fmt.Fprintf(stdout, "  Changed   %s\n", strings.Join(rec.Stages, ", "))
	}
	// Why the local model did not answer this request, if it was meant to:
	// tried first and failed (an attempt), or passed over without trying.
	if a := rec.Attempt; a != nil && a.Backend == "local" && a.Verdict != ledger.VerdictAccepted {
		fmt.Fprintf(stdout, "  Local     tried first and did not answer: %s\n", a.Why)
		if h := proxy.CauseHint(ledger.AttemptCause(*a)); h != "" {
			fmt.Fprintf(stdout, "            next step: %s\n", h)
		}
	} else if c := ledger.LocalCause(*rec); c != "" {
		if h := proxy.CauseHint(c); h != "" {
			fmt.Fprintf(stdout, "  Local     not tried here; next step: %s\n", h)
		}
	}
	if g := rec.Guard; g != nil && len(g.LocalOnly) > 0 {
		switch {
		case g.Withheld > 0:
			fmt.Fprintf(stdout, "  Local-only %d pieces withheld before sending: %s\n", g.Withheld, strings.Join(g.LocalOnly, "; "))
		default:
			fmt.Fprintf(stdout, "  Local-only %s\n", strings.Join(g.LocalOnly, "; "))
		}
	}
	if g := rec.Guard; g != nil && (g.Blocked != "" || g.Findings > 0 || g.Redacted != nil) {
		switch {
		case g.Blocked != "":
			fmt.Fprintf(stdout, "  Guard     stopped: %s\n", g.Blocked)
		case g.Redacted != nil:
			fmt.Fprintf(stdout, "  Guard     %d found (%s) in %s\n            %d distinct secrets replaced before sending; %d placeholders put back in the answer\n",
				g.Findings, kindList(g.Kinds), strings.Join(g.Where, "; "), *g.Redacted, g.Restored)
		default:
			fmt.Fprintf(stdout, "  Guard     %d found (%s) in %s\n            NOT replaced: the request was sent with them in it\n",
				g.Findings, kindList(g.Kinds), strings.Join(g.Where, "; "))
		}
	} else {
		fmt.Fprintf(stdout, "  Guard     nothing found, or the guard was off\n")
	}
	switch {
	case !saved:
		fmt.Fprintf(stdout, "  Sent      not saved: captures were off, or it has been pruned\n")
	case strings.HasSuffix(path, ".sent.txt"):
		fmt.Fprintf(stdout, "  Sent      %s\n            (as janus sent it; your client's original is beside it as .request.txt)\n", path)
	default:
		fmt.Fprintf(stdout, "  Sent      %s\n            (as your client sent it; janus did not change it)\n", path)
	}
	fmt.Fprintf(stdout, "\nPrint exactly what was sent with: janus audit -body %s\n", rec.ID)
	return 0
}
