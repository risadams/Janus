package ledger

import (
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/proxy"
	"github.com/risadams/janus/internal/route"
)

// SchemaVersion is written into every record as "v". It goes up when a
// field changes meaning or is removed; adding a field does not change it.
// The schema is documented in docs/LEDGER.md.
const SchemaVersion = 1

// Record is one line of the ledger: what one request was, what janus did
// with it, and what it cost. It holds no prompt text and no credentials.
type Record struct {
	V    int       `json:"v"`
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	// Session and Agent are Claude Code's own identifiers: one session per
	// run of the client, one agent per subagent within it.
	Session string `json:"session,omitempty"`
	Agent   string `json:"agent,omitempty"`

	Method string `json:"method"`
	Path   string `json:"path"`
	Class  string `json:"class"`

	// The decision: where the request was sent ("upstream" or "local"), the
	// rule that sent it there, the changes made to it on the way, and which
	// backend answered ("none" if janus refused it itself).
	Route   string   `json:"route"`
	Reason  string   `json:"reason"`
	Stages  []string `json:"stages"`
	Backend string   `json:"backend"`

	Status int `json:"status"`
	// Outcome is one of the Outcome constants.
	Outcome string `json:"outcome"`
	// Error is the API's error type when Outcome is api_error.
	Error      string `json:"error,omitempty"`
	TTFBMs     int64  `json:"ttfb_ms"`
	DurationMs int64  `json:"duration_ms"`

	Request  *Request  `json:"request,omitempty"`
	Response *Response `json:"response,omitempty"`
	Usage    *Usage    `json:"usage,omitempty"`
	// Unread says why a Messages response has no usage.
	Unread string `json:"unread,omitempty"`
	// Fault is what went wrong reading the response from the backend when
	// it broke off part-way.
	Fault string `json:"fault,omitempty"`
	// Capture is the capture's path under the captures directory, without
	// extension, when one was written.
	Capture string `json:"capture,omitempty"`
	// Attempt is present when the local model was tried for a request that
	// had Claude as a fallback: by a local-then-escalate rule, or by a
	// local rule whose send failed.
	Attempt *Attempt `json:"attempt,omitempty"`
	// ShadowOf marks a shadow run: the same request answered a second time
	// by the local model, for comparison. It holds the id of the real
	// request. Shadow records are kept out of every total; they did not
	// serve anyone.
	ShadowOf string `json:"shadow_of,omitempty"`
	// Compare sets a shadow's answer beside the real one.
	Compare *Compare `json:"compare,omitempty"`
}

