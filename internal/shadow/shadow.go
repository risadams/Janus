// Package shadow runs a request a second time on the local model, after
// the real answer has gone out, and records how the two compare. It is how
// a class of request is judged before a rule is trusted to send it local.
//
// Nothing here can affect a client. A shadow starts only once the real
// exchange is finished, runs on its own goroutine, and its answer goes to
// the ledger and nowhere else.
package shadow

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/capture"
	"github.com/risadams/janus/internal/ledger"
	"github.com/risadams/janus/internal/proxy"
	"github.com/risadams/janus/internal/route"
)

// queueSize is how many shadows may wait. The local model answers one
// request at a time and slowly, so a burst of traffic will outrun it; past
// this the newest are dropped rather than left to pile up.
const queueSize = 16

// maxKept bounds how much of a shadow's response is held for the capture.
const maxKept = 32 << 20

// Runner runs shadows one at a time.
type Runner struct {
	local    *backend.Local
	ledger   *ledger.Ledger
	captures *capture.Writer // nil when capture is off
	logf     func(format string, args ...any)
	client   *http.Client

	queue  chan job
	ctx    context.Context
	cancel context.CancelFunc
	done   sync.WaitGroup
}

type job struct {
	ex  proxy.Exchange
	rec ledger.Record
}

// New starts a Runner. Close it to stop.
func New(local *backend.Local, led *ledger.Ledger, captures *capture.Writer, logf func(string, ...any)) *Runner {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{
		local: local, ledger: led, captures: captures, logf: logf,
		client: &http.Client{}, // no timeout of its own: each run sets a deadline
		queue:  make(chan job, queueSize),
		ctx:    ctx, cancel: cancel,
	}
	r.done.Add(1)
	go func() {
		defer r.done.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case j := <-r.queue:
				r.write(r.run(ctx, j), j)
			}
		}
	}()
	return r
}

// Close stops the runner. A shadow in progress is abandoned, and ones still
// waiting are dropped: they are measurements, not work anyone is owed.
func (r *Runner) Close() {
	r.cancel()
	r.done.Wait()
}

// Submit asks for a shadow of a finished exchange, whose ledger record is
// rec. It returns at once. Exchanges that did not ask for a shadow, or
// have no complete real answer to compare with, are ignored.
func (r *Runner) Submit(ex proxy.Exchange, rec ledger.Record) {
	if ex.Shadow == nil || ex.Summary == nil || ex.Summary.Error != nil || !ex.Summary.Complete {
		return
	}
	j := job{ex, rec}
	select {
	case r.queue <- j:
	default:
		r.write(r.skipped(j, time.Now(), fmt.Sprintf("the shadow queue is full (%d waiting)", queueSize)), j)
	}
}

// result is a finished shadow: its ledger record, and the exchange to
// capture, if it got as far as being sent.
type result struct {
	rec ledger.Record
	ex  *proxy.Exchange
}

func (r *Runner) write(res result, j job) {
	if err := r.ledger.Append(res.rec); err != nil {
		r.logf("janus: %v", err)
	}
	if res.ex != nil && r.captures != nil && j.rec.Capture != "" {
		if err := r.captures.WriteShadow(j.rec.Capture, *res.ex); err != nil {
			r.logf("janus: %v", err)
		}
	}
	line := fmt.Sprintf("shadow of %s on %s: %s", j.ex.ID, j.ex.Shadow.Model, res.rec.Outcome)
	switch {
	case res.rec.Outcome == ledger.OutcomeSkipped:
		line += " (" + res.rec.Reason + ")"
	case res.rec.Compare != nil:
		line += fmt.Sprintf(" in %s, stop %s, tools %s", (time.Duration(res.rec.DurationMs) * time.Millisecond).Round(100*time.Millisecond),
			matchWord(res.rec.Compare.StopMatch), matchWord(res.rec.Compare.ToolsMatch))
	}
	r.logf("%s", line)
}

func matchWord(match bool) string {
	if match {
		return "match"
	}
	return "differ"
}

// base is the record every shadow of j starts from: the real request's
// identity and shape, marked as a shadow.
func (r *Runner) base(j job, start time.Time) ledger.Record {
	return ledger.Record{
		V:        ledger.SchemaVersion,
		ID:       j.ex.ID + "-shadow",
		Time:     start.UTC(),
		Session:  j.rec.Session,
		Agent:    j.rec.Agent,
		Method:   j.rec.Method,
		Path:     j.rec.Path,
		Class:    j.rec.Class,
		Route:    route.RouteShadow,
		Reason:   j.ex.Shadow.Reason,
		Stages:   []string{},
		Backend:  "local",
		Request:  j.rec.Request,
		ShadowOf: j.ex.ID,
		Capture:  j.rec.Capture,
	}
}

