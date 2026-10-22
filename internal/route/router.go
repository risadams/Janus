package route

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/openai"
	"github.com/risadams/janus/internal/proxy"
	"github.com/risadams/janus/internal/transform"
	"github.com/risadams/janus/internal/validate"
)

// LocalModel is a local model a request can be sent to.
type LocalModel struct {
	// ID is the model's id in LM Studio.
	ID string
	// ContextTokens is the largest prompt to send it. Zero means ask LM
	// Studio what context the model is loaded with.
	ContextTokens int
	// Timeout bounds one request. Zero means no limit.
	Timeout time.Duration
	// MaxConcurrent is how many requests a rule may have in flight to this
	// model at once; past it, a rule sends the request upstream. Zero means
	// no limit. Aliases and the local-only policy, which have nowhere else
	// to send a request, are not limited.
	MaxConcurrent int
}

// Routes a rule can choose.
const (
	RouteUpstream = "upstream"
	RouteLocal    = "local"
	// RouteShadow sends the request upstream as usual and asks for it to be
	// run on the local model afterwards, so the two answers can be compared
	// without the local one ever reaching the client.
	RouteShadow = "shadow"
	// RouteEscalate is local first with Claude as the fallback: the local
	// model's whole answer is checked before the client sees any of it, and
	// if it fails the checks the request goes upstream instead.
	RouteEscalate = "local-then-escalate"
)

// Rule sends the requests it matches somewhere. A request must meet every
// condition that is set; a rule with none set matches everything.
type Rule struct {
	Name string
	// Conditions.
	Class     Class // empty matches any class
	Model     string
	MaxTokens int   // zero means no limit
	Tools     *bool // nil matches either way
	// Outcome.
	Route string
	// Local is the model to use, for every route but upstream.
	Local LocalModel
	// SkipChecks names validators to leave out when judging a local answer
	// on the escalate route.
	SkipChecks []string
	// Fallback is where a matching request may go, in order. With Route
	// empty the list is the route: the request goes to the first provider
	// that can take it, and moves down the list when one cannot answer.
	// With Route upstream or shadow the list starts with upstream, and
	// the rest are where the request goes if Claude cannot answer:
	// unreachable, rate limited, overloaded, or failing. Empty means the
	// failure stands.
	Fallback []Provider
	// TrialTimeout is how long the local model gets to produce its whole
	// answer on the escalate route. It applies as well as the model's own
	// Timeout; whichever is shorter wins.
	TrialTimeout time.Duration
	// IfUsageAbove, when set, matches only once one of the subscription's
	// usage limits, by the API's name for its window ("5h", "7d"), is
	// used past the share given, from 0 to 1, as Claude last reported.
	IfUsageAbove map[string]float64
}

// RouteOpenAI is the OpenAI API, which answers in Anthropic's place
// (FR-11).
const RouteOpenAI = "openai"

// Provider is one entry of a rule's fallback list.
type Provider struct {
	// Name is RouteUpstream, RouteOpenAI, or the name of a local model.
	Name string
	// Local is the model, for a local entry.
	Local *LocalModel
}

func (p Provider) String() string { return p.Name }

// Facts is what a rule is matched against.
type Facts struct {
	Class  Class
	Model  string
	Tokens int // estimated
	Tools  bool
	// Usage is each usage limit's share used, as Claude last reported it;
	// a limit missing from it is not known.
	Usage map[string]float64
}

// FactsOf gathers the facts about a request.
func FactsOf(header http.Header, req *anthropic.Request) Facts {
	return Facts{
		Class:  ClassifyRequest(header, req),
		Model:  req.Model(),
		Tokens: EstimateTokens(req.Bytes()),
		Tools:  len(req.Tools()) > 0,
	}
}

// mismatch says why a rule does not match, or returns "" if it does.
//
// A safety check is left with Claude unless a rule names its class, so
// anyClass is set only when there is no Claude to leave it with.
func (rule Rule) mismatch(f Facts, anyClass bool) string {
	switch {
	case rule.Class != "" && rule.Class != f.Class:
		return fmt.Sprintf("class is %s, not %s", f.Class, rule.Class)
	case rule.Class == "" && f.Class == Safety && !anyClass:
		return "a safety check is only routed by a rule that names class safety"
	case rule.Model != "" && rule.Model != f.Model:
		return fmt.Sprintf("model is %s, not %s", f.Model, rule.Model)
	case rule.MaxTokens > 0 && f.Tokens > rule.MaxTokens:
		return fmt.Sprintf("about %d tokens, over the rule's %d", f.Tokens, rule.MaxTokens)
	case rule.Tools != nil && *rule.Tools != f.Tools:
		if f.Tools {
			return "the request offers tools"
		}
		return "the request offers no tools"
	}
	if len(rule.IfUsageAbove) > 0 {
		if why, _ := rule.usageMismatch(f.Usage); why != "" {
			return why
		}
	}
	return ""
}

func (rule Rule) label(index int) string {
	if rule.Name != "" {
		return "rule " + rule.Name
	}
	return fmt.Sprintf("rule %d", index+1)
}

