package ledger

import (
	"net/http"
	"strings"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/openai"
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
	// ClaudeCode is the Claude Code release that sent the request, from
	// its User-Agent, so that janus doctor can say when it is newer than
	// any janus was tested with (#267).
	ClaudeCode string `json:"claude_code,omitempty"`

	Method string `json:"method"`
	Path   string `json:"path"`
	Class  string `json:"class"`

	// The decision: where the request was sent ("upstream", "openai", or
	// "local"), the rule that sent it there, the changes made to it on the
	// way, and which backend answered ("none" if janus refused it itself).
	Route   string   `json:"route"`
	Reason  string   `json:"reason"`
	Stages  []string `json:"stages"`
	Backend string   `json:"backend"`

	Status int `json:"status"`
	// Outcome is one of the Outcome constants.
	Outcome string `json:"outcome"`
	// Error is the API's error type when Outcome is api_error.
	Error string `json:"error,omitempty"`
	// Limits are the response's headers about rate limits and retrying,
	// for a rate limit or a server error: what the API said about when
	// it would answer again.
	Limits     map[string]string `json:"limits,omitempty"`
	TTFBMs     int64             `json:"ttfb_ms"`
	DurationMs int64             `json:"duration_ms"`

	Request  *Request  `json:"request,omitempty"`
	Response *Response `json:"response,omitempty"`
	Usage    *Usage    `json:"usage,omitempty"`
	// Quota is what Claude's response said about the subscription's
	// limits: how much of each is used and when it resets (#238).
	Quota *anthropic.Quota `json:"quota,omitempty"`
	// Handback is present on Claude's first answer to a conversation's
	// main loop after the local model stood in for it (#241).
	Handback *Handback `json:"handback,omitempty"`
	// Unread says why a Messages response has no usage.
	Unread string `json:"unread,omitempty"`
	// Fault is what went wrong getting the response from the backend: it
	// could not be reached or did not answer in time, or its response
	// broke off part-way.
	Fault string `json:"fault,omitempty"`
	// Capture is the capture's path under the captures directory, without
	// extension, when one was written.
	Capture string `json:"capture,omitempty"`
	// Attempt is present when one backend was tried and another may have
	// answered: the local model first with Claude as the fallback, or
	// Claude first with the local model standing in when Claude was down.
	Attempt *Attempt `json:"attempt,omitempty"`
	// Tried is present when a rule's fallback list moved past a provider:
	// every one before the provider that answered, in order, and why it
	// did not (#429). When none answered, it ends with the last that
	// could not.
	Tried []Tried `json:"tried,omitempty"`
	// LocalCause is present when a rule or a stand-in meant this request
	// for the local model and it was never tried there: why, with the same
	// names as Attempt.Cause, plus skipped (the local backend had failed
	// repeatedly and was being left alone for a while).
	LocalCause string `json:"local_cause,omitempty"`
	// Guard is present when the guard found secrets or local-only content
	// in a request, or stopped it. It says how many and of what kinds,
	// never what they were.
	Guard *Guard `json:"guard,omitempty"`
	// Worker is present on a record of delegated work: a tool Claude called
	// that the local model carried out. Such a record is not a proxied
	// request, and is kept out of the request totals.
	Worker *Worker `json:"worker,omitempty"`
	// ShadowOf marks a shadow run: the same request answered a second time
	// by the local model, for comparison. It holds the id of the real
	// request. Shadow records are kept out of every total; they did not
	// serve anyone.
	ShadowOf string `json:"shadow_of,omitempty"`
	// Compare sets a shadow's answer beside the real one.
	Compare *Compare `json:"compare,omitempty"`
	// Transforms is present when a transform rewrote the request on its
	// way upstream, or declined to: one entry per transform.
	Transforms []Transform `json:"transforms,omitempty"`
}

// Transform is what one transform did to one request, and what the
// net-savings guard made of it. Tokens are estimated from the size of the
// JSON; dollars are at list price, and absent for an unpriced model.
type Transform struct {
	Name string `json:"name"`
	// Blocks carry the transform's output; of them, Rewritten were
	// rewritten for the first time and Revised were already sent and
	// changed. Declined counts answers not used: one that was not smaller,
	// or a revision the guard expected to cost more than it saves.
	Blocks    int `json:"blocks"`
	Rewritten int `json:"rewritten,omitempty"`
	Revised   int `json:"revised,omitempty"`
	Declined  int `json:"declined,omitempty"`
	// Tokens is about how many fewer tokens the request sent, SavedUSD
	// what they would have cost on it, and CostUSD what revising cost:
	// cache written again where it would have been read.
	Tokens   int     `json:"tokens"`
	SavedUSD float64 `json:"saved_usd"`
	CostUSD  float64 `json:"cost_usd,omitempty"`
	// ExpectedUSD is the guard's estimate, when it decided, of what the
	// rewrites and revisions made on this request will save over the rest
	// of the conversation, net of cost.
	ExpectedUSD float64 `json:"expected_usd,omitempty"`
	Unpriced    bool    `json:"unpriced,omitempty"`
}

