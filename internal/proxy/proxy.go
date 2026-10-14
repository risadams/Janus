package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/guard"
	"github.com/risadams/janus/internal/transform"
)

// Exchange describes one request janus handled, reported when it is over.
type Exchange struct {
	// ID is unique to this request and sorts by time. Captures and ledger
	// records for the same request share it.
	ID     string
	Start  time.Time
	Method string
	Path   string
	// Header is the request's headers as the client sent them, credentials
	// included. Whoever receives an Exchange is responsible for not writing
	// those anywhere readable.
	Header http.Header
	// Status is the response status, or 0 if no response was started.
	Status int
	// TTFB is the time until the first byte of the response body arrived
	// from upstream; zero if there was none.
	TTFB     time.Duration
	Duration time.Duration
	// Cancelled means the client went away before the response finished.
	Cancelled bool

	// The decision: which backend the request went to, by what route, and
	// why; the changes made to it on the way; and the backend's own name
	// for the model, when that differs from the one the client asked for.
	Backend      string
	Route        string
	Reason       string
	Stages       []string
	BackendModel string
	// Transforms is what each transform did to the request on its way
	// upstream, or declined to do.
	Transforms []transform.Effect
	// Shadow is set when the request should also be run on a local model
	// for comparison. The proxy only carries it; whoever receives the
	// Exchange does the running.
	Shadow *Shadow
	// Guard is set when the guard examined this request, through the
	// redactor, the local-only policy, or both: what it found, and what it
	// did about it.
	Guard *Guard
	// Attempt is set when a plan with a fallback was tried: either its
	// answer was put on trial, or it failed and the fallback answered.
	Attempt *Attempt
	// BackendFault is true when the backend that was meant to answer could
	// not be reached or timed out, and there was no fallback.
	BackendFault bool

	// Summary is what janus read from a Messages API response as it went
	// past: model, stop reason, token usage. It is nil for every other kind
	// of request.
	Summary *anthropic.Summary
	// Unread says why a Messages response has no Summary, when that is the
	// case: for example a content encoding janus cannot decode. The
	// response was still relayed.
	Unread string
	// Fault is what went wrong getting the response from the backend: it
	// could not be reached or did not answer in time, and there was no
	// fallback; or its response broke off part-way, because the connection
	// dropped or the body did not match its own framing. Empty for a
	// response that arrived whole, and for one the client abandoned.
	Fault string
	// NoFallback says why a plan's fallback was not used when its backend
	// failed, and the failure was the client's answer: for a local
	// stand-in, why the local model did not answer instead.
	NoFallback string

	// RequestBody is kept for Messages requests always, and for everything
	// else only when Options.KeepBodies is set. ResponseBody is kept only
	// with KeepBodies; it is decompressed where janus can do that, and
	// otherwise the bytes as they arrived. When janus answered with an
	// error of its own, it is that error. Either may be cut short at
	// maxKept, which the Truncated flags report.
	RequestBody      []byte
	RequestTruncated bool
	// SentBody is the request as it was actually sent to the backend,
	// when that differs from RequestBody: after redaction, withholding,
	// repair, a transform, or adaptation for the local model. Nil means it
	// was sent as received.
	// Kept only with KeepBodies.
	SentBody          []byte
	ResponseHeader    http.Header
	ResponseBody      []byte
	ResponseTruncated bool
}

// Guard is what the redactor and the local-only policy did with one
// request.
type Guard struct {
	// Report is what was found, without the values.
	Report guard.Report
	// Replaced is how many distinct secrets were replaced with
	// placeholders before the request was sent; Restored is how many
	// placeholders were turned back into secrets in the response.
	Replaced int
	Restored int
	// Scanned is set when the redactor went over what was sent, which is
	// when Report and Replaced mean anything.
	Scanned bool
	// LocalOnly says why the request held local-only content, if it did,
	// and Withheld how many pieces of it were replaced with a notice in
	// what went upstream.
	LocalOnly []string
	Withheld  int
	// Blocked is set when the exchange was stopped: the request could not
	// be made safe to send, or the response referred to a secret in a form
	// that could not be restored.
	Blocked string
}

// Attempt is what became of a try at a backend that had a fallback.
type Attempt struct {
	Backend string
	Model   string
	Stages  []string
	// Accepted is true when the attempt's answer is the one the client
	// got. Otherwise the fallback answered, and Why says what was wrong.
	Accepted bool
	Why      string
	// Check is the validator that failed, when one did.
	Check string
	// Fault is true when the attempt failed because the backend was
	// unavailable, as opposed to giving an answer that was not good
	// enough. It is what a circuit breaker should count.
	Fault    bool
	Duration time.Duration
	// Summary and ResponseBody are the attempt's own answer. They are set
	// for a rejected trial, and ResponseBody for a backend that answered
	// with an error status; for an accepted attempt the Exchange itself
	// carries them.
	Summary      *anthropic.Summary
	ResponseBody []byte
	// ResponseHeader is the header of a backend's error response: for an
	// upstream that could not answer, what it said about when to retry.
	ResponseHeader http.Header
}

