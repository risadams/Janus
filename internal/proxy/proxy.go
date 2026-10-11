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
	"sync"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
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
	// Shadow is set when the request should also be run on a local model
	// for comparison. The proxy only carries it; whoever receives the
	// Exchange does the running.
	Shadow *Shadow
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
	// Fault is what went wrong reading the response from the backend, when
	// it broke off part-way: the connection dropped, or the backend sent a
	// body that did not match its own framing. Empty for a response that
	// arrived whole, and for one the client abandoned.
	Fault string

	// RequestBody is kept for Messages requests always, and for everything
	// else only when Options.KeepBodies is set. ResponseBody is kept only
	// with KeepBodies; it is decompressed where janus can do that, and
	// otherwise the bytes as they arrived. Either may be cut short at
	// maxKept, which the Truncated flags report.
	RequestBody       []byte
	RequestTruncated  bool
	ResponseHeader    http.Header
	ResponseBody      []byte
	ResponseTruncated bool
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
	// for a rejected trial; for an accepted one the Exchange itself
	// carries them.
	Summary      *anthropic.Summary
	ResponseBody []byte
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
	// Accept, when set, makes this plan a trial. The backend's whole
	// answer is held back and shown to Accept; the client gets it only if
	// Accept passes it, and otherwise gets Else's answer, never knowing
	// there were two. A trial needs Else.
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
	// served is an answer given to the client from memory, by a trial
	// that passed, rather than relayed.
	served *Fetched
	// fallBack switches to the plan's Else and serves it. It is set by the
	// handler for plans that have one, and cleared once used.
	fallBack func(w http.ResponseWriter, why string, fault bool)
}

// errBackendStatus turns an error status from a backend with a fallback
// into a failure, so that the fallback is used.
type errBackendStatus int

func (e errBackendStatus) Error() string { return fmt.Sprintf("it answered HTTP %d", int(e)) }

// trialLimit bounds the answer a trial will hold.
const trialLimit = 8 << 20

type stateKey struct{}

// New returns a handler that forwards every request to the upstream and
// relays the response as it arrives. Nothing in either direction is altered
// beyond what HTTP requires of a proxy (hop-by-hop headers are dropped and
// Host is set to the upstream's), apart from what the chosen backend's
// Prepare does. For the upstream that is one header on Messages requests;
// credentials pass through as the client sent them.
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
			if state.fallBack != nil && resp.StatusCode >= 400 {
				return errBackendStatus(resp.StatusCode)
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
				fault = int(bad) >= 500
			}
			if state.fallBack != nil {
				// Nothing has been written yet: the error handler runs
				// before any of the response is relayed.
				state.fallBack(w, why, fault)
				return
			}
			state.backendFault = fault
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
		)
		switch {
		case state.messages && r.Body != nil:
			// A Messages body is read whole before anything is sent, so
			// that it can be inspected and, if the plan says so, replaced.
			original, truncated = readUpTo(r.Body, maxKept)
			send := original
			switch {
			case truncated:
				// Too large to inspect: send what was read followed by
				// the rest, untouched.
				r.Body = struct {
					io.Reader
					io.Closer
				}{io.MultiReader(bytes.NewReader(original), r.Body), r.Body}
				state.plan.Reason = "passthrough: request too large to inspect"
				send = nil
			case opts.Plan != nil:
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
		case state.keep && r.Body != nil:
			requestBody = &cappedBuffer{limit: maxKept}
			r.Body = &teeBody{ReadCloser: r.Body, copy: requestBody}
		}

		base := context.WithValue(r.Context(), stateKey{}, state)
		withTimeout := func(plan Plan) (context.Context, context.CancelFunc) {
			if plan.Timeout > 0 {
				return context.WithTimeout(base, plan.Timeout)
			}
			return base, func() {}
		}
		// sendBody is what the current plan sends: its own body if it has
		// one, and otherwise the request exactly as the client sent it.
		sendBody := func(plan Plan) []byte {
			if plan.Body != nil {
				return plan.Body
			}
			return original
		}

		if state.messages && !truncated && state.plan.Else != nil {
			// The plan has a fallback. Record the attempt, switch to the
			// fallback, and serve that.
			state.fallBack = func(w http.ResponseWriter, why string, fault bool) {
				tried := state.plan
				state.fallBack = nil
				if state.attempt == nil {
					state.attempt = &Attempt{Backend: tried.Backend.Name(), Model: tried.BackendModel, Stages: tried.Stages,
						Why: why, Fault: fault, Duration: time.Since(start)}
				}
				state.plan = *tried.Else
				state.plan.Reason = fmt.Sprintf("%s: %s; sent %s", tried.Reason, why, state.plan.Backend.Name())

				body := sendBody(state.plan)
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.ContentLength = int64(len(body))
				ctx, cancel := withTimeout(state.plan)
				defer cancel()
				rp.ServeHTTP(w, r.WithContext(ctx))
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
				Shadow:         state.plan.Shadow,
				Attempt:        state.attempt,
				BackendFault:   state.backendFault,
				Unread:         state.unread,
				ResponseHeader: state.responseHeader,
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
			opts.Done(ex)
		}()

		if refusal != nil {
			writeError(sw, refusal.Status, refusal.Type, refusal.Message)
			return
		}

		if state.fallBack != nil && state.plan.Accept != nil {
			// A trial: get the whole answer, judge it, and only then
			// decide what the client receives.
			tried := state.plan
			got := Fetch(ctx, client, tried.Backend, r.Header, sendBody(tried), trialLimit)
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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": kind, "message": message},
	})
}

// statusWriter remembers the status code for the request log. Unwrap lets
// http.ResponseController reach the real writer, which is how the reverse
// proxy flushes.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
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