// Guard is what the search for secrets found in one request.
type Guard struct {
	Findings int            `json:"findings"`
	Kinds    map[string]int `json:"kinds"`
	// Where lists the parts of the request that held at least one.
	Where []string `json:"where"`
	// Redacted is how many distinct secrets were replaced with
	// placeholders before the request was sent. Absent in detect-only
	// mode, where nothing is replaced and the secrets went upstream.
	Redacted *int `json:"redacted,omitempty"`
	// Restored is how many placeholders were turned back into secrets in
	// the answer.
	Restored int `json:"restored,omitempty"`
	// LocalOnly says why the request held content the local-only policy
	// keeps on this machine, and Withheld how many pieces of it were
	// replaced with a notice in what was sent upstream. Local-only content
	// with nothing withheld means the request did not go upstream at all.
	LocalOnly []string `json:"local_only,omitempty"`
	Withheld  int      `json:"withheld,omitempty"`
	// Blocked says why the exchange was stopped, if it was.
	Blocked string `json:"blocked,omitempty"`
}

// Worker describes one piece of delegated work.
type Worker struct {
	Tool string `json:"tool"`
	// Files and ReadBytes are what the local model read on Claude's
	// behalf; ModelCalls is how many passes it took.
	Files      int `json:"files"`
	ReadBytes  int `json:"read_bytes"`
	ModelCalls int `json:"model_calls"`
	// AnswerBytes is what went back to Claude.
	AnswerBytes int `json:"answer_bytes"`
	// AvoidedTokens is the most the call could have saved Claude; see the
	// AvoidedTokens function for how it is worked out and why it is a
	// ceiling.
	AvoidedTokens int `json:"avoided_tokens"`
}

// AvoidedTokens is what reading the files in full would have cost Claude in
// input tokens, less what the answer cost it. The arithmetic errs low on
// both sides: files are counted at four bytes a token and the answer at
// three.
//
// The assumption does not err low. It supposes Claude would otherwise have
// read every byte, and Claude frequently searches instead. In one measured
// case the figure was 63,000 tokens and the real saving was about 4,000.
// Treat it as the most that could have been saved.
func AvoidedTokens(readBytes, answerBytes int) int {
	return max(0, readBytes/4-(answerBytes+2)/3)
}

// OutcomeToolError is the outcome of delegated work that did not produce
// an answer.
const OutcomeToolError = "tool_error"