// Router decides which backend answers a Messages request.
//
// Two things can send a request to the local model. A request whose model
// is an alias always goes local: that puts the choice in the client's
// hands, and has no upstream to fall back to. Otherwise the rules are
// tried in order and the first match decides; a request that matches none
// goes upstream untouched.
type Router struct {
	Upstream backend.Backend
	Local    *backend.Local
	// OpenAI is the OpenAI API, for rules that name it. Nil if it is not
	// configured.
	OpenAI *openai.Backend
	// NoUpstream is set when there is no Anthropic account behind janus
	// (upstream.off): Upstream is backend.Nowhere, nothing is planned for
	// it, and a request no rule sends elsewhere is refused (#376).
	NoUpstream bool
	// Barred, when set, says why a provider ("upstream", "openai", or a
	// local model's name) may not have a request, or "" if it may. An
	// entry of a fallback list it bars is passed over (#406).
	Barred func(provider string, req *anthropic.Request) string
	// Aliases maps a requested model name to a local model.
	Aliases map[string]LocalModel
	Rules   []Rule
	// Transforms rewrites requests going upstream, to send fewer tokens.
	// Nil sends them as they arrived.
	Transforms *transform.Pipeline

	health breaker
	busy   slots
	// down decides when Claude has been down long enough to move down a
	// fallback list (#150).
	down outage
	// openaiDown is the same for OpenAI: one per router, since OpenAI is
	// one service.
	openaiDown outage
	// resets is when each provider can be asked again, after a limit or
	// an outage (#378).
	resets resets
	// manual is the providers chosen by hand with janus use (#379).
	manual manual
	// limits is Claude's latest word on the usage limits, for rules with
	// IfUsageAbove (#240).
	limits headroom
}

// slots counts the requests in flight to each local model, so that rules
// keep within its MaxConcurrent. A local model runs requests side by side
// in one machine's memory: on 2026-10-13 two subagents at once took
// qwen/qwen3.8-27b past 31 GB wired on a 36 GB Mac within four minutes
// (#128).
type slots struct {
	mu       sync.Mutex
	inFlight map[string]int
	// freed is closed, and replaced, whenever a place is given back, to
	// wake whoever is waiting for one.
	freed chan struct{}
}

// take reserves a place for one request to local, and returns the
// function that gives it back, which is safe to call more than once. ok is
// false, and nothing is reserved, when the model is already at its limit.
func (s *slots) take(local LocalModel) (release func(), ok bool) {
	release, ok, _ = s.try(local)
	return release, ok
}

// wait is take for a request with nowhere else to go: a stand-in, whose
// only alternative is Claude's error. It waits for a place until ctx ends
// or limit passes (no limit if zero), and says how long it waited.
func (s *slots) wait(ctx context.Context, local LocalModel, limit time.Duration) (release func(), ok bool, waited time.Duration) {
	start := time.Now()
	var expired <-chan time.Time
	if limit > 0 {
		timer := time.NewTimer(limit)
		defer timer.Stop()
		expired = timer.C
	}
	for {
		release, ok, freed := s.try(local)
		if ok {
			return release, true, time.Since(start)
		}
		select {
		case <-freed:
		case <-ctx.Done():
			return nil, false, time.Since(start)
		case <-expired:
			return nil, false, time.Since(start)
		}
	}
}

// try is one attempt at a place. When there is none, freed is the channel
// that will be closed when one is given back.
func (s *slots) try(local LocalModel) (release func(), ok bool, freed <-chan struct{}) {
	if local.MaxConcurrent <= 0 {
		return func() {}, true, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight == nil {
		s.inFlight = map[string]int{}
	}
	if s.freed == nil {
		s.freed = make(chan struct{})
	}
	if s.inFlight[local.ID] >= local.MaxConcurrent {
		return nil, false, s.freed
	}
	s.inFlight[local.ID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.inFlight[local.ID]--
			close(s.freed)
			s.freed = make(chan struct{})
		})
	}, true, nil
}

// A breaker stops rules from sending requests to a local backend that is
// plainly down, so that each request does not have to find that out for
// itself. After threshold failures in a row it opens for cooldown; the
// first request after that is let through to see whether things are
// better.
//
// It counts availability only: the backend could not be reached, timed
// out, or returned a server error. An answer that was merely not good
// enough is the backend working.
type breaker struct {
	mu        sync.Mutex
	failures  int
	openUntil time.Time
	now       func() time.Time // time.Now, except in tests
}

const (
	breakerThreshold = 3
	breakerCooldown  = 30 * time.Second
)

func (b *breaker) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// open reports whether the backend should be skipped right now.
func (b *breaker) open() (skip bool, failures int, until time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.clock().Before(b.openUntil), b.failures, b.openUntil
}

func (b *breaker) record(ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ok {
		b.failures, b.openUntil = 0, time.Time{}
		return
	}
	b.failures++
	if b.failures >= breakerThreshold {
		b.openUntil = b.clock().Add(breakerCooldown)
	}
}

