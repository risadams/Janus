// replay.go is `janus replay`: it runs saved requests on the local model
// and compares its answers with the ones Claude gave.

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/capture"
	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/ledger"
	"github.com/risadams/janus/internal/shadow"
)

func runReplay(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("janus replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: janus replay [flags] <capture file or directory>...")
		fmt.Fprintln(stderr, "\nRuns saved Messages requests on a local model and compares each answer with")
		fmt.Fprintln(stderr, "the one Claude gave. Nothing is sent upstream. Each answer is saved beside its")
		fmt.Fprintln(stderr, "capture as .replay.md and .replay.response.txt.")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", config.DefaultPath, "path to the config file")
	modelName := fs.String("local", "", "which entry of local.models to replay on (default: the only one)")
	class := fs.String("class", "", "only requests of this class: main, subagent, or unknown")
	limit := fs.Int("limit", 0, "stop after this many requests (0 means all)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	switch *class {
	case "", "main", "subagent", "unknown":
	default:
		fmt.Fprintf(stderr, "janus replay: -class %q is not one of main, subagent, unknown\n", *class)
		return 2
	}

	cfg, err := config.Read(*configPath)
	if err == nil {
		cfg = config.Apply(cfg, config.Overrides{}, os.Getenv)
		err = cfg.Validate()
	}
	if err != nil {
		fmt.Fprintf(stderr, "janus replay: %v\n", err)
		return 1
	}
	name, model, err := pickLocalModel(cfg, *modelName)
	if err != nil {
		fmt.Fprintf(stderr, "janus replay: %v\n", err)
		return 2
	}

	requests, err := findRequests(fs.Args())
	if err != nil {
		fmt.Fprintf(stderr, "janus replay: %v\n", err)
		return 1
	}
	if len(requests) == 0 {
		fmt.Fprintln(stdout, "No captured Messages requests found there. Captures are files ending _v1-messages.request.txt.")
		return 0
	}

	localURL, _ := url.Parse(cfg.Local.BaseURL)
	local := backend.NewLocal(localURL, nil)
	seconds := model.TimeoutSeconds
	if seconds == 0 {
		seconds = config.DefaultLocalTimeoutSeconds
	}

	// Ctrl+C stops after the request in progress is abandoned, and still
	// prints the summary of what was done.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fmt.Fprintf(stdout, "Replaying on %s (%s) at %s\n\n", model.Model, name, cfg.Local.BaseURL)
	var outcomes []replayOutcome
	for _, path := range requests {
		if ctx.Err() != nil || (*limit > 0 && len(outcomes) >= *limit) {
			break
		}
		if o, ran := replayOne(ctx, local, path, model.Model, model.ContextTokens, time.Duration(seconds)*time.Second, *class, stdout); ran {
			outcomes = append(outcomes, o)
		}
	}
	if ctx.Err() != nil {
		fmt.Fprintln(stdout, "\nInterrupted.")
	}
	if len(outcomes) == 0 {
		fmt.Fprintln(stdout, "No requests matched.")
		return 0
	}
	printReplaySummary(stdout, outcomes)
	return 0
}

// replayOutcome is how one replayed request came out.
type replayOutcome struct {
	class string
	// state is one of: skipped (never sent), failed (an error, cut off, or
	// empty), uncompared (answered, with no saved answer from Claude to set
	// it against), same, or differs.
	state string
	took  time.Duration
}