// IsMessages reports whether this was a call to the Messages API itself,
// as opposed to token counting, model listing, or anything else.
func (ex Exchange) IsMessages() bool {
	return ex.Method == http.MethodPost && ex.Path == "/v1/messages"
}

// Options configures the proxy handler.
type Options struct {
	// Upstream is where everything goes unless Plan says otherwise.
	Upstream backend.Backend
	// Plan decides what to do with a Messages request. Nil means every
	// request goes upstream untouched.
	Plan Planner
	// Done is called once per request, after the response has ended.
	Done func(Exchange)
	// Redactor, when set, replaces secrets with placeholders in every
	// Messages and count_tokens request sent to the upstream, and puts them
	// back in the response. A request it cannot make safe is not sent.
	Redactor *guard.Redactor
	// Policy, when set, marks some content local-only: it is never sent to
	// the upstream. What happens to a request holding such content is
	// PolicyAction. A request that cannot be checked against the policy is
	// not sent.
	Policy *guard.Policy
	// PolicyAction is "withhold" (the default): the content is replaced
	// with a notice in whatever goes upstream, and the request otherwise
	// proceeds as planned; or "local": the whole request is answered by
	// the local model, through LocalOnly, or refused if that cannot be done.
	PolicyAction string
	// LocalOnly plans a request that must stay local. Required for the
	// "local" action.
	LocalOnly Planner
	// KeepBodies makes every Exchange carry its request and response
	// bodies, for capture logging. Off, only Messages request bodies are
	// kept, which the ledger needs.
	KeepBodies bool
}

// Planner decides where one Messages request goes. It may change req; the
// plan it returns says what to send. A Refusal means janus answers the
// client itself, with an error.
type Planner func(ctx context.Context, header http.Header, req *anthropic.Request) (Plan, *Refusal)

// Plan is what to do with one request.
type Plan struct {
	Backend backend.Backend
	// Route and Reason are recorded in the ledger: where the request went
	// and the rule that sent it there.
	Route  string
	Reason string
	// Stages names the changes made to the request, in order.
	Stages []string
	// Transforms is what the transforms did to the request, and what the
	// net-savings guard made of it.
	Transforms []transform.Effect
	// Body replaces the request body. Nil sends the body as received.
	Body []byte
	// BackendModel is the model the backend was asked for, and
	// ResponseModel the name to report to the client in its place, when
	// the two differ.
	BackendModel  string
	ResponseModel string
	// Timeout bounds the whole exchange. Zero means no limit.
	Timeout time.Duration
	// Shadow, when set, asks for the request to also be run on a local
	// model afterwards, for comparison. It changes nothing about how the
	// request itself is handled.
	Shadow *Shadow

	// Else is the plan to fall back to if this one's backend fails before
	// any of its answer has reached the client: it cannot be reached, it
	// times out, or it answers with an error status. Nil means there is
	// no fallback and the failure is the client's answer.
	Else *Plan
	// Fallback is Else worked out only when it is needed, for a fallback
	// that costs something to plan. It reports false if there is nowhere
	// to fall back to after all, and the failure then stands; the plan it
	// returns then may have a Reason saying why, which is recorded. Else,
	// if set, takes precedence.
	Fallback func(ctx context.Context) (Plan, bool)
	// FallBackOn says which response statuses count as this plan's backend
	// failing. Nil means any error status, 400 and above.
	FallBackOn func(status int) bool
	// Release, when set, is called once when this plan's backend has
	// finished with the request: its answer relayed, its trial judged, or
	// the request abandoned or switched to a fallback. It is how a planner
	// that limits the requests in flight to a backend gets its place back.
	Release func()
	// Accept, when set, makes this plan a trial. The backend's whole
	// answer is held back and shown to Accept; the client gets it only if
	// Accept passes it, and otherwise gets the fallback's answer, never
	// knowing there were two. A trial needs Else or Fallback.
	Accept func(Fetched) (ok bool, why string)
}

// Shadow names the local model to compare an upstream answer against.
type Shadow struct {
	Model         string
	ContextTokens int
	Timeout       time.Duration
	// Reason is the rule that asked for it.
	Reason string
}

// Refusal is an error janus returns instead of forwarding a request.
type Refusal struct {
	Status  int
	Type    string // an Anthropic API error type, e.g. invalid_request_error
	Message string
	Route   string
	Reason  string
}