// Report tells the router how a finished exchange went, so that it can
// notice the local backend failing. Call it for every exchange.
func (r *Router) Report(ex proxy.Exchange) {
	if ex.Backend == "upstream" && ex.Status > 0 && ex.Status < 400 {
		r.down.recovered() // Claude answered
		r.resets.answered(RouteUpstream)
	}
	if ex.Path == "/v1/messages" {
		tried := make([]string, len(ex.Tried))
		for i, t := range ex.Tried {
			tried[i] = t.Backend
		}
		r.endManual(ex.Header.Get("X-Claude-Code-Session-Id"), ex.Reason, ex.Backend, tried, ex.Status == 0 || UpstreamDown(ex.Status))
	}
	if ex.Backend == RouteOpenAI && ex.Status > 0 && ex.Status < 400 {
		r.openaiDown.recovered()
		r.resets.answered(RouteOpenAI)
	}
	switch {
	case ex.Backend == "upstream":
		r.limits.observe(RouteUpstream, anthropic.QuotaFrom(ex.ResponseHeader))
	case ex.Attempt != nil && ex.Attempt.Backend == "upstream":
		r.limits.observe(RouteUpstream, anthropic.QuotaFrom(ex.Attempt.ResponseHeader))
	}
	switch {
	case ex.Backend == RouteOpenAI:
		r.limits.observe(RouteOpenAI, openai.QuotaFrom(ex.ResponseHeader, time.Now()))
	case ex.Attempt != nil && ex.Attempt.Backend == RouteOpenAI:
		r.limits.observe(RouteOpenAI, openai.QuotaFrom(ex.Attempt.ResponseHeader, time.Now()))
	}
	switch {
	case ex.Attempt != nil && ex.Attempt.Backend == "local":
		r.health.record(!ex.Attempt.Fault)
	case ex.Backend == "local":
		r.health.record(!ex.BackendFault && ex.Status < 500)
	}
}

// bytesPerToken converts a request's size to a token estimate. janus has no
// tokenizer, and the real ratio for Claude Code requests was measured
// between 2.75 and 4.1 bytes per token (docs/research/traffic.md). Three
// errs toward over-counting, which leaves the model room to answer.
const bytesPerToken = 3

// EstimateTokens is a deliberately high guess at a request's size.
func EstimateTokens(body []byte) int {
	return (len(body) + bytesPerToken - 1) / bytesPerToken
}

// Plan implements proxy.Planner.
func (r *Router) Plan(ctx context.Context, header http.Header, req *anthropic.Request) (proxy.Plan, *proxy.Refusal) {
	plan, refusal, _ := r.Explain(ctx, header, req)
	return plan, refusal
}