func printReplaySummary(w io.Writer, outcomes []replayOutcome) {
	type tally struct {
		n     map[string]int
		times []time.Duration
	}
	byClass := map[string]*tally{}
	var classes []string
	for _, o := range outcomes {
		if byClass[o.class] == nil {
			byClass[o.class] = &tally{n: map[string]int{}}
			classes = append(classes, o.class)
		}
		t := byClass[o.class]
		t.n["replayed"]++
		t.n[o.state]++
		if o.state == "same" || o.state == "differs" || o.state == "uncompared" {
			t.times = append(t.times, o.took)
		}
	}
	sort.Strings(classes)

	fmt.Fprintf(w, "\nSummary\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "  class\treplayed\tskipped\tfailed\tsame\tdiffers\tnot compared\tlocal time\t\n")
	for _, class := range classes {
		t := byClass[class]
		median := "-"
		if len(t.times) > 0 {
			sort.Slice(t.times, func(i, j int) bool { return t.times[i] < t.times[j] })
			median = fmt.Sprintf("%.1fs", t.times[len(t.times)/2].Seconds())
		}
		fmt.Fprintf(tw, "  %s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t\n", class,
			t.n["replayed"], t.n["skipped"], t.n["failed"], t.n["same"], t.n["differs"], t.n["uncompared"], median)
	}
	tw.Flush()
	fmt.Fprintf(w, "  \"same\" means the local model stopped for the same reason as Claude and called\n"+
		"  the same tools. It compares the shape of the answers, not whether they are right.\n"+
		"  Local time is the median. Each answer is saved beside its capture as .replay.md.\n")
}

// pickLocalModel resolves -local to an entry of local.models.
func pickLocalModel(cfg config.Config, name string) (string, config.LocalModel, error) {
	names := make([]string, 0, len(cfg.Local.Models))
	for n := range cfg.Local.Models {
		names = append(names, n)
	}
	sort.Strings(names)
	switch {
	case len(names) == 0:
		return "", config.LocalModel{}, fmt.Errorf(`local.models is empty; add an entry such as "janus-local": {"model": "<id of a model in LM Studio>"}`)
	case name == "" && len(names) == 1:
		return names[0], cfg.Local.Models[names[0]], nil
	case name == "":
		return "", config.LocalModel{}, fmt.Errorf("local.models has several entries; choose one with -local: %s", strings.Join(names, ", "))
	}
	model, ok := cfg.Local.Models[name]
	if !ok {
		return "", config.LocalModel{}, fmt.Errorf("-local %q is not an entry of local.models; it has: %s", name, strings.Join(names, ", "))
	}
	return name, model, nil
}

const requestSuffix = "_v1-messages.request.txt"

// findRequests lists the captured Messages requests named by, or under,
// the given paths, oldest first. Shadow and replay files are not requests.
func findRequests(paths []string) ([]string, error) {
	var found []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			found = append(found, p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(path, requestSuffix) {
				found = append(found, path)
			}
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(found)
	return found, nil
}

// replayOne runs one captured request and reports it on one line. ran is
// false if the request was filtered out or could not be read, in which
// case nothing was sent.
func replayOne(ctx context.Context, local *backend.Local, path, model string, contextTokens int, timeout time.Duration, onlyClass string, stdout io.Writer) (o replayOutcome, ran bool) {
	stem := strings.TrimSuffix(path, ".request.txt")
	id := strings.TrimSuffix(filepath.Base(path), requestSuffix)

	body, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(stdout, "%s  could not be read: %v\n", id, err)
		return o, false
	}
	req, err := anthropic.ParseRequest(body)
	if err != nil {
		fmt.Fprintf(stdout, "%s  is not a Messages request: %v\n", id, err)
		return o, false
	}
	header, _, _ := headerFor("", req)
	o.class = classOf(header)
	if onlyClass != "" && o.class != onlyClass {
		return o, false
	}
	say := func(format string, args ...any) {
		fmt.Fprintf(stdout, "%s  %-8s  %s\n", id, o.class, fmt.Sprintf(format, args...))
	}

	// Claude's answer, if the capture has it. Without it the local answer
	// is still saved, but there is nothing to compare it with.
	var real *anthropic.Summary
	if data, err := os.ReadFile(stem + ".response.txt"); err == nil && len(data) > 0 {
		if s := summarise(data); s.Complete && s.Error == nil {
			real = &s
		}
	}

	out := shadow.Run(ctx, &http.Client{}, local, shadow.Input{
		ID: id + "-replay", Body: body, Model: model, ContextTokens: contextTokens, Timeout: timeout, Reason: "replay",
	})
	switch {
	case out.Skipped != "":
		o.state = "skipped"
		say("skipped: %s", out.Skipped)
		return o, true
	case out.Err != nil:
		o.state = "failed"
		say("failed: %v", out.Err)
		return o, true
	}

	for ext, content := range map[string][]byte{".replay.response.txt": out.Exchange.ResponseBody, ".replay.md": []byte(capture.Render(*out.Exchange))} {
		if err := os.WriteFile(stem+ext, content, 0o600); err != nil {
			say("could not save %s: %v", ext, err)
		}
	}

	answer := out.Exchange.Summary
	o.took = out.Exchange.Duration
	took := fmt.Sprintf("%.1fs", o.took.Seconds())
	switch outcome := ledger.FromExchange(*out.Exchange).Outcome; {
	case outcome != ledger.OutcomeOK:
		o.state = "failed"
		say("%s after %s", outcome, took)
	case len(answer.Blocks) == 0:
		// Well-formed and empty: what a small model produces when its tool
		// call is lost on the way out.
		o.state = "failed"
		say("empty answer after %s", took)
	case real == nil:
		o.state = "uncompared"
		say("%s  local %s (no saved answer from Claude to compare with)", took, describe(answer.StopReason, answer.Tools))
	default:
		c := shadow.Compare(real.StopReason, real.Tools, 0, *answer)
		o.state = "differs"
		if c.StopMatch && c.ToolsMatch {
			o.state = "same"
		}
		say("%s  %s: local %s; Claude %s", took, o.state, describe(answer.StopReason, answer.Tools), describe(real.StopReason, real.Tools))
	}
	return o, true
}

func classOf(header http.Header) string {
	switch {
	case header.Get("X-Claude-Code-Agent-Id") != "":
		return "subagent"
	case header.Get("X-Claude-Code-Session-Id") != "":
		return "main"
	}
	return "unknown"
}

// summarise reads a saved response body, which is a stream or a JSON
// document; captures do not keep the content type, so it is told apart by
// how it starts.
func summarise(body []byte) anthropic.Summary {
	contentType := "application/json"
	if trimmed := strings.TrimSpace(string(body[:min(len(body), 64)])); strings.HasPrefix(trimmed, "event:") || strings.HasPrefix(trimmed, "data:") {
		contentType = "text/event-stream"
	}
	o := anthropic.NewObserver(contentType)
	o.Write(body)
	return o.Summary()
}

// describe puts an answer's shape in a few words: "tool_use (Read, Bash)".
func describe(stop string, tools []string) string {
	if stop == "" {
		stop = "no stop reason"
	}
	if len(tools) == 0 {
		return stop
	}
	return fmt.Sprintf("%s (%s)", stop, strings.Join(tools, ", "))
}