// maxKept bounds how much of one body janus holds in memory. Past it the
// traffic is still relayed in full; only the kept copy stops growing.
const maxKept = 32 << 20

// exchangeState carries what the response hook learns back to the handler
// that started the request.
// maxFailedBody is how much of an error response given up for a fallback
// is kept. An API error body is a few hundred bytes.
const maxFailedBody = 64 << 10

type exchangeState struct {
	messages       bool
	keep           bool
	plan           Plan
	observation    *observation
	unread         string
	responseHeader http.Header
	firstByte      time.Time
	fault          error

	attempt      *Attempt
	backendFault bool
	// failed is the error response given up in favour of the fallback,
	// kept for the attempt's record.
	failedHeader http.Header
	failedBody   []byte

	// redaction is what was replaced in the request now being sent, if it
	// is going upstream and anything was found. guard accumulates what to
	// report; restorer is the response stream being restored.
	redaction *guard.Redaction
	guard     *Guard
	restorer  *guard.Restorer
	// sent is the body last handed to a backend, if it was not the
	// client's own.
	sent []byte
	// withhold is set when local-only content must be kept out of anything
	// this request sends upstream.
	withhold bool
	// served is an answer given to the client from memory, by a trial
	// that passed, rather than relayed.
	served *Fetched
	// fallBack switches to the plan's fallback and serves it, reporting
	// false if there turned out to be none. It is set by the handler for
	// plans that may have one, and cleared once used.
	fallBack func(w http.ResponseWriter, why string, fault bool) bool
	// next is a fallback already worked out, by the response hook, which
	// has to know there is one before it throws a response away.
	next *Plan
	// resolve works out the plan's fallback.
	resolve func() (Plan, bool)
	// noFallback is why resolve found no fallback, if it said.
	noFallback string
	// release frees what the current plan holds, if anything; see
	// Plan.Release.
	release func()
}

// hold takes on what plan holds, to be freed by releaseNow.
func (s *exchangeState) hold(plan Plan) {
	s.release = plan.Release
}

// releaseNow frees what the current plan holds, if it has not been freed.
func (s *exchangeState) releaseNow() {
	if s.release != nil {
		release := s.release
		s.release = nil
		release()
	}
}

// failed reports whether a status from a plan's backend counts as the
// backend failing, so that the plan's fallback should be used.
func failed(plan Plan, status int) bool {
	if plan.FallBackOn != nil {
		return plan.FallBackOn(status)
	}
	return status >= 400
}

// errBackendStatus turns an error status from a backend with a fallback
// into a failure, so that the fallback is used.
type errBackendStatus int

func (e errBackendStatus) Error() string { return fmt.Sprintf("answered HTTP %d", int(e)) }

// trialLimit bounds the answer a trial will hold.
const trialLimit = 8 << 20

type stateKey struct{}