// Explain is Plan with its reasoning: one line per step considered, ending
// with the decision.
func (r *Router) Explain(ctx context.Context, header http.Header, req *anthropic.Request) (plan proxy.Plan, refusal *proxy.Refusal, trace []string) {
	upstreamPlan := func(req *anthropic.Request, reason string) (plan proxy.Plan, repaired bool, transformed []string) {
		return r.upstreamPlan(header, req, reason)
	}
	const repairNote = "removed the unsigned thinking a local answer left in the history, which the API would reject"
	upstream := func(reason string) proxy.Plan {
		plan, repaired, transformed := upstreamPlan(req, reason)
		if repaired {
			trace = append(trace, repairNote)
		}
		if len(transformed) > 0 {
			trace = append(trace, "rewritten to send fewer tokens, by "+strings.Join(transformed, ", "))
		}
		return plan
	}
	facts := FactsOf(header, req)
	facts.Usage = r.limits.used(time.Now())
	// The request as it arrived, kept for a fallback that is planned
	// later, after the request itself may have been repaired for upstream.
	arrived := bytes.Clone(req.Bytes())

	// A client can choose the route itself with a header, which beats the
	// rules. Forcing local is a promise that the request will not go
	// upstream, so if local cannot take it the request is refused.
	//
	// A choice made with janus use (#379) works as the header does, until
	// it is cleared or its provider cannot answer; then the request goes
	// by the rules, as if no choice had been made.
	openaiFirst := false
	choice, how := strings.TrimSpace(header.Get(backend.RouteHeader)), "override"
	session := header.Get("X-Claude-Code-Session-Id")
	if choice == "" {
		if chosen := r.manual.get(session); chosen != "" {
			choice, how = chosen, "janus use"
		}
	}
	manual := how == "janus use"
	if choice != "" && choice != RouteOpenAI {
		reason := how + " " + choice
		refuse := func(status int, kind, message string) (proxy.Plan, *proxy.Refusal, []string) {
			trace = append(trace, fmt.Sprintf("%s: refused: %s", reason, message))
			return proxy.Plan{}, &proxy.Refusal{Status: status, Type: kind, Message: "janus: " + message, Route: RouteLocal, Reason: reason + ": " + message}, trace
		}
		where, name, _ := strings.Cut(choice, "=")
		switch where {
		case RouteUpstream:
			if r.NoUpstream && manual {
				r.manual.clear(session)
				trace = append(trace, reason+": ended, since upstream is off")
				break
			}
			if r.NoUpstream {
				return refuse(http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("%s: upstream: upstream is off in janus's config, so there is no Claude to send it to", backend.RouteHeader))
			}
			trace = append(trace, reason+": upstream")
			return upstream(reason), nil, trace
		case RouteLocal:
			local, problem := r.named(name)
			if problem != "" && manual {
				r.manual.clear(session)
				trace = append(trace, fmt.Sprintf("%s: ended: %s", reason, problem))
				break
			}
			if problem != "" {
				return refuse(http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("%s: %s: %s", backend.RouteHeader, choice, problem))
			}
			plan, _, failure := r.local(ctx, req, local, facts.Model, reason, false)
			if failure != nil && manual {
				// The local model cannot answer: the choice holds no
				// longer, and the rules decide.
				r.manual.clear(session)
				trace = append(trace, fmt.Sprintf("%s: ended: %s", reason, failure.message))
				break
			}
			if failure != nil {
				return refuse(failure.status, failure.kind, failure.message)
			}
			trace = append(trace, fmt.Sprintf("%s: local, to %s", reason, local.ID))
			return plan, nil, trace
		default:
			return refuse(http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("%s: %q is not upstream, openai, local, or local=<name of an entry in local.models>", backend.RouteHeader, choice))
		}
	}

	// fallback gives a plan the rest of the rule's list, planned only if
	// the plan's backend turns out not to be able to answer.
	fallback := func(plan proxy.Plan, rule Rule, label string, from int) proxy.Plan {
		return r.fallBackAlong(plan, header, arrived, facts.Model, rule.Fallback, from, label)
	}

	// fromList plans the request for the first entry of a rule's list, from
	// index start, that can take it, and gives it the rest as its fallback.
	// passed are the entries already passed over, and why.
	fromList := func(rule Rule, label string, start int, passed []proxy.Step) (proxy.Plan, *proxy.Refusal, []string) {
		var notes []string
		var cause string
		for _, step := range passed {
			notes = append(notes, step.Why)
			cause = firstOf(cause, step.Cause)
		}
		for i := start; i < len(rule.Fallback); i++ {
			p := rule.Fallback[i]
			var plan proxy.Plan
			why := r.barred(p, req)
			switch {
			case why != nil:
			case p.Name == RouteUpstream:
				plan = upstream(label)
			default:
				plan, why = r.provider(ctx, header, req, p, facts.Model, label, i == len(rule.Fallback)-1)
			}
			if why != nil {
				notes = append(notes, why.message)
				cause = firstOf(cause, why.cause)
				passed = append(passed, why.step(p))
				trace = append(trace, fmt.Sprintf("%s: %s cannot take it: %s", label, p, why.message))
				if i == len(rule.Fallback)-1 {
					message := strings.Join(notes, "; ")
					return proxy.Plan{}, &proxy.Refusal{Status: why.status, Type: why.kind, Message: "janus: no provider on the list can take this request: " + message,
						Route: p.Name, Reason: label + ": refused: " + message}, trace
				}
				continue
			}
			if plan.LocalCause == "" {
				plan.LocalCause = cause
			}
			if len(notes) > 0 {
				plan.Reason = fmt.Sprintf("%s: %s; sent %s", label, strings.Join(notes, "; "), p)
			}
			plan.PassedOver = passed
			trace = append(trace, fmt.Sprintf("%s: %s%s", label, p, thenNote(rule.Fallback, i+1)))
			return fallback(plan, rule, label, i+1), nil, trace
		}
		trace = append(trace, label+": an empty list; upstream")
		return upstream(label), nil, trace
	}

	// An alias has no upstream model behind it, so there is nowhere to
	// fall back to: a request that cannot go local is refused, with why.
	if local, ok := r.Aliases[facts.Model]; ok {
		reason := "model alias " + facts.Model
		plan, _, problem := r.local(ctx, req, local, facts.Model, reason, false)
		if problem != nil {
			trace = append(trace, fmt.Sprintf("%s: refused: %s", reason, problem.message))
			return proxy.Plan{}, &proxy.Refusal{
				Status: problem.status, Type: problem.kind, Message: "janus: " + problem.message,
				Route: RouteLocal, Reason: reason + ": " + problem.message,
			}, trace
		}
		trace = append(trace, fmt.Sprintf("%s: local, to %s", reason, local.ID))
		return plan, nil, trace
	}

	// Claude Code's start-up quota check reads Claude's rate-limit
	// headers. With no Claude there are none to read, and asking a model
	// for its one token would only cost something, so janus answers it.
	if r.NoUpstream && facts.Class == Quota {
		trace = append(trace, "quota check: answered by janus, since upstream is off")
		return proxy.Plan{Backend: backend.Nowhere{}, Route: "janus", Reason: "quota check: answered by janus, since upstream is off"}, nil, trace
	}

	if choice == RouteOpenAI {
		openaiFirst = true
	}
	if openaiFirst && r.OpenAI != nil {
		// The session asked for OpenAI first (janus claude -provider
		// openai): OpenAI, then the rest of the list of the rule that
		// would have decided, so the chain still covers OpenAI failing.
		list := []Provider{{Name: RouteOpenAI}}
		label := how + " openai"
		for i, rule := range r.Rules {
			if rule.mismatch(facts, r.NoUpstream) != "" {
				continue
			}
			label = how + " openai, then " + rule.label(i)
			for _, p := range rule.Fallback {
				if p.Name != RouteOpenAI {
					list = append(list, p)
				}
			}
			break
		}
		trace = append(trace, label+": openai first")
		return fromList(Rule{Fallback: list}, label, 0, nil)
	} else if openaiFirst {
		trace = append(trace, "override openai: refused, since openai is not configured")
		return proxy.Plan{}, &proxy.Refusal{Status: http.StatusBadRequest, Type: "invalid_request_error",
			Message: "janus: " + backend.RouteHeader + ": openai: there is no openai section in janus's config", Route: "none",
			Reason: "override openai: refused: openai is not configured"}, trace
	}

	for i, rule := range r.Rules {
		label := rule.label(i)
		if why := rule.mismatch(facts, r.NoUpstream); why != "" {
			trace = append(trace, fmt.Sprintf("%s: no match (%s)", label, why))
			continue
		}
		// The decision says which limit made a usage rule match.
		if _, matched := rule.usageMismatch(facts.Usage); len(rule.IfUsageAbove) > 0 {
			label += " (" + matched + ")"
		}
		switch rule.Route {
		case RouteLocal, RouteEscalate:
			// The client asked for a real model, so whatever goes wrong
			// with the local one, the request goes to that model instead
			// of failing.
			if skip, failures, until := r.health.open(); skip {
				reason := fmt.Sprintf("%s: the local backend has failed %d times in a row and is being skipped until %s; sent upstream",
					label, failures, until.Format("15:04:05"))
				trace = append(trace, reason)
				plan := upstream(reason)
				plan.LocalCause = proxy.CauseSkipped
				return plan, nil, trace
			}
			release, ok := r.busy.take(rule.Local)
			if !ok {
				reason := fmt.Sprintf("%s: %s already has %d requests in flight, its limit; sent upstream",
					label, rule.Local.ID, rule.Local.MaxConcurrent)
				trace = append(trace, reason)
				plan := upstream(reason)
				plan.LocalCause = proxy.CauseBusy
				return plan, nil, trace
			}
			plan, adapted, problem := r.local(ctx, req, rule.Local, facts.Model, label, false)
			if problem != nil {
				release()
				if problem.status == http.StatusBadGateway {
					r.health.record(false) // LM Studio could not be reached
				}
				reason := fmt.Sprintf("%s: %s; sent upstream", label, problem.message)
				trace = append(trace, reason)
				plan := upstream(reason)
				plan.LocalCause = problem.cause
				return plan, nil, trace
			}
			plan.Release = release

			// The fallback is worked out only if the local attempt fails,
			// from the request as it arrived. An accepted local answer
			// then costs no transform work, and leaves the transforms'
			// record of what each conversation has sent upstream as it was.
			plan.Fallback = func(context.Context) (proxy.Plan, bool) {
				copy, err := anthropic.ParseRequest(arrived)
				if err != nil {
					return proxy.Plan{}, false
				}
				fallback, _, _ := upstreamPlan(copy, label)
				return fallback, true
			}
			if rule.Route == RouteEscalate {
				if rule.TrialTimeout > 0 && (plan.Timeout == 0 || rule.TrialTimeout < plan.Timeout) {
					plan.Timeout = rule.TrialTimeout
				}
				skip := rule.SkipChecks
				plan.Accept = func(got proxy.Fetched) (bool, string, string) {
					answer, readable := anthropic.Assemble(got.Body, got.Header.Get("Content-Type"))
					if check, why := validate.Check(adapted, got.Summary, answer, readable, skip); why != "" {
						return false, check, why
					}
					return true, "", ""
				}
				trace = append(trace, fmt.Sprintf("%s: local first, on %s; its answer is checked, and Claude answers instead if it fails", label, rule.Local.ID))
			} else {
				trace = append(trace, fmt.Sprintf("%s: local, to %s", label, rule.Local.ID))
			}
			return plan, nil, trace
		case RouteShadow, RouteUpstream:
			if len(rule.Fallback) > 0 {
				if why := r.barred(rule.Fallback[0], req); why != nil {
					// Claude may not have this request; the rest of the
					// list may.
					trace = append(trace, fmt.Sprintf("%s: upstream cannot take it: %s", label, why.message))
					return fromList(rule, label, 1, []proxy.Step{why.step(rule.Fallback[0])})
				}
			}
			plan := upstream(label)
			if rule.Route == RouteShadow {
				plan.Shadow = &proxy.Shadow{Model: rule.Local.ID, ContextTokens: rule.Local.ContextTokens, Timeout: rule.Local.Timeout, Reason: label}
				trace = append(trace, fmt.Sprintf("%s: upstream, and shadowed on %s%s", label, rule.Local.ID, thenNote(rule.Fallback, 1)))
			} else {
				trace = append(trace, label+": upstream"+thenNote(rule.Fallback, 1))
			}
			return fallback(plan, rule, label, 1), nil, trace
		default:
			// The list is the route: the first provider that can take the
			// request has it.
			return fromList(rule, label, 0, nil)
		}
	}

	if r.NoUpstream {
		message := `no routing rule sends this request anywhere, and upstream is off; add a rule with no conditions last, such as {"fallback": ["openai"]}`
		trace = append(trace, "no rule matched: refused, since upstream is off")
		return proxy.Plan{}, &proxy.Refusal{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: "janus: " + message, Route: "none", Reason: "no rule matched: " + message}, trace
	}
	trace = append(trace, "no rule matched: upstream")
	return upstream("passthrough"), nil, trace
}