// Attempt is what became of trying the local model first.
type Attempt struct {
	// Backend is which backend was tried first: "local" for local-first
	// routing, or "upstream" when Claude could not answer and the local
	// model stood in.
	Backend string `json:"backend"`
	Model   string `json:"model,omitempty"`
	// Verdict is "accepted" (the local answer was given to the client, and
	// this record's response and usage are the local model's), "rejected"
	// (a whole local answer came back and failed a check; Claude
	// answered), or "failed" (no usable answer came back at all: the model
	// was unreachable, slow, too small, or answered with an error; Claude
	// answered).
	Verdict string `json:"verdict"`
	// Why says what was wrong, for a rejected or failed attempt.
	Why string `json:"why,omitempty"`
	// Cause is why a local attempt was not accepted, as a fixed name:
	// unreachable, model_missing, load_failed, too_large, busy, slow,
	// server_error, broke_off, rejected, or unsupported (#202).
	Cause string `json:"cause,omitempty"`
	// Check is the check a rejected answer failed: empty, truncated,
	// tools, schema, refusal, malformed, or oversize.
	Check      string `json:"check,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	// Usage is what a rejected attempt spent on the local model. For an
	// accepted one, see the record's own usage.
	Usage *Usage `json:"usage,omitempty"`
	// Limits are, for an upstream that answered with a rate limit or a
	// server error, its headers about rate limits and retrying.
	Limits map[string]string `json:"limits,omitempty"`
}

// Tried is one provider on a rule's fallback list that did not answer.
type Tried struct {
	// Provider is "upstream", "openai", or "local".
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	// Outcome is "failed" (it was sent the request and did not answer it)
	// or "passed_over" (it could not take the request, and was not sent
	// it).
	Outcome string `json:"outcome"`
	Why     string `json:"why"`
	// Cause is why a local model did not answer, as for Attempt.Cause.
	Cause string `json:"cause,omitempty"`
}

// Outcomes of a provider tried.
const (
	OutcomeFailed     = "failed"
	OutcomePassedOver = "passed_over"
)

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
	// Dropped names what the request asked for that the provider that
	// answered could not be sent, such as stop_sequences for OpenAI.
	Dropped []string `json:"dropped,omitempty"`
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

// Limits picks out of h the headers that say when the API will answer
// again: Retry-After, X-Should-Retry, and every Anthropic-Ratelimit-*
// header, by lower-case name. It is nil if there are none. They are kept
// to learn whether they tell a rate limit that passes in seconds from one
// that lasts hours, which would decide when a local stand-in should
// answer (issue #150).
func Limits(h http.Header) map[string]string {
	var out map[string]string
	for k, v := range h {
		name := strings.ToLower(k)
		if name != "retry-after" && name != "x-should-retry" && !strings.HasPrefix(name, "anthropic-ratelimit-") {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[name] = strings.Join(v, ", ")
	}
	return out
}

// Handback says how many main-loop turns the local model, and OpenAI,
// gave in Claude's place, from when, before Claude answered the
// conversation again.
type Handback struct {
	LocalTurns int `json:"local_turns"`
	// OpenAITurns are the main-loop turns OpenAI gave meanwhile (#378).
	OpenAITurns int       `json:"openai_turns,omitempty"`
	Since       time.Time `json:"since"`
}

// FromExchange builds the record for one proxied request.
func FromExchange(ex proxy.Exchange) Record {
	rec := Record{
		V:          SchemaVersion,
		ID:         ex.ID,
		Time:       ex.Start.UTC(),
		Session:    ex.Header.Get("X-Claude-Code-Session-Id"),
		Agent:      ex.Header.Get("X-Claude-Code-Agent-Id"),
		ClaudeCode: route.ClaudeCodeVersion(ex.Header),
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
		LocalCause: ex.LocalCause,
	}

	if rec.Stages == nil {
		rec.Stages = []string{}
	}
	if route.UpstreamDown(ex.Status) {
		rec.Limits = Limits(ex.ResponseHeader)
	}
	switch ex.Backend {
	case "upstream":
		rec.Quota = anthropic.QuotaFrom(ex.ResponseHeader)
	case "openai":
		rec.Quota = openai.QuotaFrom(ex.ResponseHeader, ex.Start.Add(ex.Duration))
	}
	for _, e := range ex.Transforms {
		rec.Transforms = append(rec.Transforms, Transform{
			Name: e.Name, Blocks: e.Blocks, Rewritten: e.Rewritten, Revised: e.Revised, Declined: e.Declined,
			Tokens: e.Tokens, SavedUSD: e.SavedUSD, CostUSD: e.CostUSD, ExpectedUSD: e.ExpectedUSD, Unpriced: e.Unpriced,
		})
	}

	if a := ex.Attempt; a != nil {
		rec.Attempt = &Attempt{Backend: a.Backend, Model: a.Model, Why: a.Why, Cause: a.Cause, DurationMs: a.Duration.Milliseconds(),
			Limits: Limits(a.ResponseHeader)}
		// When Claude was tried first and the local model stood in,
		// Claude's answer is still the latest word on the limits.
		if a.Backend == "upstream" && rec.Quota == nil {
			rec.Quota = anthropic.QuotaFrom(a.ResponseHeader)
		}
		switch {
		case a.Accepted:
			rec.Attempt.Verdict, rec.Attempt.Why, rec.Attempt.Cause = VerdictAccepted, "", ""
		case a.Cause == proxy.CauseRejected, a.Cause == "" && !a.Fault:
			// Only an answer that came back whole and was judged is
			// rejected. An error, a timeout, or a broken answer is a
			// failure, whatever it looked like on the wire (#202).
			rec.Attempt.Verdict, rec.Attempt.Check = VerdictRejected, a.Check
		default:
			rec.Attempt.Verdict = VerdictFailed
		}
		if !a.Accepted && a.Summary != nil && a.Summary.Error == nil {
			u := a.Summary.Usage
			rec.Attempt.Usage = &Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}
			if u.CacheRead != nil && *u.CacheRead <= u.Input {
				rec.Attempt.Usage.Input = u.Input - *u.CacheRead // as for any local answer
			}
		}
	}

	for _, step := range ex.Tried {
		t := Tried{Provider: step.Backend, Model: step.Model, Outcome: OutcomePassedOver, Why: step.Why, Cause: step.Cause}
		if step.Sent {
			t.Outcome = OutcomeFailed
		}
		rec.Tried = append(rec.Tried, t)
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
			if t := ex.Translation; t != nil {
				rec.Request.Dropped = t.Dropped
			}
			if ex.Path == "/v1/messages" {
				rec.Class = string(route.ClassifyRequest(ex.Header, req))
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
			Thinking: firstInt(u.Thinking, reasoning(ex)),
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

// reasoning is what a translating backend said its answer spent
// reasoning, which OpenAI reports and Anthropic's usage has no field for.
func reasoning(ex proxy.Exchange) *int {
	if t := ex.Translation; t != nil {
		return t.Reasoning
	}
	return nil
}

func firstInt(values ...*int) *int {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}