// Attempt is what became of trying the local model first.
type Attempt struct {
	Model string `json:"model"`
	// Verdict is "accepted" (the local answer was given to the client, and
	// this record's response and usage are the local model's), "rejected"
	// (the local answer failed a check and Claude answered), or "failed"
	// (the local model was unavailable and Claude answered).
	Verdict string `json:"verdict"`
	// Why says what was wrong, for a rejected or failed attempt.
	Why        string `json:"why,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	// Usage is what a rejected attempt spent on the local model. For an
	// accepted one, see the record's own usage.
	Usage *Usage `json:"usage,omitempty"`
}

// Verdicts of an attempt.
const (
	VerdictAccepted = "accepted"
	VerdictRejected = "rejected"
	VerdictFailed   = "failed"
)

// Compare is how a shadow run's answer differed from the real one. It
// compares shape, not meaning: whether the two stopped for the same reason
// and reached for the same tools.
type Compare struct {
	UpstreamStop  string   `json:"upstream_stop"`
	LocalStop     string   `json:"local_stop"`
	UpstreamTools []string `json:"upstream_tools"`
	LocalTools    []string `json:"local_tools"`
	// StopMatch and ToolsMatch are the two indicators. Tools match when
	// both called the same tools the same number of times, in any order.
	StopMatch  bool `json:"stop_match"`
	ToolsMatch bool `json:"tools_match"`
	// UpstreamMs is how long the real answer took, to set beside this
	// record's own duration.
	UpstreamMs int64 `json:"upstream_ms"`
}

// Outcomes of a request.
const (
	OutcomeOK         = "ok"
	OutcomeAPIError   = "api_error"   // the API answered with an error object
	OutcomeHTTPError  = "http_error"  // a 4xx or 5xx janus could not read further
	OutcomeIncomplete = "incomplete"  // the response ended before it should have
	OutcomeCancelled  = "cancelled"   // the client went away
	OutcomeNoResponse = "no_response" // nothing came back from the backend
	OutcomeSkipped    = "skipped"     // a shadow run that was not attempted; Reason says why
)

// Request is what a Messages request contained, by size and shape only.
type Request struct {
	Model            string `json:"model"`
	Stream           bool   `json:"stream"`
	Bytes            int    `json:"bytes"`
	Messages         int    `json:"messages"`
	Tools            int    `json:"tools"`
	CacheBreakpoints int    `json:"cache_breakpoints"`
}

// Response is what a Messages response contained, by shape only.
type Response struct {
	Model      string   `json:"model,omitempty"`
	StopReason string   `json:"stop_reason,omitempty"`
	Blocks     []string `json:"blocks"`
	// Tools is the name of each tool called, in order.
	Tools []string `json:"tools,omitempty"`
}

// Usage is token usage split the way it is priced. A field the server did
// not report is left out, which is different from zero.
type Usage struct {
	Input        int  `json:"input"`
	Output       int  `json:"output"`
	CacheRead    *int `json:"cache_read,omitempty"`
	CacheWrite   *int `json:"cache_write,omitempty"`
	CacheWrite5m *int `json:"cache_write_5m,omitempty"`
	CacheWrite1h *int `json:"cache_write_1h,omitempty"`
	Thinking     *int `json:"thinking,omitempty"`
	// Speed is "fast" for a fast-mode request; left out otherwise.
	Speed string `json:"speed,omitempty"`
}

// FromExchange builds the record for one proxied request.
func FromExchange(ex proxy.Exchange) Record {
	rec := Record{
		V:          SchemaVersion,
		ID:         ex.ID,
		Time:       ex.Start.UTC(),
		Session:    ex.Header.Get("X-Claude-Code-Session-Id"),
		Agent:      ex.Header.Get("X-Claude-Code-Agent-Id"),
		Method:     ex.Method,
		Path:       ex.Path,
		Class:      string(route.Classify(ex.Method, ex.Path, ex.Header)),
		Route:      ex.Route,
		Reason:     ex.Reason,
		Stages:     ex.Stages,
		Backend:    ex.Backend,
		Status:     ex.Status,
		TTFBMs:     ex.TTFB.Milliseconds(),
		DurationMs: ex.Duration.Milliseconds(),
		Unread:     ex.Unread,
		Fault:      ex.Fault,
	}

	if rec.Stages == nil {
		rec.Stages = []string{}
	}

	if a := ex.Attempt; a != nil {
		rec.Attempt = &Attempt{Model: a.Model, Why: a.Why, DurationMs: a.Duration.Milliseconds()}
		switch {
		case a.Accepted:
			rec.Attempt.Verdict, rec.Attempt.Why = VerdictAccepted, ""
		case a.Fault:
			rec.Attempt.Verdict = VerdictFailed
		default:
			rec.Attempt.Verdict = VerdictRejected
		}
		if !a.Accepted && a.Summary != nil && a.Summary.Error == nil {
			u := a.Summary.Usage
			rec.Attempt.Usage = &Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}
			if u.CacheRead != nil && *u.CacheRead <= u.Input {
				rec.Attempt.Usage.Input = u.Input - *u.CacheRead // as for any local answer
			}
		}
	}

	if ex.IsMessages() && len(ex.RequestBody) > 0 && !ex.RequestTruncated {
		if req, err := anthropic.ParseRequest(ex.RequestBody); err == nil {
			rec.Request = &Request{
				Model:            req.Model(),
				Stream:           req.Stream(),
				Bytes:            len(ex.RequestBody),
				Messages:         len(req.Messages()),
				Tools:            len(req.Tools()),
				CacheBreakpoints: req.CacheBreakpoints(),
			}
		}
	}

	s := ex.Summary
	if s != nil && s.Error == nil {
		blocks := s.Blocks
		if blocks == nil {
			blocks = []string{}
		}
		rec.Response = &Response{Model: s.Model, StopReason: s.StopReason, Blocks: blocks, Tools: s.Tools}
		u := s.Usage
		rec.Usage = &Usage{
			Input: u.Input, Output: u.Output,
			CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
			CacheWrite5m: u.CacheWrite5m, CacheWrite1h: u.CacheWrite1h,
			Thinking: u.Thinking,
		}
		if u.Speed != "" && u.Speed != "standard" {
			rec.Usage.Speed = u.Speed
		}
		// LM Studio counts tokens read from its prompt cache inside
		// input_tokens, where the Anthropic API counts them apart
		// (docs/research/lmstudio.md). Take them out, so that input means
		// uncached input whichever backend answered.
		if ex.Backend == "local" && u.CacheRead != nil && *u.CacheRead <= u.Input {
			rec.Usage.Input = u.Input - *u.CacheRead
		}
	}

	switch {
	case ex.Cancelled:
		rec.Outcome = OutcomeCancelled
	case ex.Status == 0:
		rec.Outcome = OutcomeNoResponse
	case s != nil && s.Error != nil:
		rec.Outcome, rec.Error = OutcomeAPIError, s.Error.Type
	case ex.Status >= 400:
		rec.Outcome = OutcomeHTTPError
	case s != nil && !s.Complete:
		rec.Outcome = OutcomeIncomplete
	default:
		rec.Outcome = OutcomeOK
	}
	return rec
}