// upstreamPlan is the plan that sends req upstream. Whatever sends a
// request upstream, a history that a local model or OpenAI had a turn in
// is repaired first, or the API would reject it. For a conversation that
// only ever went to Claude this finds nothing and the request goes exactly
// as it arrived. Then the transforms, if any, rewrite it to send fewer
// tokens.
func (r *Router) upstreamPlan(header http.Header, req *anthropic.Request, reason string) (plan proxy.Plan, repaired bool, transformed []string) {
	plan = proxy.Plan{Backend: r.Upstream, Route: RouteUpstream, Reason: reason}
	if backend.RepairForUpstream(req) {
		plan.Stages = []string{backend.StageDropThinking}
		repaired = true
	}
	plan.Transforms = r.Transforms.Apply(header, req)
	transformed = transform.Stages(plan.Transforms)
	plan.Stages = append(plan.Stages, transformed...)
	if len(plan.Stages) > 0 {
		plan.Body = req.Bytes()
	}
	return plan, repaired, transformed
}

// fallBackAlong gives plan the providers list[from:] as its fallback. Each
// is planned only when the one before it has failed, from the request as
// it arrived, and one that cannot take the request is passed over, saying
// why. Leaving Claude is decided by the #150 rules (outage.standIn), so
// that a brief failure is retried by Claude Code rather than moving the
// conversation.
func (r *Router) fallBackAlong(plan proxy.Plan, header http.Header, arrived []byte, model string, list []Provider, from int, label string) proxy.Plan {
	if from >= len(list) {
		return plan
	}
	if plan.Route == RouteUpstream {
		plan.FallBackOn = r.leaveClaude
	}
	plan.Fallback = func(ctx context.Context) (proxy.Plan, bool) {
		var notes []string
		var cause string
		var passed []proxy.Step
		for i := from; i < len(list); i++ {
			p := list[i]
			req, err := anthropic.ParseRequest(arrived)
			if err != nil {
				return proxy.Plan{Reason: "no fallback: the request could not be read again: " + err.Error(), LocalCause: proxy.CauseUnsupported}, false
			}
			var next proxy.Plan
			why := r.barred(p, req)
			switch {
			case why != nil:
			case p.Name == RouteUpstream:
				next, _, _ = r.upstreamPlan(header, req, label)
			default:
				next, why = r.provider(ctx, header, req, p, model, label, i == len(list)-1)
			}
			if why != nil {
				notes = append(notes, why.passedOver(p))
				cause = firstOf(cause, why.cause)
				passed = append(passed, why.step(p))
				continue
			}
			// The proxy puts the rule and the failure before the reason, so
			// the reason keeps only what it adds: a wait for the model,
			// older turns left out, the entries passed over.
			added := append(notes, strings.TrimLeft(strings.TrimPrefix(next.Reason, label), ";, "))
			next.Reason = strings.Trim(strings.Join(added, "; "), "; ")
			next.LocalCause = firstOf(next.LocalCause, cause)
			next.PassedOver = passed
			return r.fallBackAlong(next, header, arrived, model, list, i+1, label), true
		}
		return proxy.Plan{Reason: strings.Join(notes, "; "), LocalCause: cause, PassedOver: passed}, false
	}
	return plan
}