// New returns a handler that sends each request where its plan says, the
// upstream unless Options.Plan decides otherwise, and relays the response
// as it arrives. With no planner, redactor, or policy, nothing in either
// direction is altered beyond what HTTP requires of a proxy (hop-by-hop
// headers are dropped and Host is set to the upstream's) and what the
// backend's Prepare does, which for the upstream is one header on Messages
// requests. Credentials pass through as the client sent them.
//
// Traffic is also read on its way past, without being delayed or changed,
// so that Done can be told what happened.
func New(opts Options) http.Handler {
	// The default transport asks for gzip and decodes it when the client
	// did not ask for compression itself. Turning that off keeps the
	// response body exactly as the upstream sent it.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true

	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			state := r.In.Context().Value(stateKey{}).(*exchangeState)
			r.SetURL(state.plan.Backend.URL())
			state.plan.Backend.Prepare(r.Out, state.messages)
			if state.redaction.Active() {
				// The response has to be read and rewritten to put the
				// secrets back, so it must not arrive compressed.
				r.Out.Header.Set("Accept-Encoding", "identity")
			}
		},
		Transport: transport,
		// Flush after every write, so a streamed completion reaches the
		// client token by token rather than when a buffer fills.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			state, _ := resp.Request.Context().Value(stateKey{}).(*exchangeState)
			if state == nil {
				return nil
			}
			if state.fallBack != nil && failed(state.plan, resp.StatusCode) {
				// Only give up this response if there really is somewhere
				// else to go. Otherwise it is the answer, error and all.
				if next, ok := state.resolve(); ok {
					state.next = &next
					state.failedHeader = resp.Header.Clone()
					state.failedBody, _ = io.ReadAll(io.LimitReader(resp.Body, maxFailedBody))
					return errBackendStatus(resp.StatusCode)
				}
				state.fallBack = nil
				if state.noFallback != "" {
					state.plan.Reason = fmt.Sprintf("%s: %s answered HTTP %d; %s", state.plan.Reason, state.plan.Backend.Name(), resp.StatusCode, state.noFallback)
				}
			}
			if a := state.attempt; a != nil && !a.Accepted {
				// Say so on the response, for anything that looks: this
				// answer is not from the backend that was meant to give it.
				note := fmt.Sprintf("%s answered because %s", state.plan.Backend.Name(), a.Why)
				resp.Header.Set("X-Janus-Fallback", strings.Join(strings.Fields(note), " "))
			}
			state.responseHeader = resp.Header.Clone()
			if state.messages || state.keep {
				var unsupported bool
				state.observation, unsupported = newObservation(
					resp.Header.Get("Content-Type"), resp.Header.Get("Content-Encoding"), state.messages, state.keep)
				if unsupported && state.messages {
					state.unread = "content-encoding " + resp.Header.Get("Content-Encoding")
				}
			}
			resp.Body = &observedBody{ReadCloser: resp.Body, state: state}
			if rd := state.redaction; rd.Active() && state.messages && resp.StatusCode == http.StatusOK {
				// Restoring wraps the observed body: the ledger and the
				// capture see what the upstream really sent, placeholders
				// and all, and only the client gets the secrets back.
				if isEventStream(resp.Header) {
					state.restorer = rd.Restore(resp.Body)
					resp.Body = state.restorer
				} else {
					raw, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					restored, n, err := rd.RestoreJSON(raw)
					if err != nil {
						state.guard.Blocked = err.Error()
						restored = errorBody("api_error", fmt.Sprintf("janus: %v. The answer was withheld so that nothing acts on it; try again.", err))
						resp.StatusCode, resp.Status = http.StatusBadGateway, "502 Bad Gateway"
						resp.Header.Set("Content-Type", "application/json")
					}
					state.guard.Restored = n
					resp.Body = io.NopCloser(bytes.NewReader(restored))
					resp.ContentLength = int64(len(restored))
					resp.Header.Set("Content-Length", strconv.Itoa(len(restored)))
				}
			}
			// Renaming wraps the observed body, so the ledger sees the
			// backend's real model name and the client sees the one it
			// asked for.
			if state.messages && state.plan.ResponseModel != "" {
				resp.Body = backend.RenameModel(resp.Body, resp.Header.Get("Content-Type"), state.plan.ResponseModel)
				resp.Header.Del("Content-Length")
				resp.ContentLength = -1
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			state := r.Context().Value(stateKey{}).(*exchangeState)
			name := state.plan.Backend.Name()
			if errors.Is(err, context.Canceled) && r.Context().Err() == context.Canceled {
				// The client went away; there is nobody to answer.
				return
			}

			why := "could not reach " + name + ": " + err.Error()
			status, fault := http.StatusBadGateway, true
			var bad errBackendStatus
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				why = fmt.Sprintf("%s did not answer within %s", name, state.plan.Timeout)
				status = http.StatusGatewayTimeout
			case errors.As(err, &bad):
				why = name + " " + bad.Error()
				// A rate limit is the backend being unavailable too; any
				// other 4xx is about this one request.
				fault = int(bad) >= 500 || int(bad) == http.StatusTooManyRequests
			}
			if state.fallBack != nil {
				// Nothing has been written yet: the error handler runs
				// before any of the response is relayed.
				if state.fallBack(w, why, fault) {
					return
				}
			}
			state.backendFault = fault
			state.fault = errors.New(why)
			writeError(w, status, "api_error", "janus: "+why)
		},
	}
	client := &http.Client{Transport: transport}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		state := &exchangeState{
			messages: isMessages(r),
			keep:     opts.KeepBodies,
			plan:     Plan{Backend: opts.Upstream, Route: "upstream", Reason: "passthrough"},
		}
		sw := &statusWriter{ResponseWriter: w}

		// By default an HTTP/1 server closes the request body the moment a
		// handler starts writing the response. A proxy starts writing as
		// soon as the backend answers, while its transport may still be
		// finishing the request body; the transport then sees a read error,
		// takes the request to have failed, and drops the backend
		// connection with the response half relayed. Full duplex tells the
		// server to leave the body alone. The error is ignored: it means
		// the connection is HTTP/2, which has no such restriction.
		http.NewResponseController(w).EnableFullDuplex()

		var (
			requestBody *cappedBuffer // a copy made as the body streams past
			original    []byte        // or the whole body, read up front
			truncated   bool
			refusal     *Refusal
			// inspectOnly marks a request that is read whole to be checked
			// for secrets but is not otherwise planned or summarised.
			inspectOnly bool
		)
		switch {
		case state.messages && r.Body != nil:
			// A Messages body is read whole before anything is sent, so
			// that it can be inspected and, if the plan says so, replaced.
			original, truncated = readUpTo(r.Body, maxKept)
			send := original
			if truncated {
				// Too large to inspect: send what was read followed by
				// the rest, untouched.
				r.Body = struct {
					io.Reader
					io.Closer
				}{io.MultiReader(bytes.NewReader(original), r.Body), r.Body}
				state.plan.Reason = "passthrough: request too large to inspect"
				send = nil
			}

			// The local-only policy is consulted first, because it outranks
			// everything else: an override header, the routing rules, and
			// any fallback they would set up.
			keptLocal := false
			if opts.Policy != nil {
				reasons, quoted, err := localOnly(opts.Policy, original, truncated)
				switch {
				case err != nil:
					// A request that cannot be checked is not sent.
					state.guard = &Guard{Blocked: err.Error()}
					refusal = &Refusal{Status: http.StatusInternalServerError, Type: "api_error",
						Message: fmt.Sprintf("janus: %v. The request was not sent.", err), Route: "none", Reason: "local-only policy: " + err.Error()}
				case len(reasons) > 0 && opts.PolicyAction == "local":
					state.guard = &Guard{LocalOnly: reasons}
					keptLocal = true
					req, _ := anthropic.ParseRequest(original) // localOnly has shown that this parses
					why := "local-only policy: " + strings.Join(reasons, "; ")
					if opts.LocalOnly == nil {
						refusal = &Refusal{Status: http.StatusForbidden, Type: "permission_error", Route: "local", Reason: why + ": refused: no local model to answer it",
							Message: "janus: this request holds content that must stay on this machine (" + strings.Join(reasons, "; ") + "), and there is no local model to answer it. It was not sent."}
					} else if plan, refused := opts.LocalOnly(r.Context(), r.Header, req); refused != nil {
						refused.Message = "janus: this request holds content that must stay on this machine (" + strings.Join(reasons, "; ") + "), and the local model cannot answer it: " +
							strings.TrimPrefix(refused.Message, "janus: ") + ". It was not sent."
						refused.Reason = why + ": refused: " + refused.Reason
						refusal = refused
					} else {
						plan.Reason = why
						state.plan = plan
						state.plan.Else, state.plan.Fallback, state.plan.Accept = nil, nil, nil // nothing may send it upstream
						if plan.Body != nil {
							send = plan.Body
						}
					}
				case len(reasons)+len(quoted) > 0:
					state.guard = &Guard{LocalOnly: append(reasons, quoted...)}
					state.withhold = true
				}
			}

			if opts.Plan != nil && !truncated && !keptLocal && refusal == nil {
				if req, err := anthropic.ParseRequest(original); err != nil {
					state.plan.Reason = "passthrough: request not understood"
				} else if plan, refused := opts.Plan(r.Context(), r.Header, req); refused != nil {
					refusal = refused
				} else {
					state.plan = plan
					if plan.Body != nil {
						send = plan.Body
					}
				}
			}
			if send != nil {
				r.Body = io.NopCloser(bytes.NewReader(send))
				r.ContentLength = int64(len(send))
			}
		case (opts.Redactor != nil || opts.Policy != nil) && isCountTokens(r) && r.Body != nil:
			// Token counting sends the same content upstream as the
			// request it is counting for, so it is checked the same way.
			// It can only be answered upstream, so local-only content in
			// it is always withheld, whatever the policy's action.
			original, truncated = readUpTo(r.Body, maxKept)
			inspectOnly = true
			if opts.Policy != nil {
				reasons, quoted, err := localOnly(opts.Policy, original, truncated)
				switch {
				case err != nil:
					state.guard = &Guard{Blocked: err.Error()}
					refusal = &Refusal{Status: http.StatusInternalServerError, Type: "api_error",
						Message: fmt.Sprintf("janus: %v. The request was not sent.", err), Route: "none", Reason: "local-only policy: " + err.Error()}
				case len(reasons)+len(quoted) > 0:
					state.guard = &Guard{LocalOnly: append(reasons, quoted...)}
					state.withhold = true
				}
			}
		case state.keep && r.Body != nil:
			requestBody = &cappedBuffer{limit: maxKept}
			r.Body = &teeBody{ReadCloser: r.Body, copy: requestBody}
		}

		// Whatever the plan holds is freed when the exchange ends at the
		// latest; a trial and a switch to a fallback free it sooner.
		state.hold(state.plan)
		defer state.releaseNow()

		base := context.WithValue(r.Context(), stateKey{}, state)
		withTimeout := func(plan Plan) (context.Context, context.CancelFunc) {
			if plan.Timeout > 0 {
				return context.WithTimeout(base, plan.Timeout)
			}
			return base, func() {}
		}
		// prepare gives what the plan sends: its own body if it has one,
		// and otherwise the request as the client sent it; with secrets
		// replaced if it is going upstream and a redactor is set. An error
		// means the request could not be made safe and must not be sent.
		prepare := func(plan Plan) ([]byte, error) {
			body := plan.Body
			if body == nil {
				body = original
			}
			state.redaction, state.sent = nil, plan.Body
			if plan.Backend.Name() != "upstream" {
				return body, nil
			}
			if state.withhold {
				// The body is checked again as it stands now, since
				// planning may have rewritten it since the first check.
				req, err := anthropic.ParseRequest(body)
				if err != nil {
					return nil, fmt.Errorf("the request could not be read to withhold its local-only content: %w", err)
				}
				state.guard.Withheld = opts.Policy.Withhold(req, opts.Policy.Check(req))
				if opts.Policy.Remaining(req) != 0 {
					return nil, errors.New("local-only content could not be removed from the request")
				}
				if state.guard.Withheld > 0 {
					body = req.Bytes()
					state.sent = body
				}
			}
			if opts.Redactor == nil {
				return body, nil
			}
			if state.guard == nil {
				state.guard = &Guard{}
			}
			if truncated {
				return nil, errors.New("the request is too large to check for secrets")
			}
			rd, err := opts.Redactor.Redact(body)
			if err != nil {
				return nil, err
			}
			state.redaction = rd
			state.guard.Scanned, state.guard.Report, state.guard.Replaced = true, rd.Report, rd.Replaced
			if rd.Active() {
				state.sent = rd.Body
			}
			return rd.Body, nil
		}
		// blocked answers the client when prepare fails. Nothing has been
		// sent anywhere.
		blocked := func(w http.ResponseWriter, err error) {
			if state.guard == nil {
				state.guard = &Guard{}
			}
			state.guard.Blocked = err.Error()
			state.plan.Reason = "guard: " + err.Error() + "; not sent"
			writeError(w, http.StatusInternalServerError, "api_error", fmt.Sprintf("janus: %v. The request was not sent upstream.", err))
		}

		if state.messages && !truncated && (state.plan.Else != nil || state.plan.Fallback != nil) {
			state.resolve = func() (Plan, bool) {
				if state.plan.Else != nil {
					return *state.plan.Else, true
				}
				next, ok := state.plan.Fallback(base)
				if !ok && next.Reason != "" {
					state.noFallback = next.Reason
				}
				return next, ok
			}
			// The plan may have a fallback. Record the attempt, switch to
			// the fallback, and serve that.
			state.fallBack = func(w http.ResponseWriter, why string, fault bool) bool {
				tried := state.plan
				state.fallBack = nil
				state.releaseNow() // the tried backend is done with the request
				next := state.next
				if next == nil {
					resolved, ok := state.resolve()
					if !ok {
						if state.noFallback != "" {
							state.plan.Reason = fmt.Sprintf("%s: %s; %s", tried.Reason, why, state.noFallback)
						}
						return false
					}
					next = &resolved
				}
				if state.attempt == nil {
					state.attempt = &Attempt{Backend: tried.Backend.Name(), Model: tried.BackendModel, Stages: tried.Stages,
						Why: why, Fault: fault, Duration: time.Since(start),
						ResponseHeader: state.failedHeader, ResponseBody: state.failedBody}
				}
				state.plan = *next
				state.hold(state.plan)
				// The fallback's reason starts with the same rule; what it
				// adds (older turns left out, a wait for the model) is kept.
				added := strings.TrimLeft(strings.TrimPrefix(next.Reason, tried.Reason), ";, ")
				state.plan.Reason = fmt.Sprintf("%s: %s; sent %s", tried.Reason, why, state.plan.Backend.Name())
				if added != "" {
					state.plan.Reason += ", " + added
				}

				body, err := prepare(state.plan)
				if err != nil {
					blocked(w, err)
					return true
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.ContentLength = int64(len(body))
				ctx, cancel := withTimeout(state.plan)
				defer cancel()
				rp.ServeHTTP(w, r.WithContext(ctx))
				return true
			}
		}

		ctx, cancel := withTimeout(state.plan)
		defer cancel()

		// Deferred because the reverse proxy aborts a response that fails
		// part-way by panicking; the exchange still has to be reported.
		defer func() {
			ex := Exchange{
				ID:             newID(start),
				Start:          start,
				Method:         r.Method,
				Path:           r.URL.Path,
				Header:         r.Header.Clone(),
				Status:         sw.status,
				Duration:       time.Since(start),
				Cancelled:      r.Context().Err() != nil,
				Backend:        state.plan.Backend.Name(),
				Route:          state.plan.Route,
				Reason:         state.plan.Reason,
				Stages:         state.plan.Stages,
				BackendModel:   state.plan.BackendModel,
				Transforms:     state.plan.Transforms,
				Shadow:         state.plan.Shadow,
				Attempt:        state.attempt,
				BackendFault:   state.backendFault,
				Unread:         state.unread,
				NoFallback:     state.noFallback,
				ResponseHeader: state.responseHeader,
			}
			if g := state.guard; g != nil {
				if rs := state.restorer; rs != nil {
					g.Restored = rs.Restored
					if rs.Err != nil {
						g.Blocked = rs.Err.Error()
					}
				}
				ex.Guard = g
				if g.Blocked != "" && sw.status == http.StatusInternalServerError {
					ex.Backend = "none"
				}
			}
			if got := state.served; got != nil {
				ex.TTFB, ex.ResponseHeader, ex.Summary = got.Duration, got.Header, &got.Summary
				if state.keep {
					ex.ResponseBody, ex.ResponseTruncated = got.Body, got.Truncated
				}
			}
			if refusal != nil {
				ex.Backend, ex.Route, ex.Reason = "none", refusal.Route, refusal.Reason
			}
			if !state.firstByte.IsZero() {
				ex.TTFB = state.firstByte.Sub(start)
			}
			// A read that failed because the client left is the client's
			// doing, already recorded as Cancelled.
			if state.fault != nil && !ex.Cancelled {
				ex.Fault = state.fault.Error()
			}
			if state.keep && state.sent != nil && sw.status != 0 {
				ex.SentBody = state.sent
			}
			if requestBody != nil {
				ex.RequestBody, ex.RequestTruncated = requestBody.snapshot()
			} else {
				ex.RequestBody, ex.RequestTruncated = original, truncated
			}
			if o := state.observation; o != nil {
				summary, unread := o.finish()
				switch {
				case unread != "":
					ex.Unread = unread
				case summary != nil:
					ex.Summary = summary
				}
				if o.kept != nil {
					ex.ResponseBody, ex.ResponseTruncated = o.kept.snapshot()
				}
			}
			if sw.own != nil && state.keep && ex.ResponseBody == nil {
				ex.ResponseBody = sw.own
				if ex.ResponseHeader == nil {
					ex.ResponseHeader = sw.Header().Clone()
				}
			}
			opts.Done(ex)
		}()

		if refusal != nil {
			writeError(sw, refusal.Status, refusal.Type, refusal.Message)
			return
		}

		var sending []byte
		if (state.messages || inspectOnly) && original != nil {
			var err error
			if sending, err = prepare(state.plan); err != nil {
				blocked(sw, err)
				return
			}
			if !truncated {
				r.Body = io.NopCloser(bytes.NewReader(sending))
				r.ContentLength = int64(len(sending))
			}
		}

		if state.fallBack != nil && state.plan.Accept != nil {
			// A trial: get the whole answer, judge it, and only then
			// decide what the client receives.
			tried := state.plan
			got := Fetch(ctx, client, tried.Backend, r.Header, sending, trialLimit)
			state.releaseNow() // the local model has given its whole answer
			if r.Context().Err() != nil {
				return // the client left while the local model was working
			}
			attempt := &Attempt{Backend: tried.Backend.Name(), Model: tried.BackendModel, Stages: tried.Stages, Duration: got.Duration}
			switch {
			case got.Err != nil && errors.Is(got.Err, context.DeadlineExceeded):
				attempt.Why, attempt.Fault = fmt.Sprintf("%s did not answer within %s", attempt.Backend, tried.Timeout), true
			case got.Err != nil:
				attempt.Why, attempt.Fault = "could not reach "+attempt.Backend+": "+got.Err.Error(), true
			case got.Status >= 500:
				attempt.Why, attempt.Fault = fmt.Sprintf("%s answered HTTP %d", attempt.Backend, got.Status), true
			case got.Truncated:
				attempt.Why = fmt.Sprintf("the %s answer was larger than %d bytes", attempt.Backend, trialLimit)
			case got.Fault != "":
				attempt.Why, attempt.Fault = "the "+attempt.Backend+" answer broke off: "+got.Fault, true
			default:
				attempt.Accepted, attempt.Why = tried.Accept(got)
			}
			state.attempt = attempt

			if attempt.Accepted {
				state.fallBack = nil
				state.plan.Reason = tried.Reason + ": local answer accepted"
				state.served = &got
				body := got.Body
				if tried.ResponseModel != "" {
					renamed, err := io.ReadAll(backend.RenameModel(io.NopCloser(bytes.NewReader(body)), got.Header.Get("Content-Type"), tried.ResponseModel))
					if err == nil {
						body = renamed
					}
				}
				if ct := got.Header.Get("Content-Type"); ct != "" {
					sw.Header().Set("Content-Type", ct)
				}
				sw.WriteHeader(http.StatusOK)
				sw.Write(body)
				return
			}
			if got.Err == nil {
				attempt.Summary, attempt.ResponseBody = &got.Summary, got.Body
			}
			state.fallBack(sw, attempt.Why, attempt.Fault)
			return
		}
		rp.ServeHTTP(sw, r.WithContext(ctx))
	})
}

