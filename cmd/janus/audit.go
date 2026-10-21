// audit.go is `janus audit`: what left this machine, request by request.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	fs := newFlagSet("janus audit", stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: janus audit [flags] [request id]")
		fmt.Fprintln(stderr, "\nShows what left this machine, to Anthropic or OpenAI: how many requests, how many")
		fmt.Fprintln(stderr, "janus changed on the way, and what became of any secrets in them. With a request")
		fmt.Fprintln(stderr, "id, shows that one request, and with -body prints it exactly as it was sent.")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", config.DefaultPath, "path to the config file")
	dir := fs.String("dir", "", "log directory holding ledger/ and captures/ (default: logging.dir from the config)")
	since := fs.String("since", "", "only requests from this point: a date, a time, or an age (24h, 7d)")
	until := fs.String("until", "", "only requests before this point")
	verify := fs.Bool("verify", false, "re-read every saved request that was sent off this machine and look for secrets in it")
	body := fs.Bool("body", false, "with a request id: print the request exactly as it was sent, and nothing else")
	asJSON := fs.Bool("json", false, "print the report as JSON; with a request id, its ledger record")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 || (*body && fs.NArg() != 1) || (*body && *asJSON) {
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
		if *asJSON {
			return auditOneJSON(stdout, stderr, records, fs.Arg(0))
		}
		return auditOne(stdout, stderr, records, captures, fs.Arg(0), *body)
	}

	a := tally(records)
	var v *verification
	if *verify {
		v = verifySent(a.sent, captures, scanner(cfg), textScanner(cfg))
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(auditJSON(*dir, a, v)); err != nil {
			fmt.Fprintf(stderr, "janus audit: %v\n", err)
			return 1
		}
	} else {
		p := newPainter(stdout)
		printAudit(stdout, p, *dir, a)
		if v != nil {
			remote := "Anthropic"
			if a.openai.total() > 0 {
				remote = "Anthropic or OpenAI"
			}
			printVerification(stdout, p, v, remote)
		}
	}
	if v != nil && len(v.leaks) > 0 {
		return 1
	}
	return 0
}

// scanner is how -verify looks for secrets and local-only content in a
// request: with the detector and the policy as they are configured now.
func scanner(cfg config.Config) func(*anthropic.Request) guard.Report {
	detector, policy := newDetector(cfg), newPolicy(cfg)
	return func(req *anthropic.Request) guard.Report {
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
	}
}

// textScanner is how -verify looks for secrets in a request that is not a
// Messages request: what was sent to OpenAI, read as its text.
func textScanner(cfg config.Config) func(string) guard.Report {
	detector := newDetector(cfg)
	return func(text string) guard.Report {
		findings := detector.Scan(text)
		report := guard.Report{Findings: len(findings)}
		for _, f := range findings {
			if report.Kinds == nil {
				report.Kinds = map[string]int{}
			}
			report.Kinds[f.Kind]++
		}
		if len(findings) > 0 {
			report.Where = []string{"the request as sent to OpenAI"}
		}
		return report
	}
}

// audit is the tally of where requests went.
type audit struct {
	from, to time.Time
	requests int
	// Requests that reached Anthropic, and OpenAI, as received and as
	// changed. One sent to both, the first failing, counts under each.
	anthropic, openai sentTo
	// keptLocal counts requests the local-only policy kept off this
	// machine altogether: answered locally, or refused.
	keptLocal int
	// Requests that did not.
	localOnly, stopped int
	// Secrets.
	carried, replaced int
	kinds             map[string]int
	exposed           []ledger.Record // sent off this machine with secrets in them
	blocked           []ledger.Record
	// sent is every record that left this machine, for verification.
	sent []ledger.Record
}

// sentTo counts the requests that reached one provider.
type sentTo struct {
	untouched, changed int
	redacted, repaired int
	withheld           int
}

// add counts rec, and says whether janus changed it on the way.
func (t *sentTo) add(rec ledger.Record) {
	g := rec.Guard
	wasRedacted := g != nil && g.Redacted != nil && *g.Redacted > 0
	wasRepaired := slices.Contains(rec.Stages, backend.StageDropThinking)
	wasWithheld := g != nil && g.Withheld > 0
	if wasRedacted {
		t.redacted++
	}
	if wasRepaired {
		t.repaired++
	}
	if wasWithheld {
		t.withheld++
	}
	if wasRedacted || wasRepaired || wasWithheld {
		t.changed++
	} else {
		t.untouched++
	}
}