// provider plans req for one entry of a fallback list that is not
// upstream, or says why the entry cannot take it. last says that nothing
// comes after it on the list: a local model then waits its turn rather
// than giving way, since slower is better than an error, and has older
// turns left out to fit, since there is nowhere else to send them.
func (r *Router) provider(ctx context.Context, header http.Header, req *anthropic.Request, p Provider, model, reason string, last bool) (proxy.Plan, *problem) {
	switch {
	case p.Name == RouteOpenAI:
		if r.OpenAI == nil {
			return proxy.Plan{}, &problem{http.StatusInternalServerError, "api_error", "openai is not configured", ""}
		}
		if why := r.OpenAI.Out(); why != "" {
			return proxy.Plan{}, &problem{http.StatusServiceUnavailable, "api_error",
				"OpenAI is out until janus restarts, since " + strings.TrimPrefix(why, "janus: "), ""}
		}
		tr, err := r.OpenAI.Translation(header, req)
		if err != nil {
			return proxy.Plan{}, &problem{http.StatusBadRequest, "invalid_request_error", "the request cannot be translated for OpenAI: " + err.Error(), ""}
		}
		return proxy.Plan{
			Backend: r.OpenAI, Route: RouteOpenAI, Reason: reason,
			BackendModel: tr.Choice.Model, ResponseModel: model, Translate: tr.Translate, Report: tr.Report,
			FallBackOn: func(status int, header http.Header) bool { return r.leaveOpenAI(tr.Failure(), status, header) },
		}, nil
	case p.Local != nil:
		local := *p.Local
		if skip, failures, until := r.health.open(); skip {
			return proxy.Plan{}, &problem{http.StatusBadGateway, "api_error",
				fmt.Sprintf("the local backend has failed %d times in a row and is being skipped until %s", failures, until.Format("15:04:05")), proxy.CauseSkipped}
		}
		var release func()
		var ok bool
		var waited time.Duration
		if last {
			// It waits no longer than the model would be given to answer.
			release, ok, waited = r.busy.wait(ctx, local, local.Timeout)
		} else {
			release, ok = r.busy.take(local)
		}
		if !ok {
			return proxy.Plan{}, &problem{529, "overloaded_error",
				fmt.Sprintf("%s had %d requests in flight, its limit, for %s", local.ID, local.MaxConcurrent, waited.Round(100*time.Millisecond)), proxy.CauseBusy}
		}
		if waited >= time.Second {
			reason = fmt.Sprintf("%s, after waiting %s for the local model", reason, waited.Round(time.Second))
		}
		plan, _, why := r.local(ctx, req, local, model, reason, last)
		if why != nil {
			release()
			if why.status == http.StatusBadGateway {
				r.health.record(false) // LM Studio could not be reached
			}
			return proxy.Plan{}, why
		}
		plan.Release = release
		return plan, nil
	}
	return proxy.Plan{}, &problem{http.StatusInternalServerError, "api_error", fmt.Sprintf("%q is not upstream, openai, or a local model", p.Name), ""}
}