// readUpTo reads at most limit bytes of r. truncated reports that there
// was more, which is left unread in r.
func readUpTo(r io.Reader, limit int) (data []byte, truncated bool) {
	data, _ = io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if len(data) > limit {
		return data, true
	}
	return data, false
}

// localOnly checks a request body against the policy and returns why it
// holds local-only content, if it does. An error means the body could not
// be checked, which the caller must treat as "do not send".
//
// reasons is why the request as a whole is local-only; quoted names
// protected files that tool results merely quote lines of, which are kept
// from the upstream without making the request local-only.
func localOnly(policy *guard.Policy, body []byte, truncated bool) (reasons, quoted []string, err error) {
	if truncated {
		return nil, nil, errors.New("the request is too large to check against the local-only policy")
	}
	req, err := anthropic.ParseRequest(body)
	if err != nil {
		return nil, nil, fmt.Errorf("the request could not be read to check it against the local-only policy: %w", err)
	}
	m := policy.Check(req)
	return m.Reasons, m.Quoted, nil
}

func isCountTokens(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/v1/messages/count_tokens"
}

func isEventStream(h http.Header) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(h.Get("Content-Type"))), "text/event-stream")
}

func isMessages(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/v1/messages"
}

// newID is a timestamp to the millisecond plus a random suffix: unique
// enough for one machine, and it sorts in the order requests began.
func newID(t time.Time) string {
	var suffix [3]byte
	rand.Read(suffix[:])
	return t.UTC().Format("20060102T150405.000") + "-" + hex.EncodeToString(suffix[:])
}