func (t sentTo) total() int { return t.untouched + t.changed }

// remotes are the providers off this machine a request was sent to, in
// order, whether or not they answered: "upstream" and "openai". A record
// from before the ledger had tried says so through its attempt.
func remotes(rec ledger.Record) []string {
	var sent []string
	add := func(provider string) {
		if (provider == "upstream" || provider == "openai") && !slices.Contains(sent, provider) {
			sent = append(sent, provider)
		}
	}
	if a := rec.Attempt; a != nil && len(rec.Tried) == 0 {
		add(a.Backend)
	}
	for _, t := range rec.Tried {
		if t.Outcome == ledger.OutcomeFailed {
			add(t.Provider)
		}
	}
	add(rec.Backend)
	return sent
}

// leftTheMachine reports whether a request was sent to any provider off
// this machine.
func leftTheMachine(rec ledger.Record) bool { return len(remotes(rec)) > 0 }

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
		case leftTheMachine(rec):
			a.sent = append(a.sent, rec)
			for _, provider := range remotes(rec) {
				if provider == "openai" {
					a.openai.add(rec)
				} else {
					a.anthropic.add(rec)
				}
			}
		case rec.Backend == "local":
			a.localOnly++
		default:
			a.stopped++
		}

		if g == nil {
			continue
		}
		if len(g.LocalOnly) > 0 && !leftTheMachine(rec) {
			a.keptLocal++
		}
		if g.Blocked != "" {
			a.blocked = append(a.blocked, rec)
		}
		if g.Findings > 0 && leftTheMachine(rec) {
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

func printAudit(w io.Writer, p painter, dir string, a audit) {
	fmt.Fprintln(w, p.paint(bold, "Audit of "+dir))
	if a.requests == 0 {
		fmt.Fprintln(w, "No requests carrying content were recorded in this range.")
		return
	}
	const stamp = "2006-01-02 15:04"
	fmt.Fprintf(w, "%s to %s (local time)\n\n", a.from.Local().Format(stamp), a.to.Local().Format(stamp))

	// Until a request reaches OpenAI, the report reads as it always has.
	remote, kept := "Anthropic", "kept from Anthropic"
	if a.openai.total() > 0 {
		remote, kept = "Anthropic or OpenAI", "kept off this machine"
	}
	fmt.Fprintf(w, "Where %s requests carrying your content went\n", commas(a.requests))
	printSentTo(w, "Anthropic", a.anthropic)
	if a.openai.total() > 0 {
		printSentTo(w, "OpenAI", a.openai)
	}
	fmt.Fprintf(w, "  %6s  to the local model only, never to %s\n", commas(a.localOnly), remote)
	fmt.Fprintf(w, "  %6s  refused or stopped by janus, and sent nowhere\n", commas(a.stopped))
	if a.openai.total() > 0 && a.anthropic.total() > 0 {
		fmt.Fprintf(w, "  A request sent to both, the first unable to answer, is counted under each.\n")
	}

	if withheld := a.anthropic.withheld + a.openai.withheld; withheld > 0 || a.keptLocal > 0 {
		fmt.Fprintf(w, "\nLocal-only content\n")
		fmt.Fprintf(w, "  %6s  requests went to %s with it replaced by a notice\n", commas(withheld), remote)
		fmt.Fprintf(w, "  %6s  requests were %s because of it\n", commas(a.keptLocal), kept)
	}

	fmt.Fprintf(w, "\nSecrets\n")
	if a.carried == 0 {
		fmt.Fprintf(w, "  None were found in anything bound for %s. That means the guard found\n"+
			"  none, or was off; it is not proof there were none. Run with -verify to look again.\n", remote)
	} else {
		kinds := make([]string, 0, len(a.kinds))
		for kind, n := range a.kinds {
			kinds = append(kinds, fmt.Sprintf("%s %s", kind, commas(n)))
		}
		sort.Strings(kinds)
		fmt.Fprintf(w, "  %s requests bound for %s carried something that looks like a secret\n  (%s).\n", commas(a.carried), remote, strings.Join(kinds, ", "))
		fmt.Fprintf(w, "  %6s  had them replaced with placeholders before sending\n", commas(a.replaced))
		exposed := fmt.Sprintf("  %6s  were SENT WITH THE SECRETS IN THEM", commas(len(a.exposed)))
		if len(a.exposed) > 0 {
			exposed = p.paint(red, exposed)
		}
		fmt.Fprintln(w, exposed)
		for _, rec := range a.exposed {
			fmt.Fprintf(w, "            %s  %s  in %s\n", p.paint(red, rec.ID), kindList(rec.Guard.Kinds), strings.Join(rec.Guard.Where, "; "))
		}
		if len(a.exposed) > 0 {
			fmt.Fprintln(w, p.paint(red, "  Treat those secrets as disclosed and rotate them.")+" Set guard.redact to stop it\n  happening again.")
		}
	}
	if len(a.blocked) > 0 {
		fmt.Fprintf(w, "\nStopped by the guard\n")
		for _, rec := range a.blocked {
			fmt.Fprintf(w, "  %s  %s\n", rec.ID, rec.Guard.Blocked)
		}
	}
}

// printSentTo prints the lines for the requests that reached one provider.
func printSentTo(w io.Writer, name string, t sentTo) {
	fmt.Fprintf(w, "  %6s  to %s, exactly as your client sent them\n", commas(t.untouched), name)
	fmt.Fprintf(w, "  %6s  to %s, changed by janus on the way", commas(t.changed), name)
	if t.changed > 0 {
		var how []string
		if t.redacted > 0 {
			how = append(how, fmt.Sprintf("%s with secrets replaced", commas(t.redacted)))
		}
		if t.withheld > 0 {
			how = append(how, fmt.Sprintf("%s with local-only content withheld", commas(t.withheld)))
		}
		if t.repaired > 0 {
			how = append(how, fmt.Sprintf("%s with a local model's thinking removed", commas(t.repaired)))
		}
		fmt.Fprintf(w, " (%s)", strings.Join(how, ", "))
	}
	fmt.Fprintln(w)
}

func kindList(kinds map[string]int) string {
	return strings.Join(sortedKinds(kinds), ", ")
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

// verification is what -verify found.
type verification struct {
	checked, clean, unchecked int
	leaks                     []leak
}

// leak is a request sent off this machine that still carries something
// that looks like a secret.
type leak struct {
	id     string
	report guard.Report
}

// verifySent re-reads every saved request that left this machine and
// looks for secrets in it, with the detector as it is configured now.
//
// This is the check that does not take the ledger's word for it: it looks
// at the bytes that were sent. What OpenAI was sent is a Responses
// request, which is read with scanText; every other is a Messages request.
func verifySent(sent []ledger.Record, captures string, scan func(*anthropic.Request) guard.Report, scanText func(string) guard.Report) *verification {
	v := &verification{checked: len(sent)}
	for _, rec := range sent {
		path, ok := sentPath(captures, rec)
		if !ok {
			v.unchecked++
			continue
		}
		data, err := os.ReadFile(path) // #nosec G304 -- a capture the ledger names, under the captures directory
		if err != nil {
			v.unchecked++
			continue
		}
		var report guard.Report
		if rec.Backend == "openai" && strings.HasSuffix(path, ".sent.txt") {
			strs, ok := jsonStrings(data)
			if !ok {
				v.unchecked++
				continue
			}
			report = scanText(strings.Join(strs, "\n"))
		} else {
			req, err := anthropic.ParseRequest(data)
			if err != nil {
				v.unchecked++
				continue
			}
			report = scan(req)
		}
		if report.Findings == 0 {
			v.clean++
			continue
		}
		v.leaks = append(v.leaks, leak{id: rec.ID, report: report})
	}
	return v
}

// jsonStrings is every string in a JSON document, keys aside, each as it
// reads once decoded: the text of a Responses request, wherever in it
// the text is.
func jsonStrings(data []byte) ([]string, bool) {
	var v any
	if json.Unmarshal(data, &v) != nil {
		return nil, false
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			out = append(out, v)
		case []any:
			for _, e := range v {
				walk(e)
			}
		case map[string]any:
			for _, e := range v {
				walk(e)
			}
		}
	}
	walk(v)
	return out, true
}

func printVerification(w io.Writer, p painter, v *verification, remote string) {
	fmt.Fprintf(w, "\nVerification: re-read %s requests as they were sent to %s\n", commas(v.checked), remote)
	fmt.Fprintf(w, "  %6s  contain nothing that looks like a secret\n", commas(v.clean))
	still := fmt.Sprintf("  %6s  STILL CONTAIN SOMETHING THAT LOOKS LIKE A SECRET", commas(len(v.leaks)))
	if len(v.leaks) > 0 {
		still = p.paint(red, still)
	}
	fmt.Fprintln(w, still)
	for _, l := range v.leaks {
		fmt.Fprintf(w, "            %s  %s  in %s\n", p.paint(red, l.id), l.report.Summary(), strings.Join(l.report.Where, "; "))
	}
	fmt.Fprintf(w, "  %6s  could not be checked: no capture was saved, or it has been pruned\n", commas(v.unchecked))
	if v.unchecked > 0 {
		fmt.Fprintf(w, "  Captures have to be on (logging.capture) for a request to be checked later.\n")
	}
	fmt.Fprintf(w, "  This uses the detector as configured now, so it also finds what a pattern\n  added since would have caught. It cannot find what no detector recognises.\n")
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
		if len(wentTo(*rec)) == 0 {
			fmt.Fprintf(stderr, "janus audit: request %s was not sent anywhere: %s\n", id, rec.Reason)
			return 1
		}
		if !saved {
			fmt.Fprintf(stderr, "janus audit: no capture of request %s was saved, or it has been pruned\n", id)
			return 1
		}
		data, err := os.ReadFile(path) // #nosec G304 -- a capture the ledger names, under the captures directory
		if err != nil {
			fmt.Fprintf(stderr, "janus audit: %v\n", err)
			return 1
		}
		if _, err := stdout.Write(data); err != nil {
			fmt.Fprintf(stderr, "janus audit: %v\n", err)
			return 1
		}
		return 0
	}

	fmt.Fprintf(stdout, "Request %s\n", rec.ID)
	fmt.Fprintf(stdout, "  When      %s\n", rec.Time.Local().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(stdout, "  What      %s %s, class %s\n", rec.Method, rec.Path, rec.Class)
	switch went := wentTo(*rec); {
	case len(went) == 0:
		fmt.Fprintf(stdout, "  Went to   nowhere: janus refused or stopped it\n")
	case len(went) == 1 && went[0] == "local":
		fmt.Fprintf(stdout, "  Went to   the local model only; it was not sent off this machine\n")
	default:
		names := make([]string, len(went))
		for i, provider := range went {
			names[i] = providerName(provider)
		}
		fmt.Fprintf(stdout, "  Went to   %s\n", strings.Join(names, ", which could not answer; then "))
	}
	fmt.Fprintf(stdout, "  Why       %s\n", rec.Reason)
	if len(rec.Stages) > 0 {
		fmt.Fprintf(stdout, "  Changed   %s\n", strings.Join(rec.Stages, ", "))
	}
	// Why the local model did not answer this request, if it was meant to:
	// tried first and failed (an attempt), or passed over without trying.
	for _, t := range rec.Tried {
		if t.Outcome == ledger.OutcomePassedOver {
			fmt.Fprintf(stdout, "  Passed    %s, not sent: %s\n", providerName(t.Provider), t.Why)
		}
	}
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
			pieces := "pieces"
			if g.Withheld == 1 {
				pieces = "piece"
			}
			fmt.Fprintf(stdout, "  Local-only %d %s withheld before sending: %s\n", g.Withheld, pieces, strings.Join(g.LocalOnly, "; "))
		default:
			fmt.Fprintf(stdout, "  Local-only %s\n", strings.Join(g.LocalOnly, "; "))
		}
	}
	if g := rec.Guard; g != nil && (g.Blocked != "" || g.Findings > 0 || g.Redacted != nil) {
		switch {
		case g.Blocked != "":
			fmt.Fprintf(stdout, "  Guard     stopped: %s\n", g.Blocked)
		case g.Findings == 0:
			// Checked, with redaction on, and nothing was there (#188).
			fmt.Fprintf(stdout, "  Guard     no secrets found\n")
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

// wentTo is every provider a request was sent to, in order, the one that
// answered last.
func wentTo(rec ledger.Record) []string {
	var went []string
	if a := rec.Attempt; a != nil && len(rec.Tried) == 0 && a.Backend != rec.Backend && a.Verdict != ledger.VerdictAccepted {
		went = append(went, a.Backend) // a record from before the ledger had tried
	}
	for _, t := range rec.Tried {
		if t.Outcome == ledger.OutcomeFailed {
			went = append(went, t.Provider)
		}
	}
	if rec.Backend != "none" && rec.Backend != "" {
		went = append(went, rec.Backend)
	}
	return went
}

// providerName names a provider for the audit.
func providerName(provider string) string {
	switch provider {
	case "upstream":
		return "Anthropic"
	case "openai":
		return "OpenAI"
	case "local":
		return "the local model"
	}
	return provider
}