// leaveOpenAI is an OpenAI plan's FallBackOn: whether OpenAI's failure
// means the next on the list should have the request
// (docs/research/openai.md, Errors). A conversation too long for the
// model, and a request OpenAI refused, which most likely means janus
// translated it wrongly, move on at once, the next provider getting the
// request as it arrived. So do a refused key and a used-up quota, which
// take OpenAI out until a restart (Backend.Out). A per-minute rate limit,
// a server error, overload, and no answer at all are judged as Claude's
// failures are (#150): at once if Retry-After says a minute or more,
// otherwise on the third within two minutes, Claude Code retrying the
// ones before.
func (r *Router) leaveOpenAI(f *openai.Failure, status int, header http.Header) bool {
	if f != nil {
		switch f.Kind {
		case openai.TooLong, openai.BadRequest, openai.KeyRefused, openai.QuotaExhausted:
			return true
		}
	}
	down := r.openaiDown.standIn(status, header)
	switch wait := openaiWait(header); {
	case status == http.StatusTooManyRequests && wait >= longWait:
		r.resets.limit(RouteOpenAI, r.resets.clock().Add(wait), "OpenAI's rate limit was reached")
	case down:
		r.resets.outage(RouteOpenAI)
	}
	return down
}

// leaveClaude is an upstream plan's FallBackOn: the #150 rules, which
// also note when Claude can be asked again. A usage limit's reset time is
// kept, so that until then the list starts below Claude; an outage starts
// a backoff instead.
func (r *Router) leaveClaude(status int, header http.Header) bool {
	down := r.down.standIn(status, header)
	switch wait := retryWait(header, r.resets.clock()); {
	case status == http.StatusTooManyRequests && wait >= longWait:
		r.resets.limit(RouteUpstream, r.resets.clock().Add(wait), "Claude's usage limit was reached")
	case down:
		r.resets.outage(RouteUpstream)
	}
	return down
}

// barred says why p may not have req, or nil if it may: the check through
// which content can be allowed to reach some providers and not others
// (#406). Nil Barred allows every provider.
//
// It also passes over a provider waiting for a limit to reset, or out for
// an outage's backoff (#378).
func (r *Router) barred(p Provider, req *anthropic.Request) *problem {
	if why := r.resets.out(p.Name); why != "" {
		return &problem{http.StatusTooManyRequests, "rate_limit_error", providerName(p) + " is out: " + why, ""}
	}
	if r.Barred == nil {
		return nil
	}
	if why := r.Barred(p.Name, req); why != "" {
		return &problem{http.StatusForbidden, "permission_error", why, ""}
	}
	return nil
}

// providerName names p in a reason.
func providerName(p Provider) string {
	switch p.Name {
	case RouteUpstream:
		return "Claude"
	case RouteOpenAI:
		return "OpenAI"
	}
	return p.describe()
}

// thenNote says, for the trace, what comes after list[from-1]. from is at
// least 1.
func thenNote(list []Provider, from int) string {
	if from >= len(list) {
		return ""
	}
	names := make([]string, 0, len(list)-from)
	for _, p := range list[from:] {
		names = append(names, p.describe())
	}
	who := "it"
	if list[from-1].Name == RouteUpstream {
		who = "Claude"
	}
	return ", or " + strings.Join(names, ", then ") + " if " + who + " cannot answer"
}