// observedBody passes a response body through unchanged, noting when its
// first byte arrives and showing a copy of each read to the observation.
type observedBody struct {
	io.ReadCloser
	state *exchangeState
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		if b.state.firstByte.IsZero() {
			b.state.firstByte = time.Now()
		}
		if b.state.observation != nil {
			b.state.observation.Write(p[:n])
		}
	}
	if err != nil && err != io.EOF {
		b.state.fault = err
	}
	return n, err
}

// teeBody passes a request body through unchanged while keeping a copy.
type teeBody struct {
	io.ReadCloser
	copy *cappedBuffer
}

func (b *teeBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.copy.Write(p[:n])
	}
	return n, err
}

// cappedBuffer keeps what is written to it up to a limit and counts the
// rest as truncated. It is locked because the transport reads a request
// body on its own goroutine, which may still be running when the handler
// reports the exchange.
type cappedBuffer struct {
	mu        sync.Mutex
	limit     int
	data      []byte
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - len(b.data); len(p) > room {
		b.data = append(b.data, p[:room]...)
		b.truncated = true
	} else {
		b.data = append(b.data, p...)
	}
	return len(p), nil
}

func (b *cappedBuffer) snapshot() (data []byte, truncated bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.data), b.truncated
}

// writeError answers in the Anthropic API's error shape, so a client built
// for that API reports janus's own failures the way it reports upstream
// ones.
func writeError(w http.ResponseWriter, status int, kind, message string) {
	body := errorBody(kind, message)
	if sw, ok := w.(*statusWriter); ok {
		sw.own = body
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

// errorBody is an error in the Anthropic API's shape.
func errorBody(kind, message string) []byte {
	body, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": kind, "message": message},
	})
	return append(body, '\n')
}

// statusWriter remembers the status code for the request log, and the
// body of any error janus wrote itself, which no backend sent and so no
// observation saw. Unwrap lets http.ResponseController reach the real
// writer, which is how the reverse proxy flushes.
type statusWriter struct {
	http.ResponseWriter
	status int
	own    []byte
}

func (w *statusWriter) WriteHeader(code int) {
	// An interim response, such as the 100 Continue a backend sends a
	// client that asked for one, comes before the answer and is not it
	// (#178). 101 Switching Protocols is final.
	interim := code >= 100 && code < 200 && code != http.StatusSwitchingProtocols
	if w.status == 0 && !interim {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