func (r *Runner) skipped(j job, start time.Time, why string) result {
	rec := r.base(j, start)
	rec.Outcome = ledger.OutcomeSkipped
	rec.Reason += ": " + why
	return result{rec: rec}
}

func (r *Runner) run(ctx context.Context, j job) result {
	start := time.Now()
	shadow := j.ex.Shadow

	req, err := anthropic.ParseRequest(j.ex.RequestBody)
	if err != nil || j.ex.RequestTruncated {
		return r.skipped(j, start, "the request could not be read back")
	}
	stages, err := backend.Normalize(req, shadow.Model)
	if err != nil {
		return r.skipped(j, start, "the request cannot be adapted for the local model: "+err.Error())
	}
	body := req.Bytes()

	limit := shadow.ContextTokens
	info, found, err := r.local.Model(ctx, shadow.Model)
	switch {
	case err != nil:
		return r.skipped(j, start, fmt.Sprintf("could not reach LM Studio: %v", err))
	case !found:
		return r.skipped(j, start, fmt.Sprintf("LM Studio does not have local model %q", shadow.Model))
	case limit == 0:
		limit = info.LoadedContext
	}
	if estimate := route.EstimateTokens(body); limit > 0 && estimate > limit {
		return r.skipped(j, start, fmt.Sprintf("about %d tokens, more than the model's %d-token context", estimate, limit))
	}

	if shadow.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, shadow.Timeout)
		defer cancel()
	}
	out, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.local.URL().String(), "/")+"/v1/messages", bytes.NewReader(body))
	out.Header.Set("Content-Type", "application/json")
	out.Header.Set("Accept-Encoding", "identity")
	if v := j.ex.Header.Get("Anthropic-Version"); v != "" {
		out.Header.Set("Anthropic-Version", v)
	}

	// The exchange that will be captured: what was sent to the local model
	// and what came back. It carries no credentials, because none were sent.
	ex := proxy.Exchange{
		ID: j.ex.ID + "-shadow", Start: start, Method: http.MethodPost, Path: "/v1/messages",
		Header: out.Header.Clone(), RequestBody: body,
		Backend: "local", Route: route.RouteShadow, Reason: shadow.Reason, Stages: stages, BackendModel: shadow.Model,
	}
	rec := r.base(j, start)
	rec.Stages = stages

	resp, err := r.client.Do(out)
	if err != nil {
		ex.Duration = time.Since(start)
		rec.DurationMs = ex.Duration.Milliseconds()
		rec.Outcome = ledger.OutcomeNoResponse
		rec.Reason += ": " + err.Error()
		return result{rec, &ex}
	}
	defer resp.Body.Close()

	observer := anthropic.NewObserver(resp.Header.Get("Content-Type"))
	var kept bytes.Buffer
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if ex.TTFB == 0 {
				ex.TTFB = time.Since(start)
			}
			observer.Write(buf[:n])
			if kept.Len() < maxKept {
				kept.Write(buf[:n])
			}
		}
		if err != nil {
			if err != io.EOF {
				rec.Reason += ": " + err.Error()
			}
			break
		}
	}
	summary := observer.Summary()
	ex.Status, ex.Duration = resp.StatusCode, time.Since(start)
	ex.ResponseHeader, ex.ResponseBody, ex.Summary = resp.Header, kept.Bytes(), &summary

	// Fill in the outcome, response, and usage the same way a real local
	// exchange would be recorded, then keep the shadow's own identity.
	filled := ledger.FromExchange(ex)
	rec.Status, rec.Outcome, rec.Error = filled.Status, filled.Outcome, filled.Error
	rec.TTFBMs, rec.DurationMs = filled.TTFBMs, filled.DurationMs
	rec.Response, rec.Usage = filled.Response, filled.Usage

	if rec.Outcome == ledger.OutcomeOK {
		rec.Compare = compare(j.ex, summary)
	}
	return result{rec, &ex}
}

// compare sets the local answer beside the real one.
func compare(real proxy.Exchange, local anthropic.Summary) *ledger.Compare {
	c := &ledger.Compare{
		UpstreamStop:  real.Summary.StopReason,
		LocalStop:     local.StopReason,
		UpstreamTools: orEmpty(real.Summary.Tools),
		LocalTools:    orEmpty(local.Tools),
		UpstreamMs:    real.Duration.Milliseconds(),
	}
	c.StopMatch = c.UpstreamStop == c.LocalStop
	c.ToolsMatch = sameTools(c.UpstreamTools, c.LocalTools)
	return c
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// sameTools reports whether two answers called the same tools the same
// number of times. Order is ignored: a model that reads two files in the
// other order has done the same thing.
func sameTools(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