// describe names a provider for people: a local model by its id.
func (p Provider) describe() string {
	if p.Local != nil {
		return p.Local.ID
	}
	return p.Name
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// UpstreamDown reports whether a status from the upstream means Claude
// could not answer, as opposed to the request being wrong. A rate limit
// counts: on a subscription it is what running out of usage looks like.
// It is what decides whether the rest of a fallback list is tried.
func UpstreamDown(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// LocalOnly returns a planner for requests that must not leave the
// machine: they go to the local model called name (or the only one
// configured), and nowhere else. Unlike a rule it does not give way when
// the local backend is down or the request does not fit; it refuses.
func (r *Router) LocalOnly(name string) proxy.Planner {
	return func(ctx context.Context, header http.Header, req *anthropic.Request) (proxy.Plan, *proxy.Refusal) {
		refuse := func(status int, kind, message string) (proxy.Plan, *proxy.Refusal) {
			return proxy.Plan{}, &proxy.Refusal{Status: status, Type: kind, Message: "janus: " + message, Route: RouteLocal, Reason: message}
		}
		local, problem := r.named(name)
		if problem != "" {
			return refuse(http.StatusForbidden, "permission_error", problem)
		}
		plan, _, why := r.local(ctx, req, local, req.Model(), "local-only policy", false)
		if why != nil {
			return refuse(why.status, why.kind, why.message)
		}
		return plan, nil
	}
}

// named resolves the local model an override asks for: the one called
// name, or the only one configured if no name is given.
func (r *Router) named(name string) (LocalModel, string) {
	if name != "" {
		if local, ok := r.Aliases[name]; ok {
			return local, ""
		}
		return LocalModel{}, fmt.Sprintf("%q is not an entry of local.models", name)
	}
	switch len(r.Aliases) {
	case 0:
		return LocalModel{}, "no local model is configured"
	case 1:
		for _, local := range r.Aliases {
			return local, ""
		}
	}
	return LocalModel{}, "several local models are configured; name one, as local=<name>"
}

// DefaultLoadContextTokens is the context Janus has LM Studio load a local
// model with, when the model is not loaded and its entry sets no
// contextTokens (#170). A fresh Claude Code turn is 25-35k tokens of system
// prompt and tool definitions before any work (2026-10-13 captures), and a
// session grows from there; a context fitted to the first turn would need
// reloading within a few turns, losing the model's cache. On 2026-10-13
// qwen/qwen3.8-27b ran at a 107k context on the owner's 36 GB Mac.
const DefaultLoadContextTokens = 65536

// StageLeaveOut records that older turns were left out of a conversation
// to fit the local model's context.
const StageLeaveOut = "leave_out_messages"

// loadContext is the context Janus has LM Studio load local with:
// contextTokens, or DefaultLoadContextTokens, within the model's maximum.
func loadContext(local LocalModel, info backend.Model) int {
	load := local.ContextTokens
	if load == 0 {
		load = DefaultLoadContextTokens
	}
	if info.MaxContext > 0 && info.MaxContext < load {
		load = info.MaxContext
	}
	return load
}

// problem is why a request cannot go to the local model.
type problem struct {
	status  int
	kind    string
	message string
	// cause is why, as one of the proxy's Cause names.
	cause string
}

// passedOver says why p was passed over on a fallback list.
func (pr *problem) passedOver(p Provider) string {
	if p.Local != nil {
		return "no local stand-in: " + pr.message
	}
	return p.Name + " cannot take it: " + pr.message
}

// step records that p was passed over for pr, for the ledger.
func (pr *problem) step(p Provider) proxy.Step {
	step := proxy.Step{Backend: p.Name, Why: pr.message, Cause: pr.cause}
	if p.Local != nil {
		step.Backend, step.Model = "local", p.Local.ID
	}
	return step
}

// local builds the plan for sending req to a local model. The response
// reports reportAs as its model, which is the name the client asked for.
//
// It works on a copy of the request, which it also returns, so that a
// request which turns out not to fit can still go upstream exactly as it
// arrived. With fit, a request too large for the model's context has its
// older turns left out until it fits (fitConversation); that is for a
// stand-in, which has nowhere else to go.
func (r *Router) local(ctx context.Context, req *anthropic.Request, local LocalModel, reportAs, reason string, fit bool) (proxy.Plan, *anthropic.Request, *problem) {
	info, found, err := r.Local.Model(ctx, local.ID)
	switch {
	case err != nil:
		return proxy.Plan{}, nil, &problem{http.StatusBadGateway, "api_error",
			fmt.Sprintf("could not reach LM Studio at %s: %v", r.Local.URL(), err), proxy.CauseUnreachable}
	case !found:
		// LM Studio would not reject this; it would answer with whatever
		// model happens to be loaded.
		return proxy.Plan{}, nil, &problem{http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("LM Studio does not have local model %q", local.ID), proxy.CauseModelMissing}
	}

	adapted, err := anthropic.ParseRequest(req.Bytes())
	if err != nil {
		return proxy.Plan{}, nil, &problem{http.StatusBadRequest, "invalid_request_error", "cannot read this request: " + err.Error(), proxy.CauseUnsupported}
	}
	stages, err := backend.Normalize(adapted, local.ID)
	if err != nil {
		return proxy.Plan{}, nil, &problem{http.StatusBadRequest, "invalid_request_error", "cannot adapt this request for the local model: " + err.Error(), proxy.CauseUnsupported}
	}
	body := adapted.Bytes()
	estimate := EstimateTokens(body)

	limit := local.ContextTokens
	if info.Loaded && limit == 0 {
		limit = info.LoadedContext
	}
	if fit {
		// The context the request has to fit: the model's, or the one
		// Janus would load it with.
		context := limit
		if !info.Loaded {
			context = loadContext(local, info)
		}
		if context > 0 && estimate > context {
			total := len(adapted.Messages())
			budget := context - answerReserve(context)
			leftOut, ok := fitConversation(adapted, budget)
			if !ok {
				return proxy.Plan{}, nil, &problem{http.StatusBadRequest, "invalid_request_error",
					fmt.Sprintf("this request is about %d tokens, and even its first and latest turns are more than the %d tokens local model %q has room for", estimate, budget, local.ID), proxy.CauseTooLarge}
			}
			body = adapted.Bytes()
			estimate = EstimateTokens(body)
			stages = append(stages, StageLeaveOut)
			reason = fmt.Sprintf("%s; %d of %d messages left out to fit the local model's context", reason, leftOut, total)
		}
	}
	if !info.Loaded {
		// Left to itself, LM Studio would load the model for this request
		// at a context Janus cannot see, and a request too large for it
		// would cost the load (most of this machine's memory, for a 27B
		// model) and then fail. So Janus loads it, at a context it chose.
		load := loadContext(local, info)
		if estimate > load {
			return proxy.Plan{}, nil, &problem{http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("this request is about %d tokens, more than the %d-token context Janus loads local model %q with (set contextTokens to change that)", estimate, load, local.ID), proxy.CauseTooLarge}
		}
		loaded, err := r.Local.Load(ctx, local.ID, load)
		if err != nil {
			return proxy.Plan{}, nil, &problem{http.StatusBadGateway, "api_error",
				fmt.Sprintf("LM Studio could not load local model %q with a %d-token context: %v", local.ID, load, err), proxy.CauseLoadFailed}
		}
		if limit == 0 || loaded.LoadedContext < limit {
			limit = loaded.LoadedContext
		}
		stages = append(stages, backend.StageLoad)
	}

	if limit > 0 && estimate > limit {
		return proxy.Plan{}, nil, &problem{http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("this request is about %d tokens, more than the %d-token context of local model %q", estimate, limit, local.ID), proxy.CauseTooLarge}
	}

	return proxy.Plan{
		Backend:       r.Local,
		Route:         RouteLocal,
		Reason:        reason,
		Stages:        stages,
		Body:          body,
		BackendModel:  local.ID,
		ResponseModel: reportAs,
		Timeout:       local.Timeout,
	}, adapted, nil
}
